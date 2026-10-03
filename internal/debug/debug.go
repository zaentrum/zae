// Package debug is `zae debug`: the portal's debug console, from a terminal —
// to begin with, a workload's container logs.
//
// These are STATIC commands, like `zae addon` and `zae platform`: they read
// the portal's own debug API (GET /api/portal/debug/…), which an instance has
// whether or not anything is installed on it, and nothing here knows a service
// by name. Every one of them only reads. Nothing is fetched around the portal —
// no cluster credentials, no kubectl — so what a terminal shows is what the
// console shows, redacted by the portal; and zae redacts it once more, with the
// portal's own rules, before it prints or writes a byte (internal/redact).
//
// The API is admin-only, and so are these commands: a bearer without the
// platform's admin role is refused by the portal and exits 5.
package debug

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
)

// Streams and clocks are variables so tests can drive the commands.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
	// pollInterval paces --follow.
	pollInterval = 2 * time.Second
	// now is the clock a follow measures its windows with.
	now = time.Now
	// notifySignals subscribes c to the signals that end a command. Tests
	// substitute it to deliver one on cue.
	notifySignals = func(c chan<- os.Signal) (stop func()) {
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		return func() { signal.Stop(c) }
	}
)

func usage(w io.Writer) {
	fmt.Fprint(w, `zae debug — the portal's debug console, read from a terminal

Usage:
  zae debug logs <workload|pod> --url https://… [--container C] [--tail N] [--since 10m]
      [--follow] [--json]

logs prints a workload's container logs: the pods Kubernetes names for a
Deployment (or a StatefulSet, DaemonSet or Job) of that name — or one pod, by
its own name — every container of each unless --container picks one, merged
by time. With more than one, each line starts with [pod/container]. --tail is
lines per container (the portal's default is 500, its most 5000); --since
keeps only newer lines. --follow keeps reading until Ctrl-C, and takes up the
pods a rollout replaces them with. --json prints one object per line.

Everything comes through the portal's debug API and is redacted there; zae
redacts it again with the portal's own rules before it prints or writes it.
Nothing here writes to the instance.

Needs the platform's admin role: sign in with 'zae login --url …', or carry a
bearer in ZAE_TOKEN (which wins when it is set).
Exit codes: 0 done · 1 the instance returned an error · 2 usage · 3 not offered
(no such workload or container; no cluster) · 4 undetermined · 5 forbidden ·
130/143 interrupted.
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

// Run executes `zae debug <command> [args]`.
func Run(args []string, version string) int {
	if len(args) == 0 {
		usage(stderr)
		return exitcode.Usage
	}
	switch args[0] {
	case "logs":
		return logs(args[1:])
	case "help", "--help", "-h":
		usage(stdout)
		return exitcode.OK
	default:
		errf("usage: zae debug has no command %q — logs", args[0])
		return exitcode.Usage
	}
}

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("zae debug "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse reads flags and positionals in any order — `logs api --url U` and
// `logs --url U api` both work, where Go's flag package alone stops at the
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

// setFlags names the flags actually given, so that --tail 0 is told apart
// from no --tail at all.
func setFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// listFlag collects a repeatable flag whose values may also be comma lists.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// session is one command's lifetime: a context that ends on Ctrl-C or
// SIGTERM, so a follow stops where it is.
type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	sig    os.Signal
}

func newSession() *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{ctx: ctx, cancel: cancel}
	ch := make(chan os.Signal, 1)
	stop := notifySignals(ch)
	go func() {
		defer stop()
		select {
		case sig := <-ch:
			s.mu.Lock()
			s.sig = sig
			s.mu.Unlock()
			cancel()
		case <-ctx.Done():
		}
	}()
	return s
}

func (s *session) close() { s.cancel() }

func (s *session) interrupted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sig != nil
}

// exitCode is 128 plus the signal's number: 130 for Ctrl-C, 143 for SIGTERM.
func (s *session) exitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sig, ok := s.sig.(syscall.Signal); ok {
		return 128 + int(sig)
	}
	return 130
}

// sleep waits d, or until the session ends; false when it ended.
func (s *session) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// printable keeps text from the cluster on one line and out of the terminal's
// control: an event's type or key is what some producer wrote, and an escape
// sequence in it would be executed by the terminal rather than shown.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}
