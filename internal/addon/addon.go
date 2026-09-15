// Package addon is `zae addon`: adding, inspecting, upgrading and removing
// addons the platform installs from a Helm chart (ADR-0011).
//
// Unlike `zae <service> <command>`, these are STATIC commands. They drive the
// portal's admin API for chart addons, which an instance has whether or not
// any addon is installed — and nothing here knows any addon: an addon is a
// chart reference and a name.
//
// Plan first, always. add and upgrade leave the addon suspended — the
// operator plans it and applies nothing — wait for the plan the operator made
// for exactly that write, print it, and install only when the admin answers yes
// or passed --yes. A plan the guardrails refused, or whose values do not
// validate, is never installed. zae never prompts on a stdin that is not a
// terminal: such an invocation needs --yes, and without it fails as usage
// before anything is written. Secret values are never printed.
package addon

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	stdin  io.Reader = os.Stdin
	// canAsk: stdin is a terminal a person can answer on.
	canAsk = stdinIsTerminal
	// pollInterval paces every wait.
	pollInterval = 2 * time.Second
	// notFoundGrace: right after the write that creates an addon, a portal
	// that reads through a cache may not see it yet. A 404 inside this window
	// is asked again rather than read as "no such addon".
	notFoundGrace = 15 * time.Second
)

const defaultTimeout = 5 * time.Minute

func usage(w io.Writer) {
	fmt.Fprint(w, `zae addon — addons the platform installs from a Helm chart

Usage:
  zae addon add <chart> --url https://… [--name N] [--version V] [--digest sha256:…]
      [--values FILE|-] [--set path=value]…
      [--set-secret path[=value]]… [--set-secret-file path=FILE]… [--secret-values FILE|-]
      [--secret-ref path=name/key]… [--yes] [--wait] [--timeout 5m]
  zae addon list --url https://… [--json]
  zae addon status <name> --url https://… [--json]
  zae addon upgrade <name> --url https://… [--version V | --chart REF] [--digest sha256:…]
      [--values FILE|-] [--set path=value]… [--set-secret path[=value]]… [--set-secret-file path=FILE]…
      [--secret-values FILE|-] [--clear-secret path]… [--yes] [--wait] [--timeout 5m]
  zae addon remove <name> --url https://… [--keep-values] [--yes]

<chart> is oci://registry/path/chart with --version (or :tag), or an https://
link to a chart archive; pin the archive with --digest. add and upgrade show
the operator's plan — chart, workloads, images, ports, refusals, missing
inputs — and ask before installing. --yes skips the question; without a
terminal on stdin it is required.

Values: --values is one JSON object (a file, or - for stdin); --set path=value
sets one value, JSON when it parses as JSON and a string otherwise
('tag="1.10"' forces a string). add starts from --values; upgrade changes the
current values unless --values replaces them.

Secret inputs are stored in a Secret and never shown again:
  --set-secret path            asked for on the terminal, not echoed
  --set-secret-file path=FILE  read from a file, one trailing newline trimmed
  --secret-values FILE|-       a JSON object of dotted path → string
  --set-secret path=value      on the command line: visible in the process list
They add to the secret inputs already set; --clear-secret path removes one.
--secret-ref path=name/key reuses a key of a values Secret kept by
'remove --keep-values'.
--wait waits until the addon is Ready and registered in the portal.

Needs the platform's admin role: ZAE_TOKEN carries the bearer.
Exit codes: 0 done · 1 refused, failed, declined or not Ready in time · 2 usage ·
3 no such addon, or the instance cannot install addons from charts ·
4 undetermined · 5 forbidden · 130/143 interrupted (an upgrade is put back first).
`)
}

func errf(format string, a ...any) { fmt.Fprintf(stderr, "zae: "+format+"\n", a...) }

// fail prints an error and returns its exit code.
func fail(err error) int {
	var (
		ae *apiError
		ce *changedError
	)
	switch {
	case errors.As(err, &ae):
		errf("%s", ae.msg)
		return ae.code
	case errors.As(err, &ce):
		errf("%s", ce.Error())
		return exitcode.Failed
	}
	errf("failed: %v", err)
	return exitcode.Failed
}

// failOr is fail, unless a signal ended the session: then it says what is left
// and returns the signal's exit code.
func failOr(s *session, err error, whenInterrupted string) int {
	if s.interrupted() {
		if whenInterrupted != "" {
			fmt.Fprintln(stdout, whenInterrupted)
		}
		return s.exitCode()
	}
	return fail(err)
}

// Run executes `zae addon <command> [args]`.
func Run(args []string) int {
	if len(args) == 0 {
		usage(stderr)
		return exitcode.Usage
	}
	switch args[0] {
	case "add":
		return add(args[1:])
	case "list":
		return list(args[1:])
	case "status":
		return status(args[1:])
	case "upgrade":
		return upgrade(args[1:])
	case "remove":
		return remove(args[1:])
	case "help", "--help", "-h":
		usage(stdout)
		return exitcode.OK
	default:
		errf("usage: zae addon has no command %q — add, list, status, upgrade, remove", args[0])
		return exitcode.Usage
	}
}

// multiFlag collects a repeatable flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("zae addon "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse reads flags and positionals in any order — `add REF --url U` and
// `add --url U REF` both work, where Go's flag package alone stops at the
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

// usageErr prints a usage message and returns the usage exit code.
func usageErr(format string, a ...any) int {
	errf("usage: "+format, a...)
	return exitcode.Usage
}

// secretFlags registers the secret input flags shared by add and upgrade.
func secretFlags(fs *flag.FlagSet, args *[]secretArg) *string {
	fs.Var(&secretFlag{args: args}, "set-secret", "path to be asked for without echo, or path=value — a value given here is visible in the process list (repeatable)")
	fs.Var(&secretFlag{args: args, file: true}, "set-secret-file", "path=FILE: a secret input read from FILE, one trailing newline trimmed (repeatable)")
	return fs.String("secret-values", "", "secret inputs as one JSON object of dotted path to string: a file, or - for stdin")
}

// checkStdin refuses, as usage and before anything is read or written, an
// invocation that needs stdin for two things — a document and a question, a
// document and a secret prompt, two documents — or that would ask on a stdin
// nobody can answer.
func checkStdin(yes bool, valuesSrc, secretSrc string, prompts []string, doing string) int {
	fromStdin := 0
	for _, src := range []string{valuesSrc, secretSrc} {
		if src == "-" {
			fromStdin++
		}
	}
	switch {
	case fromStdin == 2:
		return usageErr("--values - and --secret-values - cannot both read stdin")
	case fromStdin == 1 && len(prompts) > 0:
		return usageErr("--set-secret %s asks on the terminal, but stdin carries a document — give that secret with --set-secret-file", prompts[0])
	case len(prompts) > 0 && !canAsk():
		return usageErr("--set-secret %s asks for the value on a terminal, and stdin is not one — use --set-secret-file or --secret-values", prompts[0])
	case !yes && fromStdin == 1:
		return usageErr("stdin carries a document, and the confirmation needs it too — add --yes")
	case !yes && !canAsk():
		return usageErr("stdin is not a terminal, so zae will not ask before %s — add --yes", doing)
	}
	return exitcode.OK
}

// add: POST the addon (suspended), wait for the plan of that write, print it,
// confirm, install, and with --wait follow it to Ready.
func add(args []string) int {
	s := newSession()
	defer s.close()

	fs := flagSet("add")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	name := fs.String("name", "", "addon name; default: the chart's name from the reference")
	version := fs.String("version", "", "chart version: the tag of an oci:// chart")
	digest := fs.String("digest", "", "sha256:… that the chart archive must match")
	valuesSrc := fs.String("values", "", "values as one JSON object: a file, or - for stdin")
	var sets multiFlag
	fs.Var(&sets, "set", "path=value: one value, JSON when it parses as JSON, else a string (repeatable)")
	var secrets []secretArg
	secretValuesSrc := secretFlags(fs, &secrets)
	var refs multiFlag
	fs.Var(&refs, "secret-ref", "path=name/key: a secret input kept in one of the addon's values Secrets, e.g. from a removal that kept them (repeatable)")
	yes := fs.Bool("yes", false, "install without asking")
	wait := fs.Bool("wait", false, "after installing, wait until the addon is Ready and registered")
	timeout := fs.Duration("timeout", defaultTimeout, "how long each wait lasts: for the plan, and with --wait for Ready")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageErr("zae addon add <chart> --url https://… takes one chart reference")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	chart, err := parseChart(pos[0], *version)
	if err != nil {
		return usageErr("%v", err)
	}
	if !chart.oci && *version != "" {
		errf("note: --version does not apply to an https archive, which is one version — ignoring it")
	}
	if *digest != "" && !digestRe.MatchString(*digest) {
		return usageErr("--digest %q is not sha256: followed by 64 hex digits", *digest)
	}
	n := *name
	if n == "" {
		n = defaultName(chart.ref)
		if !validName(n) {
			return usageErr("the name %q taken from the chart reference is not a valid addon name (lowercase letters, digits and -, at most %d characters) — pass --name", n, maxNameLen)
		}
	}
	if !validName(n) {
		return usageErr("--name %q is not a valid addon name (lowercase letters, digits and -, at most %d characters)", n, maxNameLen)
	}
	if *timeout <= 0 {
		return usageErr("--timeout must be positive")
	}
	if err := checkSecretArgs(secrets, nil); err != nil {
		return usageErr("%v", err)
	}
	secretRefs, err := parseSecretRefs(n, refs)
	if err != nil {
		return usageErr("%v", err)
	}
	// Everything that can fail as usage is checked before anything is written.
	if code := checkStdin(*yes, *valuesSrc, *secretValuesSrc, prompted(secrets), "installing"); code != exitcode.OK {
		return code
	}
	values, err := valuesFrom(*valuesSrc, nil, sets)
	if err != nil {
		return usageErr("%v", err)
	}
	secretValues, err := s.secretInputs(*secretValuesSrc, secrets)
	if errors.Is(err, errInterrupted) {
		return s.exitCode()
	}
	if err != nil {
		return usageErr("%v", err)
	}
	if err := noValueIsSecret(values, secretValues); err != nil {
		return usageErr("%v", err)
	}
	for path := range secretRefs {
		if _, both := secretValues[path]; both {
			return usageErr("%s is given as a secret input and as --secret-ref — pick one", path)
		}
		if _, clash := lookup(values, path); clash {
			return usageErr("%s is given as a value and as --secret-ref — pick one", path)
		}
	}

	c := newClient(base)
	before, gerr := c.get(s.ctx, n)
	ae, _ := asAPIError(gerr)
	switch {
	case gerr == nil && (before.installed() || !before.Suspended):
		errf("failed: %s is already installed on %s (%s) — change it with zae addon upgrade %s, or remove it first", n, base, describe(before), n)
		return exitcode.Failed
	case gerr == nil:
		fmt.Fprintf(stdout, "%s is planned but not installed — its values are replaced; secret inputs already set stay, these are added (zae addon upgrade %s --clear-secret PATH removes one)\n", n, n)
	case ae != nil && ae.notFound && !ae.noAPI:
	default:
		return failOr(s, gerr, "")
	}

	body := map[string]any{"name": n, "chart": chart.ref}
	if chart.oci && chart.version != "" {
		body["version"] = chart.version
	}
	if *digest != "" {
		body["digest"] = *digest
	}
	if len(values) > 0 {
		body["values"] = values
	}
	if len(secretValues) > 0 {
		body["secretValues"] = secretValues
	}
	if len(secretRefs) > 0 {
		body["secretRefs"] = secretRefs
	}
	var acc accepted
	if err := c.do(s.ctx, "add "+n, http.MethodPost, chartsPath, body, &acc, ""); err != nil {
		return failOr(s, err, fmt.Sprintf("interrupted — zae cannot tell whether %s was added; zae addon status %s --url %s shows it", n, n, base))
	}
	if acc.Name != "" {
		n = acc.Name
	}
	fmt.Fprintf(stdout, "planning %s from %s on %s …\n", n, chart, base)

	stays := fmt.Sprintf("%s stays planned and applies nothing; discard it with zae addon remove %s --url %s --yes", n, n, base)
	a, done, perr := c.poll(s, n, *timeout, notFoundGrace, planFor(n, acc.Generation), nil)
	switch {
	case errors.Is(perr, errInterrupted):
		fmt.Fprintln(stdout, "interrupted — "+stays)
		return s.exitCode()
	case perr != nil:
		return fail(perr)
	case !done:
		errf("failed: no plan for %s after %s (%s) — is the operator running, and does it reconcile ZaentrumAddon resources?", n, *timeout, lastSeen(a))
		return exitcode.Failed
	}
	renderPlan(stdout, a, false)
	if reason := planRefusal(a); reason != "" {
		errf("failed: %s is not installed: %s. Fix that and run zae addon add again, or discard the plan: zae addon remove %s --url %s --yes", n, reason, n, base)
		return exitcode.Failed
	}
	if !*yes {
		okay, cerr := s.confirm(fmt.Sprintf("install %s (chart %s) on %s?", n, planChart(a), base))
		if errors.Is(cerr, errInterrupted) {
			fmt.Fprintln(stdout, "interrupted — "+stays)
			return s.exitCode()
		}
		if !okay {
			fmt.Fprintln(stdout, "not installed — "+stays)
			return exitcode.Failed
		}
	}
	inst, err := install(s, c, n)
	if err != nil {
		return failOr(s, err, fmt.Sprintf("interrupted — zae cannot tell whether %s was installed; zae addon status %s --url %s shows it", n, n, base))
	}
	if !*wait {
		fmt.Fprintf(stdout, "installing %s — follow it with zae addon status %s --url %s\n", n, n, base)
		return exitcode.OK
	}
	return waitReady(s, c, n, inst.Generation, *timeout)
}

// valuesFrom assembles values: --values (or start, when there is none), then
// every --set in order.
func valuesFrom(src string, start map[string]any, sets []string) (map[string]any, error) {
	values := start
	if src != "" {
		v, err := readValues(src)
		if err != nil {
			return nil, fmt.Errorf("--values: %w", err)
		}
		values = v
	}
	if values == nil {
		values = map[string]any{}
	}
	for _, s := range sets {
		path, raw, err := assignment("set", s)
		if err != nil {
			return nil, err
		}
		if err := setPath(values, path, setValue(raw)); err != nil {
			return nil, fmt.Errorf("--set: %w", err)
		}
	}
	return values, nil
}

// noValueIsSecret refuses a path given both as a plain value and as a secret
// input: the secret would be readable in the addon resource as well.
func noValueIsSecret(values map[string]any, secrets map[string]string) error {
	for path := range secrets {
		if _, clash := lookup(values, path); clash {
			return fmt.Errorf("%s is given as a value and as a secret input — a secret input belongs only in the secret flags", path)
		}
	}
	return nil
}

// install clears suspend on the addon's current plan and returns the
// generation that made.
func install(s *session, c *client, name string) (accepted, error) {
	var acc accepted
	err := c.do(s.ctx, "install "+name, http.MethodPost, namePath(name)+"/install", nil, &acc,
		fmt.Sprintf("%s no longer has an addon %q", c.base, name))
	if ae, ok := asAPIError(err); ok && ae.status == http.StatusConflict {
		ae.msg += " — the portal installs only a current plan without refusals or values errors; zae addon status " + name + " shows the plan it has now"
	}
	return acc, err
}

// list prints every installed addon.
func list(args []string) int {
	fs := flagSet("list")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	asJSON := fs.Bool("json", false, "print the portal's answer as JSON")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return usageErr("zae addon list --url https://… takes no arguments")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	var raw json.RawMessage
	if err := newClient(base).do(context.Background(), "list addons", http.MethodGet, addonsPath, nil, &raw, ""); err != nil {
		return fail(err)
	}
	if *asJSON {
		fmt.Fprintln(stdout, strings.TrimSpace(string(raw)))
		return exitcode.OK
	}
	var rows []Listed
	if err := json.Unmarshal(raw, &rows); err != nil {
		errf("undetermined: list addons: %s answered with JSON that is not a list of addons (%v)", base, err)
		return exitcode.Undetermined
	}
	renderList(stdout, rows)
	return exitcode.OK
}

// status prints one chart addon: phase, what it asks for, what runs,
// components, registration and plan.
func status(args []string) int {
	fs := flagSet("status")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	asJSON := fs.Bool("json", false, "print the portal's answer as JSON")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageErr("zae addon status <name> --url https://… takes one addon name")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	name := pos[0]
	if !validName(name) {
		return usageErr("%q is not a valid addon name", name)
	}
	c := newClient(base)
	ctx := context.Background()
	if *asJSON {
		var raw json.RawMessage
		if err := c.do(ctx, "status of "+name, http.MethodGet, namePath(name), nil, &raw,
			fmt.Sprintf("%s has no addon %q installed from a chart", base, name)); err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, strings.TrimSpace(string(raw)))
		return exitcode.OK
	}
	a, gerr := c.get(ctx, name)
	if gerr != nil {
		return fail(gerr)
	}
	renderStatus(stdout, a)
	return exitcode.OK
}

// remove deletes a chart addon. The operator's objects go with the resource
// by garbage collection. The addon's values Secret and the Secret with its
// generated values go too — or, with --keep-values, both stay.
func remove(args []string) int {
	s := newSession()
	defer s.close()

	fs := flagSet("remove")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	keep := fs.Bool("keep-values", false, "keep the addon's values Secret and its generated values, for a later install")
	yes := fs.Bool("yes", false, "remove without asking")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageErr("zae addon remove <name> --url https://… takes one addon name")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	name := pos[0]
	if !validName(name) {
		return usageErr("%q is not a valid addon name", name)
	}
	if !*yes && !canAsk() {
		return usageErr("stdin is not a terminal, so zae will not ask before removing — add --yes")
	}
	c := newClient(base)
	a, gerr := c.get(s.ctx, name)
	if gerr != nil {
		return failOr(s, gerr, "")
	}
	fmt.Fprintf(stdout, "%s — %s\n", name, describe(a))
	if len(a.Components) > 0 {
		fmt.Fprintf(stdout, "  removing deletes everything its chart applied: %s\n", componentsLine(a.Components))
	}
	kept := fmt.Sprintf("its values Secrets zaentrum-addon-%s-values-… (the secret inputs) and zaentrum-addon-%s-generated (the values the operator generated)", name, name)
	if *keep {
		fmt.Fprintf(stdout, "  kept for a later install: %s — add the addon again with --secret-ref to use them\n", kept)
	} else {
		fmt.Fprintf(stdout, "  deleted with it: %s — a new install generates new values\n", kept)
	}
	if !*yes {
		okay, cerr := s.confirm(fmt.Sprintf("remove %s from %s?", name, base))
		if errors.Is(cerr, errInterrupted) {
			fmt.Fprintln(stdout, "interrupted — nothing removed")
			return s.exitCode()
		}
		if !okay {
			fmt.Fprintln(stdout, "nothing removed")
			return exitcode.Failed
		}
	}
	q := url.Values{"keepValues": {fmt.Sprint(*keep)}}
	var r removal
	if err := c.do(s.ctx, "remove "+name, http.MethodDelete, namePath(name)+"?"+q.Encode(), nil, &r,
		fmt.Sprintf("%s has no addon %q installed from a chart", base, name)); err != nil {
		return failOr(s, err, fmt.Sprintf("interrupted — zae cannot tell whether %s was removed; zae addon status %s --url %s shows it", name, name, base))
	}
	for _, w := range r.Warnings {
		errf("warning: %s", w)
	}
	switch {
	case r.KeptValues:
		fmt.Fprintf(stdout, "removed %s — kept %s, labelled zaentrum.io/keep=true\n", name, kept)
	case !r.Resource:
		fmt.Fprintf(stdout, "removed what was left of %s: its resource was already gone\n", name)
	default:
		fmt.Fprintf(stdout, "removed %s — Kubernetes garbage collection deletes what its chart applied\n", name)
	}
	return exitcode.OK
}

// planRefusal says why a plan must not be installed, or "" when it may be.
func planRefusal(a *Addon) string {
	var why []string
	if a.Plan != nil {
		if n := len(a.Plan.Violations); n > 0 {
			why = append(why, count(n, "object refused by the guardrails", "objects refused by the guardrails"))
		}
		if n := len(a.Plan.ValuesErrors); n > 0 {
			why = append(why, count(n, "values error", "values errors"))
		}
	}
	if len(why) == 0 && a.Phase != PhasePlanned {
		s := "the operator reports " + phaseOr(a.Phase)
		if a.Message != "" {
			s += ": " + a.Message
		}
		why = append(why, s)
	}
	if len(why) == 0 && a.Plan == nil {
		why = append(why, "the operator reported no plan")
	}
	return strings.Join(why, " and ")
}

// describe is one line about an addon's state.
func describe(a *Addon) string {
	s := "phase " + phaseOr(a.Phase)
	if a.installed() {
		s = "runs " + chartLine(a.LastAppliedChart) + ", " + s
	} else {
		s = "never installed, " + s
	}
	if a.Suspended {
		s += ", suspended"
	}
	return s
}

func planChart(a *Addon) string {
	if a.Plan == nil {
		return a.Name
	}
	return strings.TrimSpace(a.Plan.Chart.Name + " " + a.Plan.Chart.Version)
}

func lastSeen(a *Addon) string {
	if a == nil {
		return "no answer"
	}
	s := "last phase " + phaseOr(a.Phase)
	if a.Message != "" {
		s += ": " + a.Message
	}
	return s
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
