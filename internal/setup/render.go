package setup

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// Where to read more: the self-hosting guide the platform's docs keep, as the
// console links it.
const (
	docs             = "https://github.com/zaentrum/zaentrum/blob/main/docs/self-hosting.md"
	docsFirstRun     = docs + "#first-run"
	docsAdminConsole = docs + "#the-admin-console"
)

// staleScan: a scan that has run this long without ending has most likely
// stopped — its catalog manager restarted under it — and may be started
// again. The console's rule.
const staleScan = 30 * time.Minute

// renderChecklist prints the checklist on one screen: every step, its state,
// and what to do next — the way `zae platform status` prints the platform.
func renderChecklist(w io.Writer, base string, d *Doc, at time.Time) {
	done, total := progress(d)
	fmt.Fprintf(w, "%s — first-run setup · %d of %d steps done\n", base, done, total)
	head, more := metadataStep(d.Metadata, base, at)
	row(w, "metadata", head, more...)
	head, more = libraryStep(d.Library, base, at)
	row(w, "library", head, more...)
	head, more = processingStep(d.Processing, base)
	row(w, "processing", head, more...)
	head, more = devicesStep(d.Devices)
	row(w, "devices", head, more...)
	head, more = peopleStep(d.People)
	row(w, "people", head, more...)
	head, more = completedStep(d.Completed, base, at)
	row(w, "marked done", head, more...)
}

// row prints one "  name         value" row, and the lines that belong to it
// under the value; every labelled row lines up.
func row(w io.Writer, name, value string, more ...string) {
	fmt.Fprintf(w, "  %-13s%s\n", name, value)
	for _, m := range more {
		fmt.Fprintf(w, "  %-13s%s\n", "", m)
	}
}

// counted are the steps that count towards "done": people is nothing the
// platform can check. An optional step — the pipeline off — counts as done.
func counted(d *Doc) []struct {
	name string
	step Step
} {
	return []struct {
		name string
		step Step
	}{
		{"metadata", d.Metadata.Step}, {"library", d.Library.Step},
		{"processing", d.Processing.Step}, {"devices", d.Devices.Step},
	}
}

func finished(s Step) bool { return s.State == stateDone || s.State == stateOptional }

// progress is how many of the counted steps are done: "2 of 4".
func progress(d *Doc) (done, total int) {
	for _, c := range counted(d) {
		total++
		if finished(c.step) {
			done++
		}
	}
	return done, total
}

// openSteps are the counted steps not done yet, in the checklist's order.
func openSteps(d *Doc) []string {
	var out []string
	for _, c := range counted(d) {
		if !finished(c.step) {
			out = append(out, c.name)
		}
	}
	return out
}

// stateWord is a step's state in a word or two, as the console's badge says
// it. A state this zae has not heard of is shown as it came.
func stateWord(step, state string) string {
	switch state {
	case stateDone:
		return "done"
	case stateTodo:
		return "to do"
	case stateWorking:
		if step == "library" {
			return "scanning"
		}
		return "starting"
	case stateOptional:
		return "optional"
	case stateInfo:
		return "manual"
	case stateUnknown, "":
		return "unknown"
	}
	return printable(state)
}

// noteOr is a step's note — why its state is unknown — or that it gave none.
func noteOr(note string) string {
	if strings.TrimSpace(note) == "" {
		return "the portal gave no reason"
	}
	return printable(strings.TrimSpace(note))
}

func metadataStep(m Metadata, base string, at time.Time) (string, []string) {
	head := stateWord("metadata", m.State) + " — "
	switch {
	case m.State == stateUnknown:
		return head + "cannot tell whether a TMDB key is set: " + noteOr(m.Note), nil
	case m.Key == "setting":
		return head + keySet(m, at), nil
	case m.Key == "environment":
		return head + "the catalog manager's own TMDB key is in effect; one set with zae setup metadata takes its place", nil
	}
	return head + "no TMDB key — titles keep their file names, and get no posters or plots", []string{
		"next: zae setup metadata --tmdb-key-file FILE --url " + base,
		"      FILE holds TMDB's API read access token (v4, it starts with eyJ)",
	}
}

// keySet says that a key is set, and when it was saved.
func keySet(m Metadata, at time.Time) string {
	if m.UpdatedAt == nil {
		return "a TMDB key is set"
	}
	return "a TMDB key is set, saved " + ago(*m.UpdatedAt, at)
}

func libraryStep(l Library, base string, at time.Time) (string, []string) {
	head := stateWord("library", l.State) + " — "
	var more []string
	if l.State == stateUnknown {
		head += "cannot tell what the catalog holds: " + noteOr(l.Note)
	} else {
		head += titlesText(l)
		more = append(more, scanText(l, at))
	}
	if strings.TrimSpace(l.Path) != "" {
		more = append(more, filesText(l))
	}
	if l.Appliance {
		more = append(more, "read: "+docsFirstRun+" — copying files to the appliance")
	}
	switch {
	case l.Scan != nil && l.Scan.Status == "running" && !scanRunning(l.Scan, at):
		more = append(more, "next: zae setup scan --url "+base)
	case l.State == stateTodo:
		more = append(more, "next: copy files there, then zae setup scan --url "+base)
	}
	return head, more
}

// titlesText says what the catalog holds: "no titles yet", "214 titles",
// "400+ titles" when the count is a floor.
func titlesText(l Library) string {
	switch {
	case l.Titles == 0:
		return "no titles yet"
	case l.TitlesMore:
		return fmt.Sprintf("%d+ titles", l.Titles)
	}
	return count(l.Titles, "title", "titles")
}

// filesText says where files go: the folder of the volume, and the path the
// catalog reads it as — the first files, or more of them.
func filesText(l Library) string {
	more := l.Titles > 0
	path := printable(l.Path)
	if strings.TrimSpace(l.Volume) == "" {
		if more {
			return "more files go to " + path + ", where the catalog reads them; scan again after copying"
		}
		return "copy files to " + path + ", where the catalog reads them, then scan"
	}
	place := "the " + printable(l.Volume) + " volume"
	if l.Folder != "" {
		place = "the " + printable(l.Folder) + "/ folder of the " + printable(l.Volume) + " volume"
	}
	if more {
		return "more files go into " + place + " (" + path + " to the catalog); scan again after copying"
	}
	return "copy files into " + place + " — the catalog reads it as " + path + " — then scan"
}

// scanRunning: a scan is under way, and not one that has stopped.
func scanRunning(s *ScanJob, at time.Time) bool {
	if s == nil || s.Status != "running" {
		return false
	}
	return s.StartedAt == nil || at.Sub(*s.StartedAt) < staleScan
}

// scanText says how the latest scan went.
func scanText(l Library, at time.Time) string {
	s := l.Scan
	if s == nil {
		return "no scan has run yet"
	}
	started := "just now"
	if s.StartedAt != nil {
		started = ago(*s.StartedAt, at)
	}
	if s.Status == "running" {
		if scanRunning(s, at) {
			return "a scan is running, started " + started + " — new titles appear as it finds them"
		}
		return "a scan started " + started + " has not ended — it may have stopped; scan again"
	}
	// when is ", 3 min ago" — or nothing, for a scan that says no time.
	when := ""
	switch {
	case s.FinishedAt != nil:
		when = ", " + ago(*s.FinishedAt, at)
	case s.StartedAt != nil:
		when = ", " + ago(*s.StartedAt, at)
	}
	switch {
	case s.Status == "failed":
		return "the last scan failed" + when + ": " + orNoReason(s.Error)
	case s.FilesSeen == 0 && when != "":
		return "the last scan" + when + ", found no files in " + printable(l.Path)
	case s.FilesSeen == 0:
		return "the last scan found no files in " + printable(l.Path)
	}
	counts := []string{count(s.FilesSeen, "file", "files"), fmt.Sprintf("%d new", s.ItemsInserted)}
	if s.ItemsUpdated > 0 {
		counts = append(counts, fmt.Sprintf("%d updated", s.ItemsUpdated))
	}
	return "the last scan" + when + ": " + strings.Join(counts, ", ")
}

func processingStep(p Processing, base string) (string, []string) {
	head := stateWord("processing", p.State) + " — "
	switch p.State {
	case stateUnknown:
		head += "cannot tell whether the media pipeline runs: " + noteOr(p.Note)
	case stateOptional:
		head += "the media pipeline is off — files stream as they are"
	case stateWorking:
		var starting []string
		for _, w := range p.Workers {
			if w.Phase != "ready" {
				starting = append(starting, printable(w.Name))
			}
		}
		head += "the media pipeline is starting: " + list(starting) + " " + isAre(len(starting)) + " not ready yet"
	case stateTodo:
		head += "the media pipeline is on, but " + faults(p.Workers)
	case stateDone:
		head += "the media pipeline runs"
	default:
		head += "the media pipeline"
	}
	var more []string
	if len(p.Workers) > 0 {
		more = append(more, "workers: "+workersLine(p.Workers))
	}
	more = append(more, gpuLine(p))
	switch {
	case p.State == stateOptional && p.Switchable:
		more = append(more, "next: zae setup pipeline on --url "+base+" — it analyzes, transcodes and packages titles for adaptive streaming")
	case p.State == stateOptional:
		more = append(more, "nothing here can switch it on: there is no operator's resource to switch it on")
	case p.State == stateTodo:
		more = append(more, "next: fix what the cluster names — zae platform status --url "+base+" shows the workloads — or zae setup pipeline off --url "+base)
	}
	return head, more
}

// workersLine is the pipeline's workers on one line: each one's phase and how
// many of its replicas are ready, and the cluster's reason when it gives one.
func workersLine(ws []Worker) string {
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		name := printable(w.Name)
		if w.Phase == "absent" {
			parts = append(parts, name+" not running yet")
			continue
		}
		s := fmt.Sprintf("%s %s %d/%d", name, printable(dash(w.Phase)), w.Ready, w.Desired)
		if w.Reason != "" {
			s += " (" + printable(w.Reason) + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}

// faults words the workers that cannot run, by what the cluster names.
func faults(ws []Worker) string {
	var names, why []string
	for _, w := range ws {
		if w.Reason == "" {
			continue
		}
		names = append(names, printable(w.Name))
		why = append(why, printable(w.Name)+": "+reasonText(w))
	}
	switch len(names) {
	case 0:
		return "not every worker runs as it should"
	case 1:
		return "the " + names[0] + " cannot run: " + reasonText(firstFaulty(ws))
	}
	return list(names) + " cannot run — " + strings.Join(why, "; ")
}

func firstFaulty(ws []Worker) Worker {
	for _, w := range ws {
		if w.Reason != "" {
			return w
		}
	}
	return Worker{}
}

// reasonText words a fault the cluster names.
func reasonText(w Worker) string {
	switch w.Reason {
	case "Unschedulable":
		if w.Name == "transcoder" {
			return "no node offers the NVIDIA GPU it asks for (nvidia.com/gpu)"
		}
		return "no node can take it"
	case "ImagePullBackOff", "ErrImagePull":
		return "its image cannot be pulled"
	case "CrashLoopBackOff":
		return "it keeps crashing"
	}
	return printable(w.Reason)
}

// gpuLine says whether a node offers the GPU the transcoder needs, or why the
// portal cannot tell.
func gpuLine(p Processing) string {
	switch {
	case p.GPUNodes == nil && strings.TrimSpace(p.GPUNote) != "":
		// The portal's note says why it cannot tell, in so many words.
		return "GPU: " + printable(strings.TrimSpace(p.GPUNote))
	case p.GPUNodes == nil:
		return "GPU: whether a node offers one cannot be told from the portal"
	case *p.GPUNodes == 0:
		return "GPU: no node offers one, which the transcoder needs"
	case *p.GPUNodes == 1:
		return "GPU: 1 node offers one"
	}
	return fmt.Sprintf("GPU: %d nodes offer one", *p.GPUNodes)
}

func devicesStep(d Devices) (string, []string) {
	head := stateWord("devices", d.State) + " — "
	host := hostOf(d.Origin)
	switch {
	case d.State == stateUnknown:
		return head + "cannot tell how the platform is reached: " + noteOr(d.Note), nil
	case d.State == stateDone:
		return head + "phones and TVs can sign in: " + host + " answers over https", nil
	case d.LocalOnly:
		head += host + " answers on this machine only — phones and TVs need a name they reach, served over https"
	case !d.HTTPS:
		head += host + " answers over http, and phones and TVs sign in over https only — put TLS in front of the platform, and set identity.issuerScheme to https"
	default:
		issuer := hostOf(d.Issuer)
		if issuer == "" {
			issuer = "the issuer"
		}
		head += "sign-in (" + issuer + ") answers over http, and phones and TVs sign in over https only"
	}
	return head, []string{"read: " + docs}
}

func peopleStep(p Step) (string, []string) {
	return stateWord("people", p.State) + " — accounts for the people who use this server are made in the identity provider's admin console; nothing here checks them",
		[]string{"read: " + docsAdminConsole}
}

func completedStep(c *Completion, base string, at time.Time) (string, []string) {
	if c == nil {
		return "no — the launchpad shows this checklist to admins until it is", []string{"next, once you are done: zae setup done --url " + base}
	}
	return "yes, " + completedText(c, at), []string{"zae setup reopen --url " + base + " shows the checklist again"}
}

// completedText says who marked setup done, and when.
func completedText(c *Completion, at time.Time) string {
	by := ""
	if strings.TrimSpace(c.By) != "" {
		by = "by " + printable(c.By) + ", "
	}
	return by + ago(c.At, at) + " (" + stamp(c.At) + ")"
}

// hostOf is the host an address names, or the address itself when it names
// none.
func hostOf(origin string) string {
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		return printable(u.Host)
	}
	return printable(origin)
}

// ago names a time for a person: "just now", "3 min ago", "5 h ago", "2 days
// ago".
func ago(t, at time.Time) string {
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

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

func orNoReason(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no reason given"
	}
	return printable(strings.TrimSpace(s))
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

// list joins names as a sentence does: "a", "a and b", "a, b and c".
func list(names []string) string {
	switch len(names) {
	case 0:
		return "none"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
