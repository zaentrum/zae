package platform

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// controllerHeading opens the controller section of a status.
const controllerHeading = "the operator's controller"

// controllerUnreported is what an operator older than the field leaves zae
// able to say. Saying it beats silence: silence reads as "there is no
// controller", and where it is updated is the same either way.
const controllerUnreported = "not reported by this operator, so zae cannot say what version is in charge"

// renderStatus prints the platform on one screen: what it was asked to run,
// what it reports running, every workload beside it — and, last, the one piece
// of all this that the platform does not update: its own controller.
func renderStatus(w io.Writer, base string, c *Console) {
	op := c.Operator
	if c.direct() {
		renderDirect(w, base, c)
		return
	}
	fmt.Fprintf(w, "%s — the platform\n", base)
	label(w, "version", versionLine(op))
	label(w, "channel", dash(op.Channel))
	label(w, "update mode", updateModeLine(op))
	label(w, "phase", dash(op.Phase))
	label(w, "running", dash(op.CurrentVersion))
	label(w, "update", updateLine(op, base))
	label(w, "verified", verificationLine(op.Verification, now()))
	for _, f := range failingChecks(op.Verification) {
		fmt.Fprintf(w, "  %-13s%s\n", "", f)
	}
	if op.Hostname != "" {
		label(w, "host", op.Hostname)
	}
	if op.Note != "" {
		label(w, "note", op.Note)
	}

	fmt.Fprintln(w)
	renderWorkloads(w, c)
	fmt.Fprintln(w)
	renderController(w, controllerHeading, op.Controller)
}

// renderDirect prints an instance whose portal manages the workloads without
// an operator — direct mode, as the console calls it: there is no version,
// channel, verification or controller to show, only the workloads, which
// restart and scale act on directly.
func renderDirect(w io.Writer, base string, c *Console) {
	fmt.Fprintf(w, "%s — the platform · direct mode\n", base)
	note := strings.TrimSpace(c.Operator.Note)
	if note == "" {
		note = "no operator detected"
	}
	label(w, "operator", "none — "+note)
	fmt.Fprintf(w, "  %-13s%s\n", "", "restart and scale act on the Deployments directly; update and verify need an operator")
	fmt.Fprintln(w)
	renderWorkloads(w, c)
}

// verificationLine is the platform's last check of itself, on one line: how
// it came out, when, and what it verified —
//
//	passed 14/14 · 3 min ago · after the update to 1.5.0 (image set 3f9a1c0b2d4e)
//
// — or that it never ran, is running, could not run, or is switched off.
func verificationLine(v *Verification, at time.Time) string {
	switch {
	case v == nil:
		return "not reported — this portal-api predates the platform's verification of itself"
	case v.Note != "":
		return "unreadable — " + v.Note
	case v.off():
		return "off — the platform does not verify itself (spec.verification.enabled is false)"
	}
	var parts []string
	switch v.Result {
	case "":
		parts = append(parts, "never")
	case VerifyPassed:
		parts = append(parts, "passed "+v.score())
	case VerifyFailed:
		parts = append(parts, fmt.Sprintf("FAILED %d of %d checks", v.Failed, v.total()))
	case VerifyRunning:
		run := "running"
		if v.Job != "" {
			run += " (job " + v.Job + ")"
		}
		parts = append(parts, run)
	case VerifyError:
		parts = append(parts, "error — "+orNoReason(v.Message))
	case VerifySkipped:
		parts = append(parts, "skipped — "+orNoReason(v.Message))
	default:
		// A result this zae has not heard of is shown as it came.
		parts = append(parts, v.Result)
	}
	if v.Result != "" {
		if when := v.when(at); when != "" {
			parts = append(parts, when)
		}
		if what := v.what(); what != "" {
			parts = append(parts, what)
		}
	}
	switch {
	case v.PendingRequest == "" || v.PendingRequest == v.Request:
	case v.Result == "":
		parts[0] = "never — one is asked for and waits to run"
	default:
		parts = append(parts, "another run is asked for and waits")
	}
	return strings.Join(parts, " · ")
}

// score is "14/14", and what of the rest was not a failure.
func (v *Verification) score() string {
	s := fmt.Sprintf("%d/%d", v.Passed, v.total())
	if v.Warned > 0 {
		s += ", " + count(v.Warned, "warning", "warnings")
	}
	if v.Skipped > 0 {
		s += fmt.Sprintf(", %d skipped", v.Skipped)
	}
	return s
}

// when is how long ago the run ended — or, while it runs, began.
func (v *Verification) when(at time.Time) string {
	if v.Result == VerifyRunning || v.FinishedAt == "" {
		if v.StartedAt == "" {
			return ""
		}
		return "started " + ago(v.StartedAt, at)
	}
	return ago(v.FinishedAt, at)
}

// what is what the run verified: the platform version, the image set, and
// whether a rollout or a person asked for it.
func (v *Verification) what() string {
	ver := strings.TrimSpace(v.Version)
	set := ""
	if fp := strings.TrimSpace(v.Fingerprint); fp != "" {
		set = " (image set " + fp + ")"
	}
	switch {
	case v.Trigger == "update" && ver != "":
		return "after the update to " + ver + set
	case v.Trigger == "update":
		return "after an update" + set
	case v.Trigger == "request" && ver != "":
		return "asked for, at " + ver + set
	case v.Trigger == "request":
		return "asked for" + set
	case ver != "":
		return "at " + ver + set
	}
	return strings.TrimSpace(set)
}

// failingChecks are the lines a failed run is shown with: each check that
// failed, and what it found.
func failingChecks(v *Verification) []string {
	if v == nil || v.Result != VerifyFailed {
		return nil
	}
	var out []string
	for _, c := range v.Checks {
		if c.Status == "fail" {
			out = append(out, clipLine("✗ "+c.Name+" — "+dash(c.Detail), 120))
		}
	}
	if len(out) == 0 && v.Failed > 0 {
		out = append(out, "(the failing checks were not reported)")
	}
	return out
}

func orNoReason(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no reason given"
	}
	return strings.TrimSpace(s)
}

// ago names a time for a person: "just now", "3 min ago", "5 h ago", "2 days
// ago". A time zae cannot read is shown as it came.
func ago(ts string, at time.Time) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(ts))
	if err != nil {
		return "at " + ts
	}
	d := at.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// clipLine shortens s to n characters, marking the cut.
func clipLine(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// renderController prints the operator's own controller: what is in charge,
// how it got there, whether something newer exists — and then ONE line, which
// is the point of the whole section: the platform does not update this, and
// here is what does.
//
// It replaces a paragraph that said only the first half. "Not covered here"
// told a reader what zae would not do without ever telling them what runs, so
// the question it raised — *then what IS in charge?* — had to be answered
// somewhere else, with cluster access the reader may not have.
func renderController(w io.Writer, title string, c *Controller) {
	fmt.Fprintln(w, title)
	if !c.reported() {
		fmt.Fprintf(w, "  %s\n", controllerUnreported)
		fmt.Fprintln(w, updatedOutside(SourceUnknown))
		return
	}
	label(w, "version", dash(controllerVersion(c)))
	label(w, "image", dash(c.Image))
	label(w, "installed", installedBy(c.Source))
	label(w, "update", controllerUpdateLine(c))
	if strings.TrimSpace(c.ObservedAt) != "" {
		label(w, "observed", c.ObservedAt)
	}
	fmt.Fprintln(w, updatedOutside(c.Source))
}

// controllerVersion names the build: what the operator reported, else what its
// image says. The portal fills this in; the fallback is for anything else that
// answers this API and does not.
func controllerVersion(c *Controller) string {
	if v := strings.TrimSpace(c.Version); v != "" {
		return v
	}
	if v := tagOrDigest(c.Image); v != "" {
		return v
	}
	return SourceUnknown
}

// controllerUpdateLine says whether something newer was found — and never what
// to do about it, because that is the next line's job and it is not a command.
//
// What was found decides the wording, because two different facts arrive in
// one field. A version is a thing you can be on or not be on: "v0.5.0
// available" is a complete statement. A CHANNEL TAG is not — an install
// running :sha-19ea431 against a channel that serves :latest was rendered as
// "latest available", which reads as a version number and names nothing a
// reader can compare themselves against. The tag has not changed and never
// will; what it points at has. So that case says so instead.
func controllerUpdateLine(c *Controller) string {
	up := strings.TrimSpace(c.AvailableUpdate)
	switch {
	case up == "":
		// Not "none": an operator installed from a manifest may never look,
		// and "none offered" would be a claim about a check that never ran.
		return "none reported"
	case up == controllerVersion(c):
		return up + " — already running"
	case !versionLike(up):
		return fmt.Sprintf("the %q channel now serves a different image", up)
	}
	return up + " available"
}

// versionLike reports whether a value names a release rather than a moving
// tag: `v` and a digit (v0.5.0), or dotted numbers on their own (1.5.0,
// 2.0.0-rc1). Everything else — latest, stable, edge — is a name that outlives
// every image it points at, and is worded as one.
//
// Two numeric components are enough. A release is what a channel serves, so
// the question is only ever "is this a fixed point or a moving one", and
// nothing that reaches here is both.
func versionLike(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// A leading v and a digit is the release convention, and settles it.
	if (s[0] == 'v' || s[0] == 'V') && len(s) > 1 && s[1] >= '0' && s[1] <= '9' {
		return true
	}
	// A pre-release or build suffix does not change what the value is.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// installedBy names how the controller got here, for the "installed" row.
func installedBy(source string) string {
	switch strings.TrimSpace(source) {
	case SourceOLM:
		return "OLM — a subscription the cluster manages"
	case SourceManifest:
		return "its install manifest"
	case SourceAppliance:
		return "the appliance"
	case "", SourceUnknown:
		return "not reported"
	default:
		return source
	}
}

// updatedOutside is the one line the section exists for: what updates the
// controller, given how it was installed. Each source sends a reader somewhere
// different, which is why the operator reports the source at all — and none of
// them is a thing zae can do, which is why this is a sentence and not a flag.
func updatedOutside(source string) string {
	const prefix = "Updated outside the platform: "
	switch strings.TrimSpace(source) {
	case SourceOLM:
		return prefix + "approve the update in its OLM subscription."
	case SourceManifest:
		return prefix + "apply the pinned install manifest, usually through the deployment repository that holds it."
	case SourceAppliance:
		return prefix + "update the appliance — its own update carries the controller."
	case "", SourceUnknown:
		return prefix + "update it where it was installed from — an OLM subscription, the install manifest, or the appliance."
	default:
		return prefix + fmt.Sprintf("update it where it was installed from (%s).", source)
	}
}

// renderWorkloads prints the workload table: what the operator renders first,
// what was installed next to it after that, and what neither claims last.
func renderWorkloads(w io.Writer, c *Console) {
	if len(c.Instances) == 0 {
		if c.Error != "" {
			fmt.Fprintf(w, "workloads: the portal could not list them: %s\n", c.Error)
			return
		}
		fmt.Fprintln(w, "workloads: none")
		return
	}
	rows := append([]Workload(nil), c.Instances...)
	sort.SliceStable(rows, func(i, j int) bool { return groupRank(rows[i].Group) < groupRank(rows[j].Group) })

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tGROUP\tIMAGE\tREADY\tPHASE\tREASON")
	for _, wl := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\n", wl.Name, groupLabel(wl), imageTag(wl.Image),
			wl.ReadyReplicas, wl.DesiredReplicas, dash(wl.Phase), dash(wl.Reason))
	}
	tw.Flush()
	if c.Error != "" {
		fmt.Fprintf(w, "the portal could not list every workload: %s\n", c.Error)
	}
}

// groupRank orders the groups. A group this zae does not know goes before the
// unclaimed ones rather than among them: not recognising a label is not the
// same as nothing owning the workload.
func groupRank(g string) int {
	switch g {
	case GroupPlatform:
		return 0
	case GroupAddon:
		return 1
	case GroupOther:
		return 3
	default:
		return 2
	}
}

// groupLabel names the group, and for an addon which addon it belongs to.
func groupLabel(w Workload) string {
	if w.Group == GroupAddon && w.Addon != "" {
		return GroupAddon + ":" + w.Addon
	}
	return dash(w.Group)
}

// imageTag identifies an image in one column: its tag, or the head of its
// digest. A reference with neither is pulled as :latest, which is what the
// column then says, because that is what will run.
func imageTag(image string) string {
	if strings.TrimSpace(image) == "" {
		return "-"
	}
	if v := tagOrDigest(image); v != "" {
		return v
	}
	return "latest"
}

// tagOrDigest is the part of a reference that identifies the build — the tag,
// or the head of the digest — and "" when it carries neither. The distinction
// matters twice: a workload with neither is pulled as :latest, which is what
// its column says; a CONTROLLER with neither cannot be identified at all, and
// saying "latest" there would name a build nobody can look up.
func tagOrDigest(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	if at := strings.LastIndex(image, "@"); at >= 0 {
		d := image[at+1:]
		if alg, hex, ok := strings.Cut(d, ":"); ok && len(hex) > 12 {
			return alg + ":" + hex[:12]
		}
		return d
	}
	name := image
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	if _, tag, ok := strings.Cut(name, ":"); ok && tag != "" {
		return tag
	}
	return ""
}

// versionLine says what the platform was pinned to, or that nothing is pinned
// and it follows a channel — the difference an update has to start from.
func versionLine(op Operator) string {
	v := strings.TrimSpace(op.Version)
	if v == "" || v == "latest" {
		return "latest — nothing pinned; the operator follows the " + channelOr(op.Channel) + " channel"
	}
	return v + " — pinned"
}

func updateModeLine(op Operator) string {
	switch strings.TrimSpace(op.UpdateMode) {
	case "auto":
		return "auto — the operator applies in-channel updates itself"
	case "manual":
		return "manual — an update is applied when someone asks for it"
	case "":
		return "-"
	default:
		return op.UpdateMode
	}
}

// updateLine says whether there is an update, and what applies it.
func updateLine(op Operator, base string) string {
	up := strings.TrimSpace(op.AvailableUpdate)
	switch {
	case up == "":
		return "none offered on the " + channelOr(op.Channel) + " channel"
	case up == strings.TrimSpace(op.CurrentVersion):
		return up + " — already running"
	}
	return fmt.Sprintf("%s available — apply it with: zae platform update --apply --url %s", up, base)
}

func channelOr(c string) string {
	if strings.TrimSpace(c) == "" {
		return "configured release"
	}
	return c
}

// change is one field an update asks to change.
type change struct{ what, from, to string }

func (c change) String() string {
	if c.from == c.to {
		return fmt.Sprintf("%s (unchanged)", dash(c.to))
	}
	return fmt.Sprintf("%s → %s", dash(c.from), dash(c.to))
}

// renderChanges prints what a write will do, before it is done.
func renderChanges(w io.Writer, base string, chs []change, rolls int) {
	fmt.Fprintf(w, "%s — the platform\n", base)
	for _, c := range chs {
		label(w, c.what, c.String())
	}
	if rolls > 0 {
		label(w, "rolls", fmt.Sprintf("%s the operator manages", count(rolls, "workload", "workloads")))
	}
}

// label prints one "  name         value" row; every labelled row lines up.
func label(w io.Writer, name, value string) { fmt.Fprintf(w, "  %-13s%s\n", name, value) }

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
