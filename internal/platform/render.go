package platform

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// controllerNote is the boundary of this command group, printed with every
// status and repeated in the docs. It is a fact about where the two pieces
// run, not a shortcoming: the portal administers the platform's namespace,
// and the controller that deploys the platform runs outside it.
const controllerNote = `Not covered here: the operator's own controller image. The controller runs in
its own namespace, outside the one the portal administers, so zae can neither
read nor change the version of the controller itself. Update the controller by
applying its install bundle — or through OLM, on a cluster that installs it
that way. This command updates the platform the controller deploys.`

// renderStatus prints the platform on one screen: what it was asked to run,
// what it reports running, and every workload beside it.
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
	fmt.Fprintln(w, controllerNote)
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
	image = strings.TrimSpace(image)
	if image == "" {
		return "-"
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
	return "latest"
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
