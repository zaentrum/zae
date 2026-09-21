// Package platform is `zae platform`: seeing and driving the platform's own
// updates from a terminal.
//
// These are STATIC commands, like `zae addon`. They drive the portal's
// operator console — the admin API an instance has whether or not anything is
// installed on it — and nothing here knows any service by name. What they
// change is the operator's own resource: the version the platform is pinned
// to, the channel it follows, whether it applies in-channel updates by
// itself, and the replica count or rollout of one workload.
//
// What they do not change is the controller that reads that resource. It runs
// in its own namespace, outside the one the portal administers, and is
// updated by applying its install bundle. Every status says so, because the
// difference decides which of two very different things an administrator has
// to do.
//
// zae never prompts on a stdin that is not a terminal: such an invocation
// needs --yes, and without it fails as usage before anything is written.
package platform

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
	"github.com/zaentrum/zae/internal/term"
)

// Streams and clocks are variables so tests can drive the commands.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
	stdin  io.Reader = os.Stdin
	// canAsk: stdin is a terminal a person can answer on.
	canAsk = stdinIsTerminal
	// pollInterval paces every wait.
	pollInterval = 2 * time.Second
)

// defaultTimeout: a platform update rolls every service, pulling an image per
// workload, so it is measured in minutes and not in seconds.
const defaultTimeout = 10 * time.Minute

func usage(w io.Writer) {
	fmt.Fprint(w, `zae platform — the platform's own updates and workloads

Usage:
  zae platform status --url https://… [--json]
  zae platform update --url https://… [--version V] [--channel C] [--mode auto|manual]
      [--apply] [--yes] [--wait] [--timeout 10m]
  zae platform restart <workload> --url https://… [--yes] [--wait] [--timeout 10m]
  zae platform scale <workload> <replicas> --url https://… [--yes] [--wait] [--timeout 10m]

status shows the version the platform is pinned to — or that nothing is
pinned and it follows a channel — the channel, the update mode, the phase, the
version it reports running and whether an update is offered; then every
workload: the ones the operator renders first, addons after them, and whatever
neither claims last.

update changes what the platform asks for. --version pins an image tag
('latest' follows the channel again), --channel picks the release train,
--mode auto lets the operator apply in-channel updates by itself. --apply pins
the update the operator has already discovered, so it takes no version of its
own. --wait follows the rollout until the platform reports the new version and
every workload the operator manages is ready.

restart and scale act on one workload. The platform protects its stateful
services and refuses those itself, in its own words.

This does not update the operator's own controller image: the controller runs
outside the namespace the portal administers. Apply its install bundle for
that — or use OLM, where the cluster installs it that way.

Needs the platform's admin role: sign in with 'zae login --url …', or carry a
bearer in ZAE_TOKEN (which wins when it is set).
Exit codes: 0 done · 1 the instance refused it, the change was declined, or it
was not ready in time · 2 usage · 3 this instance has no operator console, or
no such workload · 4 undetermined · 5 forbidden.
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

// Run executes `zae platform <command> [args]`.
func Run(args []string) int {
	if len(args) == 0 {
		usage(stderr)
		return exitcode.Usage
	}
	switch args[0] {
	case "status":
		return status(args[1:])
	case "update":
		return update(args[1:])
	case "restart":
		return restart(args[1:])
	case "scale":
		return scale(args[1:])
	case "help", "--help", "-h":
		usage(stdout)
		return exitcode.OK
	default:
		errf("usage: zae platform has no command %q — status, update, restart, scale", args[0])
		return exitcode.Usage
	}
}

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("zae platform "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse reads flags and positionals in any order — `restart api --url U` and
// `restart --url U api` both work, where Go's flag package alone stops at the
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

// setFlags names the flags actually given, so that `--version ""` — take the
// pin off, follow the channel again — is told apart from not asking at all.
func setFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// usageErr prints a usage message and returns the usage exit code.
func usageErr(format string, a ...any) int {
	errf("usage: "+format, a...)
	return exitcode.Usage
}

// stdinIsTerminal reports whether a person can answer on stdin.
func stdinIsTerminal() bool { return term.Is(os.Stdin) }

// requireTTY refuses, as usage and before anything is written, an invocation
// that would ask on a stdin nobody can answer.
func requireTTY(yes bool, doing string) int {
	if !yes && !canAsk() {
		return usageErr("stdin is not a terminal, so zae will not ask before %s — add --yes", doing)
	}
	return exitcode.OK
}

// confirm asks on stdin. Anything but y or yes is no — an empty line and a
// closed stdin included — so a stray Enter never changes the platform.
func confirm(question string) bool {
	fmt.Fprintf(stdout, "%s [y/N] ", question)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(stdout)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// requireConsole reads the console and refuses an instance that has none to
// drive, having already said why; cons is nil and code is the exit status.
func requireConsole(ctx context.Context, c *client, base string) (cons *Console, code int) {
	cons, _, err := c.console(ctx)
	if err != nil {
		return nil, fail(err)
	}
	if !cons.offered() {
		errf("not offered: %s", cons.noConsole(base))
		return nil, exitcode.NotOffered
	}
	return cons, exitcode.OK
}

// status prints the platform and its workloads on one screen.
func status(args []string) int {
	fs := flagSet("status")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	asJSON := fs.Bool("json", false, "print the portal's answer as JSON")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return usageErr("zae platform status --url https://… takes no arguments")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	cons, raw, cerr := newClient(base).console(context.Background())
	if cerr != nil {
		return fail(cerr)
	}
	if *asJSON {
		// The portal's own document, unchanged: --json is for scripts, and a
		// script should read the API's shape rather than zae's view of it.
		fmt.Fprintln(stdout, strings.TrimSpace(string(raw)))
	}
	if !cons.offered() {
		errf("not offered: %s", cons.noConsole(base))
		return exitcode.NotOffered
	}
	if !*asJSON {
		renderStatus(stdout, base, cons)
	}
	return exitcode.OK
}

// update changes what the platform asks for, and optionally follows the
// rollout that answers.
func update(args []string) int {
	fs := flagSet("update")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	version := fs.String("version", "", "pin the platform to this image tag; 'latest' follows the channel again")
	channel := fs.String("channel", "", "the release channel the operator follows")
	mode := fs.String("mode", "", "update mode: auto (the operator applies in-channel updates itself) or manual")
	apply := fs.Bool("apply", false, "pin the platform to the update the operator has already discovered")
	yes := fs.Bool("yes", false, "change it without asking")
	wait := fs.Bool("wait", false, "follow the rollout until the platform reports the new version and every workload the operator manages is ready")
	timeout := fs.Duration("timeout", defaultTimeout, "how long --wait lasts")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return usageErr("zae platform update --url https://… takes no arguments")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	set := setFlags(fs)
	switch {
	case !set["version"] && !set["channel"] && !set["mode"] && !*apply:
		return usageErr("zae platform update changes nothing on its own — pass --version, --channel, --mode or --apply")
	case *apply && set["version"]:
		return usageErr("--apply pins the update the operator discovered, and --version pins the one you name — use one of them")
	case *apply && set["channel"]:
		return usageErr("--apply applies the update discovered on the channel the platform follows now, which is not the one --channel asks for — run zae platform update --channel %s first, then --apply once the operator has looked", strings.TrimSpace(*channel))
	case set["mode"] && *mode != "auto" && *mode != "manual":
		return usageErr("--mode is auto or manual, not %q", *mode)
	case set["channel"] && strings.TrimSpace(*channel) == "":
		return usageErr("--channel names the release channel to follow, e.g. --channel stable")
	case *timeout <= 0:
		return usageErr("--timeout must be positive")
	}
	if code := requireTTY(*yes, "changing the platform"); code != exitcode.OK {
		return code
	}

	ctx := context.Background()
	c := newClient(base)
	cons, code := requireConsole(ctx, c, base)
	if cons == nil {
		return code
	}
	op := cons.Operator

	// target is the version a --wait watches for, "" when there is none.
	target := ""
	var chs []change
	body := map[string]any{}
	if set["version"] {
		v := strings.TrimSpace(*version)
		body["version"] = v
		chs = append(chs, change{"version", versionOr(op.Version), versionOr(v)})
		target = waitVersion(v)
	}
	if set["channel"] {
		v := strings.TrimSpace(*channel)
		body["channel"] = v
		chs = append(chs, change{"channel", op.Channel, v})
	}
	if set["mode"] {
		body["updateMode"] = *mode
		chs = append(chs, change{"update mode", op.UpdateMode, *mode})
	}
	if *apply {
		// The operator discovered it; applying is pinning the platform to it.
		// When it has discovered nothing there is no update to apply, and
		// saying that here beats a 400 that says the same thing later.
		up := strings.TrimSpace(op.AvailableUpdate)
		if up == "" {
			errf("failed: no update to apply — the operator offers none on the %s channel, and reports %s running",
				channelOr(op.Channel), dash(op.CurrentVersion))
			return exitcode.Failed
		}
		chs = append(chs, change{"version", versionOr(op.Version), up})
		target = waitVersion(up)
	}

	rolls := 0
	if set["version"] || *apply {
		rolls = len(cons.managed())
	}
	renderChanges(stdout, base, chs, rolls)
	if !*yes && !confirm(fmt.Sprintf("apply this to %s?", base)) {
		fmt.Fprintln(stdout, "nothing changed")
		return exitcode.Failed
	}

	if len(body) > 0 {
		if err := c.do(ctx, "change the platform", http.MethodPatch, operatorPath, body, nil); err != nil {
			return fail(err)
		}
	}
	if *apply {
		if err := c.do(ctx, "apply the update", http.MethodPost, applyPath, nil, nil); err != nil {
			return fail(err)
		}
	}
	if !*wait {
		fmt.Fprintf(stdout, "asked %s — follow it with zae platform status --url %s\n", base, base)
		return exitcode.OK
	}
	waitingFor := "every workload the operator manages to be ready"
	settled := "every workload the operator manages is ready"
	if target != "" {
		waitingFor = fmt.Sprintf("the platform to report %s, and every workload the operator manages to be ready", target)
		settled = fmt.Sprintf("the platform reports %s, and every workload the operator manages is ready", target)
	}
	return follow(ctx, c, waitingFor, settled, *timeout, false, platformSettled(target))
}

// restart rolls one workload.
func restart(args []string) int {
	fs := flagSet("restart")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	yes := fs.Bool("yes", false, "restart without asking")
	wait := fs.Bool("wait", false, "wait until the workload is ready again")
	timeout := fs.Duration("timeout", defaultTimeout, "how long --wait lasts")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageErr("zae platform restart <workload> --url https://… takes one workload name")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	if *timeout <= 0 {
		return usageErr("--timeout must be positive")
	}
	name := pos[0]
	if code := requireTTY(*yes, "restarting "+name); code != exitcode.OK {
		return code
	}

	ctx := context.Background()
	c := newClient(base)
	cons, code := requireConsole(ctx, c, base)
	if cons == nil {
		return code
	}
	w := cons.find(name)
	if w == nil {
		return noSuchWorkload(base, name, cons)
	}
	fmt.Fprintln(stdout, describe(w))
	if !askFirst(w, *yes, fmt.Sprintf("restart %s on %s?", name, base)) {
		fmt.Fprintln(stdout, "nothing restarted")
		return exitcode.Failed
	}
	if err := c.do(ctx, "restart "+name, http.MethodPost, instancePath(name, "restart"), nil, nil); err != nil {
		return fail(err)
	}
	if !*wait {
		fmt.Fprintf(stdout, "restarting %s — follow it with zae platform status --url %s\n", name, base)
		return exitcode.OK
	}
	return follow(ctx, c, name+" to be ready again", name+" is ready", *timeout, true, workloadSettled(name, -1))
}

// scale sets one workload's replica count.
func scale(args []string) int {
	fs := flagSet("scale")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	yes := fs.Bool("yes", false, "scale without asking")
	wait := fs.Bool("wait", false, "wait until the workload runs that many replicas")
	timeout := fs.Duration("timeout", defaultTimeout, "how long --wait lasts")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 2 {
		return usageErr("zae platform scale <workload> <replicas> --url https://… takes a workload name and a replica count")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	if *timeout <= 0 {
		return usageErr("--timeout must be positive")
	}
	name := pos[0]
	// How many replicas a workload may have is the platform's rule, and it
	// enforces it; zae only insists on a number.
	n, err := strconv.Atoi(pos[1])
	if err != nil {
		return usageErr("zae platform scale %s <replicas> takes a replica count, not %q", name, pos[1])
	}
	if code := requireTTY(*yes, fmt.Sprintf("scaling %s", name)); code != exitcode.OK {
		return code
	}

	ctx := context.Background()
	c := newClient(base)
	cons, code := requireConsole(ctx, c, base)
	if cons == nil {
		return code
	}
	w := cons.find(name)
	if w == nil {
		return noSuchWorkload(base, name, cons)
	}
	fmt.Fprintln(stdout, describe(w))
	if !askFirst(w, *yes, fmt.Sprintf("scale %s from %d to %d on %s?", name, w.DesiredReplicas, n, base)) {
		fmt.Fprintln(stdout, "nothing scaled")
		return exitcode.Failed
	}
	what := fmt.Sprintf("scale %s to %d", name, n)
	if err := c.do(ctx, what, http.MethodPost, instancePath(name, "scale"), map[string]any{"replicas": n}, nil); err != nil {
		return fail(err)
	}
	if !*wait {
		fmt.Fprintf(stdout, "asked for %d — follow it with zae platform status --url %s\n", n, base)
		return exitcode.OK
	}
	return follow(ctx, c, fmt.Sprintf("%s to run %d", name, n), fmt.Sprintf("%s runs %d", name, n),
		*timeout, false, workloadSettled(name, n))
}

// askFirst asks before a change, unless --yes was given or the platform
// protects the workload. A protected workload is sent anyway and refused by
// the platform: the rule and its wording belong to the side that makes it,
// and there is nothing to confirm about a call that changes nothing.
func askFirst(w *Workload, yes bool, question string) bool {
	if yes || w.Protected {
		return true
	}
	return confirm(question)
}

// noSuchWorkload is exit 3: the instance answered, and it runs nothing by
// that name. Definitive — a script may branch on it.
func noSuchWorkload(base, name string, cons *Console) int {
	errf("not offered: %s runs no workload named %q — zae platform status --url %s lists the %s it runs",
		base, name, base, count(len(cons.Instances), "workload", "workloads"))
	return exitcode.NotOffered
}

// describe is one workload on one line, for a confirmation.
func describe(w *Workload) string {
	s := fmt.Sprintf("%s — %s · %s · %d/%d %s", w.Name, groupLabel(*w), imageTag(w.Image),
		w.ReadyReplicas, w.DesiredReplicas, dash(w.Phase))
	if w.Reason != "" {
		s += " (" + w.Reason + ")"
	}
	if w.Protected {
		s += " · protected by the platform"
	}
	return s
}

// versionOr names a version for a person: no pin is `latest`, which is what
// the operator makes of it.
func versionOr(v string) string {
	if strings.TrimSpace(v) == "" {
		return "latest"
	}
	return strings.TrimSpace(v)
}

// waitVersion is the version a wait can watch for. `latest` is not one: it
// asks the operator to follow the channel, and what it then runs is whatever
// the channel resolved to, not the word.
func waitVersion(v string) string {
	if v = strings.TrimSpace(v); v == "latest" {
		return ""
	}
	return v
}
