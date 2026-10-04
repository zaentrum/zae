// Package setup is `zae setup`: the platform's first-run checklist, from a
// terminal — which steps are done, what to do next, and the writes that do it.
//
// These are STATIC commands, like `zae platform`: setup is what a fresh
// platform needs before anything is installed on it that could declare a
// command. They drive the portal's setup API — GET /api/portal/setup and the
// writes beside it, admin-only — which reads every step live from where it is
// configured: the catalog (with the caller's own bearer), the operator's
// resource, the cluster. The portal stores nothing of it but the record that
// setup was marked done, and nothing here knows a service by name.
//
// The TMDB key is a secret. It is read from a file or from stdin — asked for
// without echo on a terminal — and never taken from the command line, where it
// would land in shell history and the process list. zae never prints it: not
// in a summary, not in an error, not where the portal's own words are quoted.
//
// zae never prompts on a stdin that is not a terminal: an invocation that
// would ask needs --yes, and without it fails as usage before anything is
// written.
package setup

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// Streams and clocks are variables so tests can drive the commands.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
	// now is the clock "3 min ago" is read against.
	now = time.Now
)

func usage(w io.Writer) {
	fmt.Fprint(w, `zae setup — the platform's first-run checklist

Usage:
  zae setup --url https://… [--json]

The checklist is what the launchpad shows an admin until one marks setup
done: metadata (a TMDB key), library (the titles, where files go, the latest
scan), processing (the media pipeline and its workers), devices (https, which
phones and TVs sign in over) and people (accounts, which nothing here checks).
The portal reads each step live from where it is configured; zae prints each
one's state and what to do next. --json prints the portal's own document.

Needs the platform's admin role: sign in with 'zae login --url …', or carry a
bearer in ZAE_TOKEN (which wins when it is set).
Exit codes: 0 read · 2 usage · 3 not offered (a portal-api without the
checklist) · 4 undetermined · 5 forbidden.
`)
}

func errf(format string, a ...any) { fmt.Fprintf(stderr, "zae: "+format+"\n", a...) }

// fail prints an error and returns its exit code.
func fail(err error) int {
	if ae, ok := asAPIError(err); ok {
		errf("%s", ae.msg)
		return ae.code
	}
	errf("failed: %v", err)
	return exitcode.Failed
}

// usageErr prints a usage message and returns the usage exit code.
func usageErr(format string, a ...any) int {
	errf("usage: "+format, a...)
	return exitcode.Usage
}

// Run executes `zae setup [command] [args]`. Without a command it prints the
// checklist.
func Run(args []string) int {
	if len(args) == 0 {
		usage(stderr)
		return exitcode.Usage
	}
	switch args[0] {
	case "help", "--help", "-h":
		usage(stdout)
		return exitcode.OK
	}
	if strings.HasPrefix(args[0], "-") {
		return checklist(args)
	}
	errf("usage: zae setup has no command %q — the checklist is zae setup --url …", args[0])
	return exitcode.Usage
}

// checklist prints every step of the checklist, its state and what to do
// next.
func checklist(args []string) int {
	fs := flagSet("")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	asJSON := fs.Bool("json", false, "print the portal's checklist as JSON")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return usageErr("zae setup --url https://… takes no arguments")
	}
	base, code := baseOf(*rawURL)
	if code != exitcode.OK {
		return code
	}
	d, raw, err := read(context.Background(), newClient(base))
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		// The portal's own document, unchanged: --json is for scripts, and a
		// script should read the API's shape rather than zae's view of it.
		fmt.Fprintln(stdout, strings.TrimSpace(string(raw)))
		return exitcode.OK
	}
	renderChecklist(stdout, base, d, now())
	return exitcode.OK
}

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(strings.TrimSpace("zae setup "+name), flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse reads flags and positionals in any order — `pipeline on --url U` and
// `pipeline --url U on` both work, where Go's flag package alone stops at the
// first positional. A "--" ends the flags. ok is false when parsing ended the
// command; code is then its exit status (0 for -h).
func parse(fs *flag.FlagSet, args []string) (pos []string, code int, ok bool) {
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, exitcode.OK, false
			}
			return nil, exitcode.Usage, false
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, exitcode.OK, true
		}
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), exitcode.OK, true
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// baseOf validates --url, as usage.
func baseOf(raw string) (string, int) {
	base, err := instance.Base(raw)
	if err != nil {
		return "", usageErr("%v", err)
	}
	return base, exitcode.OK
}
