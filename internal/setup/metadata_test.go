package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// tmdbKey is a key that must never reach a terminal. Distinctive on purpose:
// a substring search for it is the whole test.
const tmdbKey = "eyJhbGciOiJIUzI1NiJ9.eyJhdWQiOiJ0bWRiIn0.c2lnbmVk-must-never-be-printed"

func keyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmdb.key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// noKey is the rule this command rests on: the key never reaches a terminal,
// a scrollback buffer or a CI log.
func noKey(t *testing.T, where string, streams ...string) {
	t.Helper()
	for _, s := range streams {
		if strings.Contains(s, tmdbKey) || strings.Contains(s, "must-never-be-printed") {
			t.Fatalf("%s: the key reached the terminal:\n%s", where, s)
		}
	}
}

// sent is the key the portal was sent in its one POST, as JSON decodes it.
func (p *fakePortal) sent(t *testing.T) string {
	t.Helper()
	bodies := p.bodies["POST "+metadataPath]
	if len(bodies) != 1 {
		t.Fatalf("want one POST %s, got %d: %v", metadataPath, len(bodies), p.calls)
	}
	var b map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &b); err != nil || len(b) != 1 {
		t.Fatalf("the body is {\"tmdbKey\": …} alone: %v %q", err, bodies[0])
	}
	k, _ := b["tmdbKey"].(string)
	return k
}

// From a file: what changes is shown — never the key — asked, and the key is
// sent as the file holds it, the newline an editor leaves trimmed.
func TestMetadataSetsTheKeyFromAFile(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	file := keyFile(t, tmdbKey+"\n")
	code, out, errs := runWith(t, "y\n", true, "metadata", "--url", srv.URL, "--tmdb-key-file", file)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	noKey(t, "metadata", out, errs)
	if got := p.sent(t); got != tmdbKey {
		t.Fatalf("the key sent is the file's, trimmed: %q", got)
	}
	for _, s := range []string{
		srv.URL + " — the catalog's TMDB key",
		"  now          none — titles keep their file names, and get no posters or plots",
		"  sets         the key read from the file --tmdb-key-file names, to the catalog manager's setting — it is never shown again",
		"set the TMDB key on " + srv.URL + "? [y/N] ",
		"set the TMDB key on " + srv.URL + "\n",
		"  metadata     done — a TMDB key is set, saved just now",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("metadata lacks %q:\n%s", s, out)
		}
	}
	if strings.Index(out, "  sets ") > strings.Index(out, "[y/N]") {
		t.Errorf("what changes is printed before the question:\n%s", out)
	}
	// The file names the path in nothing printed: a key typed where the
	// path goes would be repeated by it.
	if strings.Contains(out+errs, file) {
		t.Errorf("the path was printed:\n%s\n%s", out, errs)
	}

	// A key saved before is replaced, and the summary says so; a byte order
	// mark an editor wrote is no part of the key.
	p2, srv2 := newPortal(t, setUp())
	at := clock.Add(-26 * 60 * 60 * 1e9)
	p2.doc.Metadata = Metadata{Step: Step{State: stateDone}, Key: "setting", UpdatedAt: &at}
	code, out, errs = runWith(t, "", false, "metadata", "--url", srv2.URL, "--tmdb-key-file", keyFile(t, "\xef\xbb\xbf"+tmdbKey+"\r\n"), "--yes")
	if code != exitcode.OK || p2.sent(t) != tmdbKey {
		t.Fatalf("--yes: want 0 and the key without its mark, got %d %q\n%s\n%s", code, p2.key, out, errs)
	}
	if !strings.Contains(out, "  now          a TMDB key is set, saved 26 h ago — this replaces it") || strings.Contains(out, "[y/N]") {
		t.Errorf("the key it replaces, and no question with --yes:\n%s", out)
	}
	noKey(t, "metadata --yes", out, errs)
}

// Confirm before writing: a no writes nothing, and without a terminal zae will
// not ask — the invocation needs --yes, and fails as usage before anything is
// read or written.
func TestMetadataAsksBeforeWriting(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	file := keyFile(t, tmdbKey)
	code, out, errs := runWith(t, "n\n", true, "metadata", "--url", srv.URL, "--tmdb-key-file", file)
	if code != exitcode.Failed || !strings.Contains(out, "nothing changed") || p.called("POST "+metadataPath) != 0 {
		t.Fatalf("declined: want 1 and no write, got %d %v\n%s\n%s", code, p.calls, out, errs)
	}
	// An empty answer, and a closed stdin, are no too.
	for _, input := range []string{"\n", ""} {
		if code, _, _ := runWith(t, input, true, "metadata", "--url", srv.URL, "--tmdb-key-file", file); code != exitcode.Failed {
			t.Errorf("answer %q: want 1, got %d", input, code)
		}
	}
	if p.called("POST "+metadataPath) != 0 {
		t.Fatalf("an answer that is not yes wrote: %v", p.calls)
	}

	calls := len(p.calls)
	code, _, errs = runWith(t, "y\n", false, "metadata", "--url", srv.URL, "--tmdb-key-file", file)
	if code != exitcode.Usage || !strings.Contains(errs, "stdin is not a terminal") || len(p.calls) != calls {
		t.Fatalf("no terminal, no --yes: want 2 before any call, got %d %q %v", code, errs, p.calls[calls:])
	}
	noKey(t, "metadata", out, errs)
}

// From stdin: piped, it is everything piped in — and stdin cannot then carry
// the answer as well, so --yes is required. On a terminal, it is a line typed
// without echo, and the question follows on the same terminal.
func TestMetadataReadsTheKeyFromStdin(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	code, out, errs := runWith(t, "  "+tmdbKey+"\n", false, "metadata", "--url", srv.URL, "--tmdb-key-stdin", "--yes")
	if code != exitcode.OK || p.sent(t) != tmdbKey {
		t.Fatalf("piped with --yes: want 0 and the key, got %d %q\n%s\n%s", code, p.key, out, errs)
	}
	if !strings.Contains(out, "the key read from stdin") {
		t.Errorf("the summary says where the key came from:\n%s", out)
	}
	noKey(t, "metadata --tmdb-key-stdin", out, errs)

	p2, srv2 := newPortal(t, freshBox())
	code, _, errs = runWith(t, tmdbKey+"\n", false, "metadata", "--url", srv2.URL, "--tmdb-key-stdin")
	if code != exitcode.Usage || !strings.Contains(errs, "stdin carries the key") || len(p2.calls) != 0 {
		t.Fatalf("piped without --yes: want 2 before any call, got %d %q %v", code, errs, p2.calls)
	}

	p3, srv3 := newPortal(t, freshBox())
	code, out, errs = runWith(t, tmdbKey+"\ny\n", true, "metadata", "--url", srv3.URL, "--tmdb-key-stdin")
	if code != exitcode.OK || p3.sent(t) != tmdbKey {
		t.Fatalf("typed on a terminal: want 0 and the key, got %d\n%s\n%s", code, out, errs)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "(not shown)") || !strings.Contains(out, "the key typed on the terminal") || !strings.Contains(out, "[y/N]") {
		t.Errorf("a hidden prompt, then the question: %q\n%s", prompts, out)
	}
	noKey(t, "metadata on a terminal", out, errs)
}

// Ctrl-C at the hidden prompt ends the command as a signal does, with echo
// back on and nothing sent.
func TestMetadataInterruptedAtThePrompt(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	realHidden := readHidden
	defer func() { readHidden = realHidden }()
	var ob, eb strings.Builder
	stdout, stderr = &ob, &eb
	canAsk = func() bool { return true }
	readHidden = func(string) (string, error) { return "", errInterrupted{os.Interrupt} }
	code := Run([]string{"metadata", "--url", srv.URL, "--tmdb-key-stdin"})
	stdout, stderr = os.Stdout, os.Stderr
	if code != 130 || len(p.calls) != 0 {
		t.Fatalf("interrupted: want 130 and nothing sent, got %d %v", code, p.calls)
	}
}

// The key is never taken from the command line — where it would be kept in
// shell history — and none of the ways it could be typed there anyway is
// repeated: not as an argument, not as a flag's value, not as the path a file
// was meant to be at.
func TestMetadataNeverTakesTheKeyAsAnArgument(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	for name, args := range map[string][]string{
		"an argument":                  {"metadata", tmdbKey, "--url", srv.URL, "--yes"},
		"an argument after the flags":  {"metadata", "--url", srv.URL, "--tmdb-key-stdin", "--yes", tmdbKey},
		"a flag that takes no value":   {"metadata", "--url", srv.URL, "--tmdb-key-stdin=" + tmdbKey, "--yes"},
		"a flag that does not exist":   {"metadata", "--url", srv.URL, "--tmdb-key=" + tmdbKey, "--yes"},
		"a flag zae does not have":     {"metadata", "--url", srv.URL, "--tmdb-key", tmdbKey, "--yes"},
		"--yes given the key":          {"metadata", "--url", srv.URL, "--tmdb-key-stdin", "--yes=" + tmdbKey},
		"the key where the file goes":  {"metadata", "--url", srv.URL, "--tmdb-key-file", tmdbKey, "--yes"},
		"the key where the file goes=": {"metadata", "--url", srv.URL, "--tmdb-key-file=" + tmdbKey, "--yes"},
	} {
		code, out, errs := run(t, args...)
		if code != exitcode.Usage {
			t.Errorf("%s: want 2, got %d %q", name, code, errs)
		}
		noKey(t, name, out, errs)
		if !strings.Contains(errs, "--tmdb-key-file") {
			t.Errorf("%s: the refusal says where the key goes: %q", name, errs)
		}
	}
	if len(p.calls) != 0 {
		t.Fatalf("a refused invocation reaches no instance: %v", p.calls)
	}
}

// What cannot be a TMDB token is refused before anything is sent — the
// portal's own rules, and TMDB's v3 API key, which the portal would take and
// TMDB then refuse without a word — and none of it repeats what was read.
func TestMetadataRefusesWhatCannotBeAKey(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	for name, c := range map[string]struct{ content, says string }{
		"an empty file":       {"", "the key is empty"},
		"whitespace":          {" \n\t\n", "the key is empty"},
		"two lines":           {tmdbKey + "\n" + tmdbKey + "\n", "spaces or line breaks"},
		"a key with a space":  {"eyJ " + tmdbKey, "spaces or line breaks"},
		"longer than a token": {strings.Repeat("e", maxKey+1), "longer than any TMDB token"},
		"a control character": {tmdbKey + "\x00", "control characters"},
		"the v3 API key":      {"0123456789abcdef0123456789ABCDEF", "that is TMDB's API key (v3); the catalog needs the API read access token (v4)"},
	} {
		code, out, errs := run(t, "metadata", "--url", srv.URL, "--tmdb-key-file", keyFile(t, c.content), "--yes")
		if code != exitcode.Usage || !strings.Contains(errs, c.says) {
			t.Errorf("%s: want 2 saying %q, got %d %q", name, c.says, code, errs)
		}
		noKey(t, name, out, errs)
		if c.content != "" && strings.TrimSpace(c.content) != "" && strings.Contains(errs, strings.TrimSpace(c.content)[:8]) {
			t.Errorf("%s: what was read is repeated: %q", name, errs)
		}
	}
	if code, _, errs := run(t, "metadata", "--url", srv.URL, "--tmdb-key-file", filepath.Join(t.TempDir(), "absent"), "--yes"); code != exitcode.Usage ||
		!strings.Contains(errs, "cannot open the file it names (no such file or directory)") {
		t.Errorf("a file that is not there: want 2, got %d %q", code, errs)
	}
	if len(p.calls) != 0 {
		t.Fatalf("a key that cannot be one reaches no instance: %v", p.calls)
	}
}

// Should the portal ever repeat the key — in a refusal, in its answer — zae
// still does not print it.
func TestMetadataDoesNotRepeatAKeyThePortalRepeats(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	p.echo = "refusal"
	code, out, errs := run(t, "metadata", "--url", srv.URL, "--tmdb-key-file", keyFile(t, tmdbKey), "--yes")
	if code != exitcode.Failed || !strings.Contains(errs, "tmdbKey [the key] is not one this portal takes") {
		t.Fatalf("a refusal quoting the key: want 1, with the key cut out, got %d %q", code, errs)
	}
	noKey(t, "a refusal that quotes the key", out, errs)

	// An answer that carries it where zae prints the portal's words: the
	// step's state, and its note.
	p2, srv2 := newPortal(t, freshBox())
	p2.echo = "answer"
	code, out, errs = run(t, "metadata", "--url", srv2.URL, "--tmdb-key-file", keyFile(t, tmdbKey), "--yes")
	if code != exitcode.OK || !strings.Contains(out, "[the key]") {
		t.Fatalf("an answer that carries the key: want 0, with the key cut out, got %d\n%s\n%s", code, out, errs)
	}
	noKey(t, "an answer that carries the key", out, errs)
}

// The catalog manager is where the key goes, through the portal: without one
// there is nowhere to set it (3, before asking); one that refuses the admin is
// forbidden (5); one that does not answer leaves it open whether the key was
// set (4); one that answers an error is a failure (1).
func TestMetadataWhereTheCatalogDoesNotTakeIt(t *testing.T) {
	file := keyFile(t, tmdbKey)

	none := freshBox()
	none.Metadata = Metadata{Step: Step{State: stateUnknown, Note: catalogAnswers["none"].body}, Key: "none"}
	p, srv := newPortal(t, none)
	code, out, errs := runWith(t, "y\n", true, "metadata", "--url", srv.URL, "--tmdb-key-file", file)
	if code != exitcode.NotOffered || strings.Contains(out, "[y/N]") || p.called("POST "+metadataPath) != 0 ||
		!strings.Contains(errs, "has no catalog manager to set the key in: portal-api is pointed at no catalog manager") {
		t.Errorf("no catalog manager: want 3 before asking, got %d\n%s\n%s", code, out, errs)
	}

	for catalog, want := range map[string]struct {
		code int
		says string
	}{
		"none":    {exitcode.NotOffered, "has no catalog manager to set the TMDB key"},
		"refused": {exitcode.Forbidden, "the catalog manager refused this admin: forbidden: settings requires the catalog-admin role — the account needs the catalog manager's admin role too"},
		"silent":  {exitcode.Undetermined, "the catalog manager did not answer: dial tcp 10.0.0.7:8080: i/o timeout — zae cannot tell whether it happened"},
		"failed":  {exitcode.Failed, "failed: set the TMDB key: the catalog manager answered: setting tmdb.api_key: database is read-only"},
	} {
		p, srv := newPortal(t, freshBox())
		p.catalog = catalog
		code, out, errs := run(t, "metadata", "--url", srv.URL, "--tmdb-key-file", file, "--yes")
		if code != want.code || !strings.Contains(errs, want.says) {
			t.Errorf("%s: want %d saying %q, got %d %q", catalog, want.code, want.says, code, errs)
		}
		noKey(t, catalog, out, errs)
	}

	p2, srv2 := newPortal(t, freshBox())
	p2.old = true
	if code, _, errs := run(t, "metadata", "--url", srv2.URL, "--tmdb-key-file", file, "--yes"); code != exitcode.NotOffered || !strings.Contains(errs, "predates the setup checklist") {
		t.Errorf("a portal-api without the checklist: want 3, got %d %q", code, errs)
	}
	p3, srv3 := newPortal(t, freshBox())
	p3.token = "the-admin-bearer"
	t.Setenv(instance.TokenEnv, "")
	if code, _, errs := run(t, "metadata", "--url", srv3.URL, "--tmdb-key-file", file, "--yes"); code != exitcode.Forbidden || p3.called("POST "+metadataPath) != 0 {
		t.Errorf("without the admin role: want 5, got %d %q", code, errs)
	}
}

// With everything else in order — a file that holds a good key, --yes — an
// argument is still refused, not ignored, and so is a second source: a key
// piped in beside --tmdb-key-file is not quietly preferred to it.
func TestMetadataRefusesAnArgumentOrTwoSourcesWhateverElseIsRight(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	file := keyFile(t, tmdbKey)
	code, out, errs := run(t, "metadata", "--url", srv.URL, "--tmdb-key-file", file, "--yes", tmdbKey)
	if code != exitcode.Usage {
		t.Errorf("an argument beside a good file: want 2, got %d %q", code, errs)
	}
	noKey(t, "an argument beside a good file", out, errs)
	code, _, errs = runWith(t, tmdbKey+"\n", false, "metadata", "--url", srv.URL, "--tmdb-key-file", file, "--tmdb-key-stdin", "--yes")
	if code != exitcode.Usage || !strings.Contains(errs, "give one") {
		t.Errorf("a key piped in beside a file: want 2, got %d %q", code, errs)
	}
	if len(p.calls) != 0 {
		t.Fatalf("a refused invocation reaches no instance: %v", p.calls)
	}
}

func TestMetadataUsage(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	file := keyFile(t, tmdbKey)
	usageCases(t, p, map[string][]string{
		"no source":     {"metadata", "--url", srv.URL, "--yes"},
		"both sources":  {"metadata", "--url", srv.URL, "--tmdb-key-file", file, "--tmdb-key-stdin", "--yes"},
		"an empty file": {"metadata", "--url", srv.URL, "--tmdb-key-file", " ", "--yes"},
		"no url":        {"metadata", "--tmdb-key-file", file, "--yes"},
	})
}
