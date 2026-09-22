package platform

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
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
	fmt.Fprintf(w, "%s — the platform\n", base)
	label(w, "version", versionLine(op))
	label(w, "channel", dash(op.Channel))
	label(w, "update mode", updateModeLine(op))
	label(w, "phase", dash(op.Phase))
	label(w, "running", dash(op.CurrentVersion))
	label(w, "update", updateLine(op, base))
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
