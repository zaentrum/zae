package addon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// Addons added by their address.
//
// An addon deployed some other way — its own chart, a manifest, a deployment
// repository — is added by its in-cluster address: portal-api reads the
// capability manifest it serves and creates what it declares (its app, tiles,
// slot rows and CLI commands), all under the addon's key. Nothing is installed
// in the cluster: the platform neither deploys such an addon nor deletes it.
//
// One call does both halves. POST /api/portal/addons with dryRun answers what
// installing would do and writes nothing — the console's "check" — and
// without it installs, or refreshes an addon installed from that address.
// DELETE /api/portal/addons/{key} removes what the portal created. There is no
// read of one such addon: the list carries them all.

// spacesPath lists the launchpad's spaces, for a --space that names one.
const spacesPath = "/api/portal/spaces"

// keyRe is an address addon's key: the service its manifest names, which the
// portal holds to one DNS label.
var keyRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validKey(s string) bool { return len(s) <= 63 && keyRe.MatchString(s) }

// setupDecl is what an addon's manifest says about its setup: the endpoint
// that reports it, and the sections that report covers.
type setupDecl struct {
	Path     string `json:"path"`
	Sections []struct {
		Key         string `json:"key"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Required    bool   `json:"required"`
	} `json:"sections"`
}

// setupStatus is what the addon itself answers at its setup path.
type setupStatus struct {
	State    string `json:"state"`
	Sections []struct {
		Key     string `json:"key"`
		State   string `json:"state"`
		Summary string `json:"summary"`
	} `json:"sections"`
	// unanswered: the addon was asked and gave no answer zae can read.
	unanswered bool
}

// removedAddress answers DELETE /api/portal/addons/{key}.
type removedAddress struct {
	Removed struct {
		Tiles int    `json:"tiles"`
		Rows  int    `json:"rows"`
		Space string `json:"space"`
	} `json:"removed"`
	RemainingWorkloads []string `json:"remainingWorkloads"`
}

// findListed reads the addons list and returns the row for name, with its own
// JSON; row is nil when the list has none.
func findListed(ctx context.Context, c *client, name string) (*Listed, json.RawMessage, error) {
	var rows []json.RawMessage
	if err := c.do(ctx, "list addons", http.MethodGet, addonsPath, nil, &rows, ""); err != nil {
		return nil, nil, err
	}
	for _, raw := range rows {
		var r Listed
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		if r.key() == name {
			return &r, raw, nil
		}
	}
	return nil, nil, nil
}

// fallsBack: the chart API has no addon of that name — or the instance cannot
// install from charts at all — so an addon added by its address is looked
// for. Not on a failure to ask: that is no answer about either kind.
func fallsBack(err error) bool {
	ae, ok := asAPIError(err)
	return ok && (ae.notFound || ae.noAPI)
}

// statusByAddress prints an addon from the addons list: one added by its
// address, which only the list carries.
func statusByAddress(ctx context.Context, c *client, base, name string, asJSON bool) int {
	row, raw, err := findListed(ctx, c, name)
	if err != nil {
		return fail(err)
	}
	if row == nil {
		errf("not offered: %s has no addon %q — zae addon list --url %s lists what it has", base, name, base)
		return exitcode.NotOffered
	}
	if asJSON {
		fmt.Fprintln(stdout, strings.TrimSpace(string(raw)))
		return exitcode.OK
	}
	var st *setupStatus
	if row.Setup != nil && row.Registered {
		if st = readSetup(ctx, c, row.key(), row.Setup.Path); st == nil {
			st = &setupStatus{State: "unknown", unanswered: true}
		}
	}
	renderAddressStatus(stdout, base, row, st)
	return exitcode.OK
}

// readSetup asks the addon, through the portal's app proxy and with the
// caller's bearer, whether it is set up — as the console does. Its answer is
// its own; anything that is not one reads as unknown, and a failure to ask
// is not an error of the status around it.
func readSetup(ctx context.Context, c *client, key, path string) *setupStatus {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.Contains(path, "//") ||
		strings.ContainsAny(path, "\\# ") || printable(path) != path {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/portal/apps/"+url.PathEscape(key)+path, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	if instance.Authorize(ctx, req, c.base) != nil {
		return nil
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var st setupStatus
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st) != nil {
		return nil
	}
	return &st
}

// setupStates are the states an addon may report; anything else is unknown.
var setupStates = map[string]bool{"ready": true, "needs-setup": true, "degraded": true}

func setupState(s string) string {
	if setupStates[s] {
		return s
	}
	return "unknown"
}

// removeAddress removes an addon added by its address: what the portal created
// for it. Its containers are not the platform's to delete, and the removal
// says which still run.
func removeAddress(s *session, c *client, base string, row *Listed, yes bool) int {
	name := row.key()
	fmt.Fprintf(stdout, "%s — %s, added by its address %s\n", name, printable(dash(row.Title)), dash(row.ProxyURL))
	fmt.Fprintf(stdout, "  removing deletes from the portal: its app, %s, %s, and any space it brought\n",
		count(row.Tiles, "tile", "tiles"), count(row.Slots, "slot row", "slot rows"))
	if len(row.Components) > 0 {
		fmt.Fprintln(stdout, "  the platform does not delete containers — what of these is deployed keeps running until you remove it through your deployment channel:")
		for _, cm := range row.Components {
			fmt.Fprintf(stdout, "    %s  (%s · %s · %s)\n", printable(cm.Workload), printable(cm.Name), printable(dash(cm.Role)), componentPhase(cm))
		}
	} else {
		fmt.Fprintln(stdout, "  the platform does not delete containers: the addon's workload is yours to remove")
	}
	if !yes {
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
	var r removedAddress
	if err := c.do(s.ctx, "remove "+name, http.MethodDelete, addonsPath+"/"+url.PathEscape(name), nil, &r,
		fmt.Sprintf("%s has no addon %q", base, name)); err != nil {
		return failOr(s, err, fmt.Sprintf("interrupted — zae cannot tell whether %s was removed; zae addon status %s --url %s shows it", name, name, base))
	}
	parts := []string{count(r.Removed.Tiles, "tile", "tiles"), count(r.Removed.Rows, "slot row", "slot rows")}
	if r.Removed.Space != "" {
		parts = append(parts, "the space "+printable(r.Removed.Space))
	}
	msg := fmt.Sprintf("removed %s: %s", name, strings.Join(parts, ", "))
	if len(r.RemainingWorkloads) > 0 {
		left := make([]string, 0, len(r.RemainingWorkloads))
		for _, w := range r.RemainingWorkloads {
			left = append(left, printable(w))
		}
		msg += " — still running, remove through your deployment channel: " + strings.Join(left, ", ")
	}
	fmt.Fprintln(stdout, msg)
	return exitcode.OK
}

// renderAddressStatus prints an addon from the addons list.
func renderAddressStatus(w io.Writer, base string, r *Listed, st *setupStatus) {
	name := r.key()
	head := name
	if t := printable(r.Title); t != "" && t != name {
		head += " — " + t
	}
	how := "added by its address"
	if r.Chart != nil {
		how = "registered from the chart " + chartLine(&Chart{Ref: r.Chart.Ref, Version: r.Chart.Version})
	}
	fmt.Fprintf(w, "%s · %s\n", head, how)
	label(w, "address", dash(r.ProxyURL))
	if v := printable(r.Version); v != "" {
		label(w, "version", v)
	} else {
		label(w, "version", "- (the addon's manifest names none)")
	}
	when := stamp(r.InstalledAt)
	if !r.RefreshedAt.IsZero() && !r.RefreshedAt.Equal(r.InstalledAt) {
		when += ", refreshed " + stamp(r.RefreshedAt)
	}
	label(w, "installed", when)
	switch {
	case r.Chart != nil:
		label(w, "refresh", "by the portal — it registers a chart addon again whenever its manifest changes")
	case r.RefreshAvailable:
		label(w, "refresh", fmt.Sprintf("available — the addon serves a different manifest than the one installed: zae addon refresh %s --url %s", name, base))
	default:
		label(w, "refresh", "none waiting — the addon serves the manifest it was installed with")
	}
	label(w, "registers", fmt.Sprintf("%s, %s", count(r.Tiles, "tile", "tiles"), count(r.Slots, "slot row", "slot rows")))
	if r.RegistrationError != "" {
		label(w, "registration", printable(r.RegistrationError))
	}
	renderComponents(w, r.Components)
	if r.Setup != nil {
		renderSetup(w, r.Setup, st)
	} else {
		label(w, "setup", "none declared")
	}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// renderComponents prints an addon's containers with their live state: the
// workload's phase, "not deployed" when nothing by that name runs, "unknown"
// when the portal cannot see workloads.
func renderComponents(w io.Writer, cs []listComponent) {
	if len(cs) == 0 {
		label(w, "containers", "none declared")
		return
	}
	fmt.Fprintln(w, "  containers")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range cs {
		ready := "-"
		if c.Ready != nil && c.Desired != nil {
			ready = fmt.Sprintf("%d/%d", *c.Ready, *c.Desired)
		}
		extra := ""
		if c.Reason != nil && *c.Reason != "" {
			extra = printable(*c.Reason)
		}
		if c.Restarts != nil && *c.Restarts > 0 {
			extra = strings.TrimSpace(extra + " " + count(*c.Restarts, "restart", "restarts"))
		}
		if extra == "" && c.Summary != "" {
			extra = printable(c.Summary)
		}
		row(tw, printable(c.Name), printable(dash(c.Role)), printable(dash(c.Workload)), componentPhase(c), ready, extra)
	}
	tw.Flush()
}

func componentPhase(c listComponent) string {
	if c.Phase == nil {
		return "not deployed"
	}
	return printable(dash(*c.Phase))
}

// renderSetup prints an addon's setup sections, with the state the addon
// reported for each when it was asked.
func renderSetup(w io.Writer, d *setupDecl, st *setupStatus) {
	head := fmt.Sprintf("%s, reported by the addon at %s", count(len(d.Sections), "section", "sections"), printable(d.Path))
	switch {
	case st != nil && st.unanswered:
		head = "unknown — the addon did not answer its setup check at " + printable(d.Path)
	case st != nil:
		head = setupState(st.State)
	}
	label(w, "setup", head)
	reported := map[string]int{}
	if st != nil {
		for i, s := range st.Sections {
			reported[s.Key] = i
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, sec := range d.Sections {
		need := "optional"
		if sec.Required {
			need = "required"
		}
		t := sec.Title
		if t == "" {
			t = sec.Key
		}
		state, summary := "", printable(sec.Description)
		if st != nil && !st.unanswered {
			state = "unknown"
			if i, ok := reported[sec.Key]; ok {
				state = setupState(st.Sections[i].State)
				if s := st.Sections[i].Summary; s != "" {
					summary = printable(clip(s, 200))
				}
			}
		}
		cells := []string{printable(t), need}
		if state != "" {
			cells = append(cells, state)
		}
		row(tw, append(cells, summary)...)
	}
	tw.Flush()
}

// row writes one indented table row, without the empty cells at its end: a
// cell that ends a line pads the one before it with spaces nobody sees.
func row(w io.Writer, cells ...string) {
	for len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}
	fmt.Fprintf(w, "    %s\n", strings.Join(cells, "\t"))
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// printable keeps text an addon wrote — its title, its components' summaries,
// its setup's answers — on one line and out of the terminal's control: an
// escape sequence in it would be executed by the terminal rather than shown.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}
