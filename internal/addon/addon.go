// Package addon is `zae addon`: adding, inspecting, upgrading and removing
// addons the platform installs from a Helm chart (ADR-0011).
//
// Unlike `zae <service> <command>`, these are STATIC commands. They drive the
// portal's admin API for chart addons, which an instance has whether or not
// any addon is installed — and nothing here knows any addon: an addon is a
// chart reference and a name.
//
// Plan first, always. add and upgrade leave the addon suspended — the
// operator plans it and applies nothing — print the plan, and install only
// when the admin answers yes or passed --yes. A plan the guardrails refused,
// or whose values do not validate, is never installed. zae never prompts on a
// stdin that is not a terminal: such an invocation needs --yes, and without it
// fails as usage before anything is written.
package addon

import (
	"bufio"
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
      [--values FILE|-] [--set path=value]… [--set-secret path=value]… [--yes] [--wait] [--timeout 5m]
  zae addon list --url https://… [--json]
  zae addon status <name> --url https://… [--json]
  zae addon upgrade <name> --url https://… (--version V | --chart REF) [--digest sha256:…] [--yes] [--wait] [--timeout 5m]
  zae addon remove <name> --url https://… [--keep-values] [--yes]

<chart> is oci://registry/path/chart with --version (or :tag), or an https://
link to a chart archive. add and upgrade show the operator's plan — chart,
workloads, images, ports, refusals, missing inputs — and ask before installing.
--yes skips the question; without a terminal on stdin it is required.

--values takes one JSON object, from a file or - for stdin (then --yes too).
--set path=value sets one value: JSON when it parses as JSON, otherwise a
string ('tag="1.10"' forces a string). --set-secret path=value sets a secret
input: it is stored in a Secret and never shown again.

Needs the platform's admin role: ZAE_TOKEN carries the bearer.
Exit codes: 0 done · 1 refused, failed, declined or not Ready in time · 2 usage ·
3 no such addon, or the instance has no addon chart API · 4 undetermined ·
5 forbidden.
`)
}

func errf(format string, a ...any) { fmt.Fprintf(stderr, "zae: "+format+"\n", a...) }

// fail prints a call's error and returns its exit code.
func fail(err *apiError) int {
	errf("%s", err.msg)
	return err.code
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

func namePath(name string) string { return chartsPath + "/" + url.PathEscape(name) }

// add: POST the addon (suspended), wait for the plan, print it, confirm,
// install, and with --wait follow it to Ready.
func add(args []string) int {
	fs := flagSet("add")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	name := fs.String("name", "", "addon name; default: the chart's name from the reference")
	version := fs.String("version", "", "chart version: the tag of an oci:// chart")
	digest := fs.String("digest", "", "sha256:… that the chart archive must match")
	valuesSrc := fs.String("values", "", "values as one JSON object: a file, or - for stdin")
	var sets, secrets multiFlag
	fs.Var(&sets, "set", "path=value: one value, JSON when it parses as JSON, else a string (repeatable)")
	fs.Var(&secrets, "set-secret", "path=value: one secret input, stored in a Secret and never shown (repeatable)")
	yes := fs.Bool("yes", false, "install without asking")
	wait := fs.Bool("wait", false, "after installing, wait until the addon is Ready")
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
	}
	if !validName(n) {
		return usageErr("%q is not a valid addon name (lowercase letters, digits and -, at most %d characters) — pass --name", n, maxNameLen)
	}
	if *timeout <= 0 {
		return usageErr("--timeout must be positive")
	}
	// Everything that can fail as usage is checked before anything is written.
	if !*yes {
		if *valuesSrc == "-" {
			return usageErr("--values - reads stdin, which the confirmation needs too — add --yes")
		}
		if !canAsk() {
			return usageErr("stdin is not a terminal, so zae will not ask before installing — add --yes")
		}
	}
	values, secretValues, err := inputsFrom(*valuesSrc, sets, secrets)
	if err != nil {
		return usageErr("%v", err)
	}

	c := newClient(base)
	before, gerr := c.get(n)
	switch {
	case gerr == nil && (before.installed() || !before.Suspended):
		errf("failed: %s is already installed on %s (%s) — change its chart with zae addon upgrade %s, or remove it first", n, base, describe(before), n)
		return exitcode.Failed
	case gerr == nil:
		fmt.Fprintf(stdout, "%s is planned but not installed — its plan is replaced\n", n)
	case gerr.notFound && !gerr.noAPI:
		before = nil
	default:
		return fail(gerr)
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
	var created struct {
		Name string `json:"name"`
	}
	if err := c.do("add "+n, http.MethodPost, chartsPath, body, &created, ""); err != nil {
		return fail(err)
	}
	if created.Name != "" {
		n = created.Name
	}
	fmt.Fprintf(stdout, "planning %s from %s on %s …\n", n, chart, base)

	a, done, perr := c.poll(n, *timeout, notFoundGrace, planAnswered(before, chart.version), nil)
	if perr != nil {
		return fail(perr)
	}
	if !done {
		errf("failed: no plan for %s after %s (%s) — is the operator running, and does it reconcile ZaentrumAddon resources?", n, *timeout, lastSeen(a))
		return exitcode.Failed
	}
	renderPlan(stdout, a, false)
	if reason := planRefusal(a); reason != "" {
		errf("failed: %s is not installed: %s. Fix that and run zae addon add again, or discard the plan: zae addon remove %s --url %s --yes", n, reason, n, base)
		return exitcode.Failed
	}
	if !*yes && !confirm(fmt.Sprintf("install %s (%s) on %s?", n, planChart(a), base)) {
		fmt.Fprintf(stdout, "not installed — %s stays planned and applies nothing; discard it with zae addon remove %s --url %s --yes\n", n, n, base)
		return exitcode.Failed
	}
	if err := install(c, n); err != nil {
		return fail(err)
	}
	if !*wait {
		fmt.Fprintf(stdout, "installing %s — follow it with zae addon status %s --url %s\n", n, n, base)
		return exitcode.OK
	}
	return waitReady(c, n, *timeout, nil)
}

// inputsFrom assembles the values and the secret inputs from --values, --set
// and --set-secret, in that order.
func inputsFrom(src string, sets, secrets []string) (map[string]any, map[string]string, error) {
	values := map[string]any{}
	if src != "" {
		v, err := readValues(src)
		if err != nil {
			return nil, nil, fmt.Errorf("--values: %w", err)
		}
		values = v
	}
	for _, s := range sets {
		path, raw, err := assignment("set", s)
		if err != nil {
			return nil, nil, err
		}
		if err := setPath(values, path, setValue(raw)); err != nil {
			return nil, nil, fmt.Errorf("--set: %w", err)
		}
	}
	secretValues := map[string]string{}
	for _, s := range secrets {
		path, raw, err := assignment("set-secret", s)
		if err != nil {
			return nil, nil, err
		}
		if _, clash := lookup(values, path); clash {
			return nil, nil, fmt.Errorf("%s is given as a value and as a secret input — a secret input belongs only in --set-secret", path)
		}
		secretValues[path] = raw
	}
	return values, secretValues, nil
}

func install(c *client, name string) *apiError {
	err := c.do("install "+name, http.MethodPost, namePath(name)+"/install", nil, nil,
		fmt.Sprintf("%s no longer has an addon %q", c.base, name))
	if err != nil && err.status == http.StatusConflict {
		err.msg += " — the portal installs only a current plan without refusals or values errors; zae addon status " + name + " shows the plan it has now"
	}
	return err
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
	if err := newClient(base).do("list addons", http.MethodGet, addonsPath, nil, &raw, ""); err != nil {
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

// status prints one chart addon: phase, what runs, components, plan.
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
	if *asJSON {
		var raw json.RawMessage
		if err := c.do("status of "+name, http.MethodGet, namePath(name), nil, &raw,
			fmt.Sprintf("%s has no addon %q installed from a chart", base, name)); err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, strings.TrimSpace(string(raw)))
		return exitcode.OK
	}
	a, gerr := c.get(name)
	if gerr != nil {
		return fail(gerr)
	}
	renderStatus(stdout, a)
	return exitcode.OK
}

// upgrade: PATCH the chart (the portal suspends the addon to plan it), wait
// for the plan with its changes, confirm, install. A running addon that is
// not upgraded — refused, declined, no plan in time — is put back to the
// chart it runs, so it is never left suspended at a chart it does not run.
func upgrade(args []string) int {
	fs := flagSet("upgrade")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	version := fs.String("version", "", "the chart version to move to: the tag of an oci:// chart")
	chartFlag := fs.String("chart", "", "a new chart reference: oci://… or an https:// archive link")
	digest := fs.String("digest", "", "sha256:… that the new chart archive must match")
	yes := fs.Bool("yes", false, "install without asking")
	wait := fs.Bool("wait", false, "after installing, wait until the addon is Ready")
	timeout := fs.Duration("timeout", defaultTimeout, "how long each wait lasts: for the plan, and with --wait for Ready")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageErr("zae addon upgrade <name> --version V --url https://… takes one addon name")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	name := pos[0]
	if !validName(name) {
		return usageErr("%q is not a valid addon name", name)
	}
	if *version == "" && *chartFlag == "" {
		return usageErr("upgrade needs --version, or --chart with a new chart reference")
	}
	patch := map[string]any{}
	if *chartFlag != "" {
		ref, err := parseChart(*chartFlag, *version)
		if err != nil {
			return usageErr("%v", err)
		}
		patch["chart"] = ref.ref
		if ref.oci && ref.version != "" {
			patch["version"] = ref.version
		} else if *version != "" {
			errf("note: --version does not apply to an https archive, which is one version — ignoring it")
		}
	} else {
		patch["version"] = *version
	}
	if *digest != "" {
		if !digestRe.MatchString(*digest) {
			return usageErr("--digest %q is not sha256: followed by 64 hex digits", *digest)
		}
		patch["digest"] = *digest
	}
	if *timeout <= 0 {
		return usageErr("--timeout must be positive")
	}
	if !*yes && !canAsk() {
		return usageErr("stdin is not a terminal, so zae will not ask before upgrading — add --yes")
	}

	c := newClient(base)
	before, gerr := c.get(name)
	if gerr != nil {
		return fail(gerr)
	}
	if *chartFlag == "" && before.installed() && strings.HasPrefix(before.LastAppliedChart.Ref, "https://") {
		return usageErr("%s runs an https archive, which is one version — upgrade it with --chart and the link to the new archive", name)
	}
	running := before.installed() && !before.Suspended
	wantVersion, _ := patch["version"].(string)
	wantRef, _ := patch["chart"].(string)
	if running && runs(before.LastAppliedChart, wantRef, wantVersion, *digest) {
		fmt.Fprintf(stdout, "%s already runs %s — nothing to upgrade\n", name, chartLine(before.LastAppliedChart))
		return exitcode.OK
	}
	if err := c.do("upgrade "+name, http.MethodPatch, namePath(name), patch, nil,
		fmt.Sprintf("%s has no addon %q installed from a chart", base, name)); err != nil {
		return fail(err)
	}

	restore := func() {
		if !running {
			return
		}
		last := before.LastAppliedChart
		back := map[string]any{"suspend": false}
		for key, was := range map[string]string{"chart": last.Ref, "version": last.Version, "digest": last.Digest} {
			if _, changed := patch[key]; changed {
				back[key] = was
			}
		}
		if err := c.do("restore "+name, http.MethodPatch, namePath(name), back, nil, ""); err != nil {
			errf("%s", err.msg)
			errf("failed: %s could not be put back: it is suspended at the new chart and applies nothing, while its workloads still run %s", name, chartLine(last))
			return
		}
		fmt.Fprintf(stdout, "%s is back at %s — nothing it runs was changed\n", name, chartLine(last))
	}

	fmt.Fprintf(stdout, "planning %s at %s on %s …\n", name, describePatch(patch), base)
	a, done, perr := c.poll(name, *timeout, 0, planAnswered(before, wantVersion), nil)
	if perr != nil {
		code := fail(perr)
		if !perr.notFound {
			restore()
		}
		return code
	}
	if !done {
		errf("failed: no plan for %s after %s (%s) — is the operator running?", name, *timeout, lastSeen(a))
		restore()
		return exitcode.Failed
	}
	renderPlan(stdout, a, false)
	if reason := planRefusal(a); reason != "" {
		errf("failed: %s is not upgraded: %s", name, reason)
		restore()
		return exitcode.Failed
	}
	if !*yes && !confirm(fmt.Sprintf("upgrade %s to %s on %s?", name, planChart(a), base)) {
		fmt.Fprintln(stdout, "not upgraded")
		if !running {
			fmt.Fprintf(stdout, "%s stays planned at the new chart and applies nothing\n", name)
		}
		restore()
		return exitcode.Failed
	}
	if err := install(c, name); err != nil {
		code := fail(err)
		restore()
		return code
	}
	if !*wait {
		fmt.Fprintf(stdout, "upgrading %s — follow it with zae addon status %s --url %s\n", name, name, base)
		return exitcode.OK
	}
	return waitReady(c, name, *timeout, func(a *Addon) bool {
		return a.installed() && runs(a.LastAppliedChart, wantRef, wantVersion, "")
	})
}

// runs: the applied chart is the one asked for (empty fields ask nothing).
func runs(last *Chart, ref, version, digest string) bool {
	return last != nil && (ref == "" || last.Ref == ref) && (version == "" || last.Version == version) &&
		(digest == "" || last.Digest == digest)
}

// remove deletes a chart addon. The operator's objects go with the resource
// by garbage collection; the values Secret too, unless --keep-values.
func remove(args []string) int {
	fs := flagSet("remove")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	keep := fs.Bool("keep-values", false, "keep the Secret holding the addon's secret inputs, for a later install")
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
	a, gerr := c.get(name)
	if gerr != nil {
		return fail(gerr)
	}
	fmt.Fprintf(stdout, "%s — %s\n", name, describe(a))
	if len(a.Components) > 0 {
		fmt.Fprintf(stdout, "  removing deletes everything its chart applied: %s\n", componentsLine(a.Components))
	}
	if *keep {
		fmt.Fprintf(stdout, "  its values Secret zaentrum-addon-%s-values is kept\n", name)
	} else {
		fmt.Fprintln(stdout, "  its values and secret inputs are deleted with it")
	}
	fmt.Fprintln(stdout, "  values the operator generated for it are deleted with it; a new install generates new ones")
	if !*yes && !confirm(fmt.Sprintf("remove %s from %s?", name, base)) {
		fmt.Fprintln(stdout, "nothing removed")
		return exitcode.Failed
	}
	q := url.Values{"keepValues": {fmt.Sprint(*keep)}}
	if err := c.do("remove "+name, http.MethodDelete, namePath(name)+"?"+q.Encode(), nil, nil,
		fmt.Sprintf("%s has no addon %q installed from a chart", base, name)); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "removed %s — Kubernetes garbage collection deletes what its chart applied\n", name)
	return exitcode.OK
}

// poll reads the addon until done accepts it or timeout passes, and returns
// the last document read; done is false on timeout. changed is called when the
// phase or a component's readiness moves. While waiting, no answer or a
// failing portal is asked again — a restarting portal-api is not a verdict —
// and so is a 404 within grace of the write that created the addon.
func (c *client) poll(name string, timeout, grace time.Duration, done func(*Addon) bool, changed func(*Addon)) (*Addon, bool, *apiError) {
	start := time.Now()
	var last *Addon
	var lastErr *apiError
	seen := ""
	for {
		a, err := c.get(name)
		switch {
		case err == nil:
			last, lastErr = a, nil
			if key := progressKey(a); changed != nil && key != seen {
				seen = key
				changed(a)
			}
			if done(a) {
				return a, true, nil
			}
		case err.transient, err.notFound && !err.noAPI && time.Since(start) < grace:
			lastErr = err
		default:
			return last, false, err
		}
		if time.Since(start) >= timeout {
			if last == nil && lastErr != nil {
				return nil, false, lastErr
			}
			return last, false, nil
		}
		time.Sleep(pollInterval)
	}
}

func progressKey(a *Addon) string {
	return fmt.Sprintf("%s|%t|%s", a.Phase, a.Suspended, componentsLine(a.Components))
}

// planAnswered accepts a document that carries the plan for the spec zae just
// wrote, not a status left over from before the write. The v1 document has no
// generation, so zae uses what it has: a generation pair when the portal sends
// one; the suspension and the phase, which only planning reaches (a running
// addon reads Ready until the operator plans the change); the version it asked
// for, which a stale plan does not name; and a Failed that was there before
// the write is not taken for the answer unless the plan names that version.
func planAnswered(before *Addon, version string) func(*Addon) bool {
	return func(a *Addon) bool {
		if !a.Suspended {
			return false
		}
		switch a.Phase {
		case PhasePlanned, PhasePlanFailed, PhaseFailed:
		default:
			return false
		}
		if a.Generation > 0 {
			return a.current()
		}
		named := a.Plan != nil && a.Plan.Chart.Version != ""
		if version != "" && named && a.Plan.Chart.Version != version {
			return false
		}
		if before != nil && before.Phase == PhaseFailed && a.Phase == PhaseFailed && (version == "" || !named) {
			return false
		}
		return true
	}
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

// waitReady follows an install until the addon is Ready, printing progress as
// it moves. matches, when set, refuses a Ready left over from before the
// install — an upgrade's previous chart.
func waitReady(c *client, name string, timeout time.Duration, matches func(*Addon) bool) int {
	fmt.Fprintf(stdout, "waiting for %s to become Ready (timeout %s)\n", name, timeout)
	a, done, err := c.poll(name, timeout, 0, func(a *Addon) bool {
		return a.Phase == PhaseReady && !a.Suspended && a.current() && (matches == nil || matches(a))
	}, func(a *Addon) {
		if !a.Suspended { // still the plan the install has not reached yet
			fmt.Fprintf(stdout, "  %-12s%s\n", phaseOr(a.Phase), componentsLine(a.Components))
		}
	})
	if err != nil {
		return fail(err)
	}
	if !done {
		errf("failed: %s is not Ready after %s (%s) — zae addon status %s --url %s shows why", name, timeout, lastSeen(a), name, c.base)
		return exitcode.Failed
	}
	fmt.Fprintf(stdout, "%s is Ready\n", name)
	return exitcode.OK
}

// confirm asks on stdin. Anything but y or yes is no — an empty line and a
// closed stdin included — so a stray Enter never installs or removes.
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

// stdinIsTerminal reports whether a person can answer on stdin. Without a
// terminal library this checks for a character device — and rules out
// /dev/null, which is one too.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
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

func describePatch(patch map[string]any) string {
	var parts []string
	for _, k := range []string{"chart", "version", "digest"} {
		if v, ok := patch[k].(string); ok {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " ")
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
