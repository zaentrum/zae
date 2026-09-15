package addon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// restoreTimeout bounds putting an addon back, which runs after the command's
// own context may already have ended.
const restoreTimeout = 30 * time.Second

// upgrade changes an addon's chart, values or secret inputs, plan first.
//
// The PATCH always suspends the addon, so even a values change is planned
// before it applies; the plan zae shows is the one the operator made for the
// generation that PATCH produced. Whatever does not end in an install —
// refused, declined, no plan in time, Ctrl-C or SIGTERM — puts the addon back
// to the spec zae read before it wrote: chart reference, version and digest,
// values and suspension. It re-reads first and leaves the addon alone when
// someone else has changed it since. Secret inputs cannot be put back: zae
// never reads a secret value, so it says which ones stay as written.
func upgrade(args []string) int {
	s := newSession()
	defer s.close()

	fs := flagSet("upgrade")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	version := fs.String("version", "", "the chart version to move to: the tag of an oci:// chart")
	chartFlag := fs.String("chart", "", "a new chart reference: oci://… or an https:// archive link")
	digest := fs.String("digest", "", "sha256:… that the chart archive must match")
	valuesSrc := fs.String("values", "", "replace the values with one JSON object: a file, or - for stdin")
	var sets multiFlag
	fs.Var(&sets, "set", "path=value: change one value, over the current values (repeatable)")
	var secrets []secretArg
	secretValuesSrc := secretFlags(fs, &secrets)
	var clears multiFlag
	fs.Var(&clears, "clear-secret", "path: remove a secret input (repeatable)")
	yes := fs.Bool("yes", false, "install without asking")
	wait := fs.Bool("wait", false, "after installing, wait until the addon is Ready and registered")
	timeout := fs.Duration("timeout", defaultTimeout, "how long each wait lasts: for the plan, and with --wait for Ready")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageErr("zae addon upgrade <name> --url https://… takes one addon name")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	name := pos[0]
	if !validName(name) {
		return usageErr("%q is not a valid addon name", name)
	}
	changesValues := *valuesSrc != "" || len(sets) > 0
	changesSecrets := *secretValuesSrc != "" || len(secrets) > 0 || len(clears) > 0
	if *version == "" && *chartFlag == "" && *digest == "" && !changesValues && !changesSecrets {
		return usageErr("upgrade needs something to change: --version, --chart, --digest, values or secret inputs")
	}
	patch := map[string]any{"suspend": true} // plan first — a values change alone would apply at once
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
	} else if *version != "" {
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
	if err := checkSecretArgs(secrets, clears); err != nil {
		return usageErr("%v", err)
	}
	if code := checkStdin(*yes, *valuesSrc, *secretValuesSrc, prompted(secrets), "upgrading"); code != exitcode.OK {
		return code
	}

	c := newClient(base)
	before, err := c.get(s.ctx, name)
	if err != nil {
		return failOr(s, err, "interrupted — nothing changed")
	}
	spec := before.spec()
	if *chartFlag == "" && *version != "" && spec != nil && strings.HasPrefix(spec.Ref, "https://") {
		return usageErr("%s installs an https archive, which is one version — upgrade it with --chart and the link to the new archive", name)
	}
	var newValues map[string]any
	if changesValues {
		newValues, err = valuesFrom(*valuesSrc, before.valuesMap(), sets)
		if err != nil {
			return usageErr("%v", err)
		}
		if !sameJSON(newValues, before.Values) {
			patch["values"] = newValues
		}
	}
	secretValues, err := s.secretInputs(*secretValuesSrc, secrets)
	if errors.Is(err, errInterrupted) {
		return s.exitCode()
	}
	if err != nil {
		return usageErr("%v", err)
	}
	if err := noValueIsSecret(newValues, secretValues); err != nil {
		return usageErr("%v", err)
	}
	if len(secretValues) > 0 {
		patch["secretValues"] = secretValues
	}
	if len(clears) > 0 {
		patch["clearSecrets"] = []string(clears)
	}
	if !asksForChange(patch, spec, before) {
		fmt.Fprintf(stdout, "%s already asks for %s with these values — nothing to upgrade\n", name, chartLine(spec))
		return exitcode.OK
	}

	var acc accepted
	if err := c.do(s.ctx, "upgrade "+name, http.MethodPatch, namePath(name), patch, &acc,
		fmt.Sprintf("%s has no addon %q installed from a chart", base, name)); err != nil {
		return failOr(s, err, fmt.Sprintf("interrupted — zae cannot tell whether %s was changed; zae addon status %s --url %s shows it", name, name, base))
	}
	if acc.Generation != 0 && acc.Generation == before.Generation {
		fmt.Fprintf(stdout, "%s already asks for %s — nothing to upgrade\n", name, chartLine(spec))
		return exitcode.OK
	}
	u := &upgrading{s: s, c: c, name: name, before: before, spec: spec, patch: patch, wrote: acc.Generation}

	fmt.Fprintf(stdout, "planning %s at %s on %s …\n", name, describeTarget(acc.Chart, patch), base)
	a, done, perr := c.poll(s, name, *timeout, 0, planFor(name, acc.Generation), nil)
	var changed *changedError
	switch {
	case errors.Is(perr, errInterrupted):
		fmt.Fprintln(stdout, "interrupted — putting it back")
		u.restore(false)
		return s.exitCode()
	case errors.As(perr, &changed):
		return fail(perr) // someone else's change: not zae's to undo
	case perr != nil:
		code := fail(perr)
		if ae, ok := asAPIError(perr); !ok || !ae.notFound {
			u.restore(false)
		}
		return code
	case !done:
		errf("failed: no plan for %s after %s (%s) — is the operator running?", name, *timeout, lastSeen(a))
		u.restore(false)
		return exitcode.Failed
	}
	renderPlan(stdout, a, false)
	if reason := planRefusal(a); reason != "" {
		errf("failed: %s is not upgraded: %s", name, reason)
		u.restore(false)
		return exitcode.Failed
	}
	if !*yes {
		verb := "upgrade %s to chart %s on %s?"
		if !before.installed() {
			verb = "install %s (chart %s) on %s?"
		}
		okay, cerr := s.confirm(fmt.Sprintf(verb, name, planChart(a), base))
		if errors.Is(cerr, errInterrupted) {
			fmt.Fprintln(stdout, "interrupted — putting it back")
			u.restore(false)
			return s.exitCode()
		}
		if !okay {
			fmt.Fprintln(stdout, "not upgraded")
			u.restore(false)
			return exitcode.Failed
		}
	}
	inst, err := install(s, c, name)
	if err != nil {
		if s.interrupted() {
			fmt.Fprintln(stdout, "interrupted while installing")
			u.restore(true)
			return s.exitCode()
		}
		code := fail(err)
		u.restore(false)
		return code
	}
	if !*wait {
		fmt.Fprintf(stdout, "upgrading %s — follow it with zae addon status %s --url %s\n", name, name, base)
		return exitcode.OK
	}
	return waitReady(s, c, name, inst.Generation, *timeout)
}

// upgrading is what putting an upgrade back needs.
type upgrading struct {
	s      *session
	c      *client
	name   string
	before *Addon
	spec   *Chart
	patch  map[string]any
	wrote  int64 // the generation zae's PATCH produced
}

// restore puts the addon back to the spec read before the upgrade wrote. It
// re-reads first: a generation beyond zae's own write means someone else — or,
// after an interrupted install, the install itself — has changed the addon,
// and then nothing is undone.
func (u *upgrading) restore(afterInstall bool) {
	ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	defer cancel()
	now, err := u.c.get(ctx, u.name)
	if err != nil {
		errf("failed: could not read %s to put it back: %v", u.name, err)
		return
	}
	if u.wrote > 0 && now.Generation != u.wrote {
		if afterInstall {
			fmt.Fprintf(stdout, "the install went through before the interrupt — %s is upgrading; zae addon status %s --url %s follows it\n", u.name, u.name, u.c.base)
			return
		}
		errf("warning: %s changed after zae's upgrade (generation %d, zae wrote %d) — someone else is changing it, so zae does not put it back", u.name, now.Generation, u.wrote)
		return
	}
	back := map[string]any{"suspend": u.before.Suspended}
	if u.spec != nil {
		back["chart"], back["version"], back["digest"] = u.spec.Ref, u.spec.Version, u.spec.Digest
	}
	if _, changed := u.patch["values"]; changed {
		if len(bytes.TrimSpace(u.before.Values)) == 0 {
			back["values"] = nil
		} else {
			back["values"] = u.before.Values
		}
	}
	if err := u.c.do(ctx, "restore "+u.name, http.MethodPatch, namePath(u.name), back, nil, ""); err != nil {
		errf("%s", err.Error())
		errf("failed: %s could not be put back: it stays suspended at the new spec and applies nothing, while what it runs keeps running (%s)", u.name, chartLine(u.before.LastAppliedChart))
		return
	}
	fmt.Fprintf(stdout, "%s is back at %s\n", u.name, chartLine(u.spec))
	var secretPaths []string
	if sv, ok := u.patch["secretValues"].(map[string]string); ok {
		for p := range sv {
			secretPaths = append(secretPaths, p)
		}
	}
	if cl, ok := u.patch["clearSecrets"].([]string); ok {
		secretPaths = append(secretPaths, cl...)
	}
	if len(secretPaths) > 0 {
		sort.Strings(secretPaths)
		errf("warning: the secret inputs this upgrade set or cleared stay as written (%s) — zae never reads secret values, so it cannot put them back", strings.Join(secretPaths, ", "))
	}
}

// asksForChange reports whether a PATCH would change the addon's spec beyond
// suspending it: a running addon must not be suspended, re-planned and
// re-installed only to arrive where it already is.
func asksForChange(patch map[string]any, spec *Chart, before *Addon) bool {
	for _, k := range []string{"values", "secretValues", "clearSecrets"} {
		if _, ok := patch[k]; ok {
			return true
		}
	}
	if spec == nil {
		return true
	}
	if ref, ok := patch["chart"].(string); ok && ref != spec.Ref {
		return true
	}
	if v, ok := patch["version"].(string); ok && v != spec.Version {
		return true
	}
	if d, ok := patch["digest"].(string); ok && d != spec.Digest {
		return true
	}
	// Nothing about the chart or its inputs changes; a plan-only addon is
	// suspended already, so the PATCH would be a no-op for it as well.
	return false
}

// sameJSON reports whether values v, as zae would send them, equal the raw
// values the addon has: key order, spacing and number spelling aside.
func sameJSON(v map[string]any, raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return len(v) == 0
	}
	a, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return canonical(a) == canonical(raw)
}

func canonical(b []byte) string {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return string(b)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(b)
	}
	return string(out)
}

// describeTarget names what an upgrade asked for: the chart the portal says
// the resource now names, or what the PATCH carried.
func describeTarget(chart *Chart, patch map[string]any) string {
	if chart != nil && chart.Ref != "" {
		return chartLine(chart)
	}
	var parts []string
	for _, k := range []string{"chart", "version", "digest"} {
		if v, ok := patch[k].(string); ok {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return "new values"
	}
	return strings.Join(parts, " ")
}
