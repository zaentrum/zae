package setup

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"unicode"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/term"
)

// The TMDB key — TMDB's API read access token, which the catalog manager signs
// in to TMDB with — reaches zae two ways, and neither is the command line:
//
//	--tmdb-key-file FILE  the file's content, surrounding whitespace trimmed
//	--tmdb-key-stdin      a line typed without echo on a terminal, or
//	                      everything piped in
//
// A key given as an argument would be kept in shell history and shown in the
// process list, so there is no flag that takes one, and an argument that might
// be one is refused without being repeated. Nothing zae prints quotes the key:
// not the summary before the question, not an error — not even the path a
// failing --tmdb-key-file names, which could be the key typed in the wrong
// place — and not the portal's own words, from which it is cut out should
// they ever carry it.

const (
	// maxKeyRead bounds what is read as a key; maxKey is the longest the
	// portal takes. A read access token is a few hundred bytes.
	maxKeyRead = 64 << 10
	maxKey     = 4096
)

// v3Key is TMDB's other credential, the API key (v3): 32 hex digits. The
// catalog manager sends the token as a bearer, which TMDB refuses for this
// one — and enrichment would then fail without a word — so it is refused here,
// as the console refuses it.
var v3Key = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// quoted is a value Go's flag package quotes in an error, which may be a key
// given where a flag takes none.
var quoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// errInterrupted is a key prompt ended by Ctrl-C or SIGTERM, with echo back on.
type errInterrupted struct{ sig os.Signal }

func (e errInterrupted) Error() string { return "interrupted" }

// readHidden asks for one line on the terminal with echo turned off, and turns
// it back on however the line ends — Ctrl-C included, which would otherwise
// leave the terminal blind. Tests substitute it.
var readHidden = func(prompt string) (string, error) {
	restore, err := term.WithoutEcho(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("cannot turn off echo on this terminal (%v) — give the key with --tmdb-key-file", err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	fmt.Fprint(stderr, prompt)
	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := lineReader().ReadString('\n')
		got <- result{line, err}
	}()
	select {
	case r := <-got:
		restore()
		fmt.Fprintln(stderr)
		if r.err != nil && r.line == "" {
			return "", fmt.Errorf("nothing was typed (%v)", r.err)
		}
		return strings.TrimRight(r.line, "\r\n"), nil
	case s := <-sig:
		restore()
		fmt.Fprintln(stderr)
		return "", errInterrupted{s}
	}
}

// metadata sets the catalog's TMDB key: read, checked, shown as what changes —
// never as itself — asked, and posted.
func metadata(args []string) int {
	fs := flag.NewFlagSet("zae setup metadata", flag.ContinueOnError)
	// The flag package's own errors quote what they could not read, and what
	// was given where a flag takes no value may be the key: they are worded
	// here instead.
	fs.SetOutput(io.Discard)
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	file := fs.String("tmdb-key-file", "", "a file that holds TMDB's API read access token (v4)")
	fromStdin := fs.Bool("tmdb-key-stdin", false, "read the token from stdin: typed without echo on a terminal, else piped in")
	yes := fs.Bool("yes", false, "set it without asking")
	var pos []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fs.SetOutput(stderr)
				fs.Usage()
				return exitcode.OK
			}
			return usageErr("zae setup metadata: %s — the key is read from a file (--tmdb-key-file) or stdin (--tmdb-key-stdin), never from the command line",
				quoted.ReplaceAllString(err.Error(), "(a value, not shown)"))
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(pos) != 0 {
		// Not repeated: an argument here is most likely the key itself.
		return usageErr("zae setup metadata takes no arguments — the key is read from a file (--tmdb-key-file FILE) or stdin (--tmdb-key-stdin), never from the command line, where it would land in shell history")
	}
	base, code := baseOf(*rawURL)
	if code != exitcode.OK {
		return code
	}
	set := setFlags(fs)
	switch {
	case set["tmdb-key-file"] && *fromStdin:
		return usageErr("--tmdb-key-file and --tmdb-key-stdin both name where the key comes from — give one")
	case !set["tmdb-key-file"] && !*fromStdin:
		return usageErr("zae setup metadata reads the key from a file (--tmdb-key-file FILE) or stdin (--tmdb-key-stdin)")
	case set["tmdb-key-file"] && strings.TrimSpace(*file) == "":
		return usageErr("--tmdb-key-file names the file that holds the key")
	case *fromStdin && !*yes && !canAsk():
		return usageErr("stdin carries the key, and the question before setting it needs stdin too — add --yes")
	}
	if code := requireTTY(*yes, "setting the TMDB key"); code != exitcode.OK {
		return code
	}

	// The key first, and checked: a key that cannot be one is usage, and
	// nothing reaches the instance.
	key, from, err := readKey(*file, *fromStdin)
	var intr errInterrupted
	switch {
	case errors.As(err, &intr):
		if s, ok := intr.sig.(syscall.Signal); ok {
			return 128 + int(s)
		}
		return 130
	case err != nil:
		return usageErr("%v", err)
	}
	if problem := keyProblem(key); problem != "" {
		return usageErr("%s", problem)
	}

	ctx := context.Background()
	c := newClient(base)
	d, _, err := read(ctx, c)
	if err != nil {
		return failWithout(err, key)
	}
	m := d.Metadata
	if m.State == stateUnknown && strings.HasPrefix(m.Note, noCatalog) {
		errf("not offered: %s has no catalog manager to set the key in: %s", base, noteOr(m.Note))
		return exitcode.NotOffered
	}
	fmt.Fprintf(stdout, "%s — the catalog's TMDB key\n", base)
	row(stdout, "now", currentKey(m))
	row(stdout, "sets", "the key "+from+", to the catalog manager's setting — it is never shown again")
	if !*yes && !confirm(fmt.Sprintf("set the TMDB key on %s?", base)) {
		fmt.Fprintln(stdout, "nothing changed")
		return exitcode.Failed
	}
	var after Doc
	if err := c.do(ctx, "set the TMDB key", http.MethodPost, metadataPath, map[string]string{"tmdbKey": key}, &after); err != nil {
		return failWithout(err, key)
	}
	fmt.Fprintf(stdout, "set the TMDB key on %s\n", base)
	head, _ := metadataStep(after.Metadata, base, now())
	row(stdout, "metadata", strings.ReplaceAll(head, key, "[the key]"))
	return exitcode.OK
}

// currentKey says what the key the write replaces is.
func currentKey(m Metadata) string {
	switch {
	case m.State == stateUnknown:
		return "cannot tell: " + noteOr(m.Note)
	case m.Key == "setting":
		return keySet(m, now()) + " — this replaces it"
	case m.Key == "environment":
		return "the catalog manager's own key — one set here takes its place"
	}
	return "none — titles keep their file names, and get no posters or plots"
}

// readKey reads the key from the file --tmdb-key-file names, or from stdin: a
// line typed without echo on a terminal, else everything piped in. A UTF-8
// byte order mark and surrounding whitespace are trimmed — an editor's, a
// trailing newline — as the portal trims them. No error quotes what was read,
// nor the path: a key typed where the path goes would be repeated by it.
func readKey(file string, fromStdin bool) (key, from string, err error) {
	var b []byte
	switch {
	case fromStdin && canAsk():
		line, err := readHidden("TMDB API read access token (not shown): ")
		if err != nil {
			var intr errInterrupted
			if errors.As(err, &intr) {
				return "", "", err
			}
			return "", "", fmt.Errorf("--tmdb-key-stdin: %v", err)
		}
		b, from = []byte(line), "typed on the terminal"
	case fromStdin:
		if b, err = io.ReadAll(io.LimitReader(stdin, maxKeyRead+1)); err != nil {
			return "", "", fmt.Errorf("--tmdb-key-stdin: cannot read stdin (%v)", cause(err))
		}
		from = "read from stdin"
	default:
		f, err := os.Open(file)
		if err != nil {
			return "", "", fmt.Errorf("--tmdb-key-file: cannot open the file it names (%v) — it takes the path of a file that holds the key, never the key itself", cause(err))
		}
		defer f.Close()
		if b, err = io.ReadAll(io.LimitReader(f, maxKeyRead+1)); err != nil {
			return "", "", fmt.Errorf("--tmdb-key-file: cannot read the file it names (%v)", cause(err))
		}
		from = "read from the file --tmdb-key-file names"
	}
	if len(b) > maxKeyRead {
		return "", "", fmt.Errorf("the key %s is longer than any TMDB token", from)
	}
	return strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff")), from, nil
}

// cause is an error without the path it names.
func cause(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// keyProblem is why what was read cannot be the token the catalog manager
// signs in to TMDB with, or "". The portal refuses the first three as well;
// the last it would take, and enrichment would then fail without a word.
func keyProblem(key string) string {
	switch {
	case key == "":
		return "the key is empty — there is no TMDB token in what was read"
	case len(key) > maxKey:
		return "the key is longer than any TMDB token"
	case strings.ContainsFunc(key, unicode.IsSpace):
		return "the key holds spaces or line breaks, which no TMDB token does — the file should hold the token alone"
	case strings.ContainsFunc(key, unicode.IsControl):
		return "the key holds control characters, which no TMDB token does"
	case v3Key.MatchString(key):
		return "that is TMDB's API key (v3); the catalog needs the API read access token (v4) from the same page of your TMDB account's API settings — it starts with eyJ"
	}
	return ""
}

// failWithout is fail, with the key cut out of what is printed: the portal
// never answers with it, and zae does not rely on that.
func failWithout(err error, key string) int {
	if ae, ok := asAPIError(err); ok && key != "" {
		cut := *ae
		cut.msg = strings.ReplaceAll(cut.msg, key, "[the key]")
		return fail(&cut)
	}
	return fail(err)
}
