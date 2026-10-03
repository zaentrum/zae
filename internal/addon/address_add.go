package addon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/zaentrum/zae/internal/exitcode"
)

// installResult answers POST /api/portal/addons: what installing an addon
// from its address created — or, for a check, would create.
type installResult struct {
	Key string `json:"key"`
	App struct {
		Title    string `json:"title"`
		ProxyURL string `json:"proxyUrl"`
	} `json:"app"`
	// Space is the launchpad section the addon brings, nil when its tiles go
	// to the space it is installed into.
	Space *struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	} `json:"space"`
	Tiles      int             `json:"tiles"`
	Slots      int             `json:"slots"`
	Commands   int             `json:"commands"`
	Version    string          `json:"version"`
	Components []listComponent `json:"components"`
	Setup      *setupDecl      `json:"setup"`
	// Refresh: the addon is installed already, and installing again refreshes
	// it. Adopt: it was registered at this address before addons were
	// recorded, and installing records it.
	Refresh bool `json:"refresh"`
	Adopt   bool `json:"adopt"`
	// PreviousAddress is the address an installed addon is recorded at, when
	// installing from this one would move it.
	PreviousAddress string `json:"previousAddress"`
	// DryRun is the portal saying it only checked. A pointer: a portal-api
	// that predates the check sends no such field — and installed.
	DryRun *bool `json:"dryRun"`
}

// isAddress tells an addon's address from a chart reference: http:// is an
// address — charts are fetched over https only, and a link to a chart archive
// over http is refused as one — and so is an https:// URL with no path, which
// no chart archive has.
func isAddress(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "http":
		p := strings.ToLower(u.Path)
		return !strings.HasSuffix(p, ".tgz") && !strings.HasSuffix(p, ".tar.gz")
	case "https":
		return strings.Trim(u.Path, "/") == ""
	}
	return false
}

// parseAddress checks an address's shape. Where it may point — in the
// cluster, at a Service — is the portal's rule, and the portal says so in its
// own words; zae only refuses what no address can be.
func parseAddress(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return "", fmt.Errorf("%q is not an addon's address like http://example", raw)
	case u.User != nil:
		return "", fmt.Errorf("%q carries credentials — an addon's address is its Service, e.g. http://example", raw)
	case u.RawQuery != "" || u.Fragment != "":
		return "", fmt.Errorf("%q has a query or a fragment — an addon's address is its Service, e.g. http://example", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// chartOnly are the add flags that describe a chart; an address addon is what
// its address serves.
var chartOnly = []string{"name", "version", "digest", "values", "set", "set-secret", "set-secret-file",
	"secret-values", "secret-ref", "wait", "timeout"}

// addressOnly are the add flags of an addon added by its address.
var addressOnly = []string{"dry-run", "space", "replace-address", "json"}

// addAddress checks what adding the addon at addr would do, shows it, asks,
// and installs — or, with --dry-run, stops after the check.
func addAddress(s *session, base, raw string, set map[string]bool, space string, dryRun, replace, yes, asJSON bool) int {
	for _, f := range chartOnly {
		if set[f] {
			return usageErr("--%s describes a chart; %s is an addon's address, and the addon is what it serves", f, raw)
		}
	}
	addr, err := parseAddress(raw)
	if err != nil {
		return usageErr("%v", err)
	}
	if asJSON && !dryRun {
		return usageErr("--json prints a check — add --dry-run; an install asks first")
	}
	if !dryRun && !yes && !canAsk() {
		return usageErr("stdin is not a terminal, so zae will not ask before installing — add --yes, or check first with --dry-run")
	}
	c := newClient(base)
	if space != "" {
		if code := checkSpace(s.ctx, c, base, space); code != exitcode.OK {
			return code
		}
	}
	chk, rawCheck, code := check(s, c, base, addr, space, dryRun)
	if chk == nil {
		return code
	}
	if asJSON {
		fmt.Fprintln(stdout, strings.TrimSpace(string(rawCheck)))
		return exitcode.OK
	}
	renderCheck(stdout, chk, addr)
	moving := chk.PreviousAddress != ""
	verb := "install"
	switch {
	case moving:
		verb = "move and refresh"
	case chk.Refresh:
		verb = "refresh"
	}
	if dryRun {
		hint := ""
		if moving {
			hint = " --replace-address"
		}
		fmt.Fprintf(stdout, "checked — nothing was written. %s it with: zae addon add %s --url %s%s\n", verb, addr, base, hint)
		return exitcode.OK
	}
	if moving && !replace {
		errf("failed: %s is installed from %s — installing it from %s moves it there, and the portal's proxy then sends every request for %s, with its caller's token, to the new address. Move it with --replace-address",
			chk.Key, chk.PreviousAddress, addr, chk.Key)
		return exitcode.Failed
	}
	if !yes {
		okay, cerr := s.confirm(fmt.Sprintf("%s %s on %s?", verb, chk.Key, base))
		if errors.Is(cerr, errInterrupted) {
			fmt.Fprintln(stdout, "interrupted — nothing written")
			return s.exitCode()
		}
		if !okay {
			fmt.Fprintln(stdout, "nothing written")
			return exitcode.Failed
		}
	}
	body := map[string]any{"proxyUrl": addr}
	if space != "" {
		body["space"] = space
	}
	if moving {
		body["replaceAddress"] = true
	}
	return installAddress(s, c, base, chk.Key, body)
}

// check asks the portal what installing the addon at addr would do. A
// portal-api that predates the check installs instead; that is said, and the
// result is nil with the exit code it comes to.
func check(s *session, c *client, base, addr, space string, onlyChecking bool) (*installResult, json.RawMessage, int) {
	body := map[string]any{"proxyUrl": addr, "dryRun": true}
	if space != "" {
		body["space"] = space
	}
	var raw json.RawMessage
	if err := c.do(s.ctx, "check "+addr, http.MethodPost, addonsPath, body, &raw, ""); err != nil {
		return nil, nil, failOr(s, err, "")
	}
	var chk installResult
	if err := json.Unmarshal(raw, &chk); err != nil {
		errf("undetermined: check %s: %s answered with JSON that is not an install result (%v)", addr, base, err)
		return nil, nil, exitcode.Undetermined
	}
	if chk.DryRun != nil && *chk.DryRun {
		return &chk, raw, exitcode.OK
	}
	done := "installed"
	if chk.Refresh {
		done = "refreshed"
	}
	if onlyChecking {
		errf("failed: %s cannot check first — its portal-api predates the check, and %s %s from %s when asked to check it: %s. Remove it with zae addon remove %s --url %s if it should not be there",
			base, done, printable(chk.Key), addr, summarise(&chk), printable(chk.Key), base)
		return nil, nil, exitcode.Failed
	}
	fmt.Fprintf(stdout, "%s cannot check first — its portal-api predates the check, and %s %s from %s when asked to check it, without the question: %s\n",
		base, done, printable(chk.Key), addr, summarise(&chk))
	return nil, nil, exitcode.OK
}

// installAddress makes the install a check showed.
func installAddress(s *session, c *client, base, key string, body map[string]any) int {
	var res installResult
	if err := c.do(s.ctx, "install "+key, http.MethodPost, addonsPath, body, &res, ""); err != nil {
		return failOr(s, err, fmt.Sprintf("interrupted — zae cannot tell whether %s was written; zae addon status %s --url %s shows it", key, key, base))
	}
	if res.Key == "" {
		res.Key = key
	}
	done := "installed"
	if res.Refresh {
		done = "refreshed"
	}
	fmt.Fprintf(stdout, "%s %s: %s\n", done, printable(res.Key), summarise(&res))
	return exitcode.OK
}

// checkSpace refuses, before anything is checked, a --space the launchpad
// does not have: the portal would place the tiles there regardless.
func checkSpace(ctx context.Context, c *client, base, space string) int {
	var spaces []struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	}
	if err := c.do(ctx, "list spaces", http.MethodGet, spacesPath, nil, &spaces, ""); err != nil {
		return fail(err)
	}
	keys := make([]string, 0, len(spaces))
	for _, sp := range spaces {
		if sp.Key == space {
			return exitcode.OK
		}
		keys = append(keys, sp.Key)
	}
	errf("not offered: the launchpad of %s has no space %q — its spaces: %s", base, space, dash(strings.Join(keys, ", ")))
	return exitcode.NotOffered
}

// summarise is what an install created, or a check would create, in the order
// it matters to an admin — the console's own summary.
func summarise(r *installResult) string {
	var parts []string
	if r.Space != nil {
		t := r.Space.Title
		if t == "" {
			t = r.Space.Key
		}
		parts = append(parts, fmt.Sprintf("space %q", printable(t)))
	}
	if r.Tiles > 0 {
		parts = append(parts, count(r.Tiles, "tile", "tiles"))
	}
	if r.Slots > 0 {
		parts = append(parts, count(r.Slots, "slot row", "slot rows"))
	}
	if r.Commands > 0 {
		parts = append(parts, count(r.Commands, "CLI command", "CLI commands"))
	}
	if n := len(r.Components); n > 0 {
		parts = append(parts, count(n, "container", "containers"))
	}
	if len(parts) == 0 {
		return "nothing to show — the addon declares no UI"
	}
	return strings.Join(parts, ", ")
}

// renderCheck prints what installing the addon at addr does: who it is,
// whether it is new, refreshed or moved, what it creates, what it runs, and
// what it still needs set up.
func renderCheck(w io.Writer, r *installResult, addr string) {
	title := printable(r.App.Title)
	if title == "" || title == r.Key {
		title = ""
	} else {
		title = " — " + title
	}
	if r.Version != "" {
		title += " " + printable(r.Version)
	}
	fmt.Fprintf(w, "check %s%s\n", printable(r.Key), title)
	switch {
	case r.Adopt:
		label(w, "status", "registered at this address before addons were recorded; installing records it as an addon")
	case r.Refresh:
		label(w, "status", "already installed; installing again refreshes it")
	default:
		label(w, "status", "not installed yet")
	}
	label(w, "address", addr)
	if r.PreviousAddress != "" {
		label(w, "moves", fmt.Sprintf("from %s — the portal's proxy then sends every request for %s, with its caller's token, to %s",
			printable(r.PreviousAddress), printable(r.Key), addr))
	}
	label(w, "creates", summarise(r))
	renderComponents(w, r.Components)
	if r.Setup != nil {
		renderSetup(w, r.Setup, nil)
	}
}

// note prints a remark on stderr.
func note(format string, a ...any) { errf("note: "+format, a...) }
