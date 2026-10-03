package addon

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

func TestAddByAddressChecksAsksAndInstalls(t *testing.T) {
	f, srv := newAddresses(t)
	code, out, errs := run(t, "y\n", true, "add", "http://sample-addon", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if got := mustJSON(t, f.body(post, 0)); got != `{"dryRun":true,"proxyUrl":"http://sample-addon"}` {
		t.Fatalf("the check: %s", got)
	}
	if got := mustJSON(t, f.body(post, 1)); got != `{"proxyUrl":"http://sample-addon"}` {
		t.Fatalf("the install sends what was checked, and moves nothing: %s", got)
	}
	for _, s := range []string{
		"check sample — Sample addon 1.2.0",
		"status      not installed yet",
		"address     http://sample-addon",
		"creates     1 tile, 1 slot row, 2 CLI commands, 1 container",
		"install sample on " + srv.URL + "? [y/N]",
		"installed sample: 1 tile, 1 slot row, 2 CLI commands, 1 container",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if !regexpLine(out, `^    sample\s+primary\s+sample-addon\s+ready\s+1/1\s+serves the sample$`) {
		t.Errorf("the container is shown with its state:\n%s", out)
	}
	if strings.Index(out, "creates") > strings.Index(out, "[y/N]") {
		t.Errorf("what it creates is shown before the question:\n%s", out)
	}
}

func TestAddByAddressDryRunWritesNothing(t *testing.T) {
	f, srv := newAddresses(t)
	code, out, errs := run(t, "", false, "add", "http://sample-addon", "--url", srv.URL, "--dry-run")
	if code != exitcode.OK || f.installs != 0 || f.called(post) != 1 {
		t.Fatalf("a check without a terminal: want 0 and one check, got %d (%d installs)\n%s\n%s", code, f.installs, out, errs)
	}
	if !strings.Contains(out, "checked — nothing was written. install it with: zae addon add http://sample-addon --url "+srv.URL) {
		t.Errorf("the check says how to install:\n%s", out)
	}
	if strings.Contains(out, "[y/N]") {
		t.Errorf("a check asks nothing:\n%s", out)
	}

	code, out, _ = run(t, "", false, "add", "http://sample-addon", "--url", srv.URL, "--dry-run", "--json")
	var doc map[string]any
	if code != exitcode.OK || json.Unmarshal([]byte(out), &doc) != nil || doc["dryRun"] != true || doc["key"] != "sample" {
		t.Fatalf("--json prints the portal's check: %d %q", code, out)
	}
	if f.installs != 0 {
		t.Fatal("a check installed")
	}
}

func TestAddByAddressDeclinedWritesNothing(t *testing.T) {
	f, srv := newAddresses(t)
	code, out, _ := run(t, "n\n", true, "add", "http://sample-addon", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(out, "nothing written") || f.installs != 0 || f.called(post) != 1 {
		t.Fatalf("declined: want 1 and nothing installed, got %d (%d installs)\n%s", code, f.installs, out)
	}
}

// Moving an installed addon redirects its proxy, and every bearer the proxy
// forwards: never without --replace-address.
func TestAddByAddressMovesOnlyWhenAsked(t *testing.T) {
	f, srv := newAddresses(t)
	f.sample("1.2.0")
	f.serves["http://sample-next"] = served{service: "sample", title: "Sample addon", version: "1.3.0", tiles: 1, slots: 1, workload: "sample-next"}
	code, out, errs := run(t, "", false, "add", "http://sample-next", "--url", srv.URL, "--yes")
	if code != exitcode.Failed || !strings.Contains(errs, "Move it with --replace-address") || f.installs != 0 {
		t.Fatalf("a move without consent: want 1 and nothing written, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "moves       from http://sample-addon") {
		t.Errorf("the check names where it moves from:\n%s", out)
	}
	code, out, errs = run(t, "", false, "add", "http://sample-next", "--url", srv.URL, "--yes", "--replace-address")
	if code != exitcode.OK || !strings.Contains(out, "refreshed sample") {
		t.Fatalf("with --replace-address: want 0, got %d\n%s\n%s", code, out, errs)
	}
	if b := f.body(post, 2); b["replaceAddress"] != true {
		t.Fatalf("the move must be confirmed to the portal: %v", b)
	}
	if f.addons["sample"].address != "http://sample-next" {
		t.Fatalf("not moved: %+v", f.addons["sample"])
	}
	// A dry run of a move says the flag it will need, and writes nothing.
	f.serves["http://sample-third"] = served{service: "sample", title: "Sample addon", version: "1.3.0", workload: "sample-third"}
	_, out, _ = run(t, "", false, "add", "http://sample-third", "--url", srv.URL, "--dry-run")
	if !strings.Contains(out, "move and refresh it with: zae addon add http://sample-third --url "+srv.URL+" --replace-address") {
		t.Errorf("the check of a move names --replace-address:\n%s", out)
	}
}

func TestAddByAddressSpace(t *testing.T) {
	f, srv := newAddresses(t)
	code, _, errs := run(t, "", false, "add", "http://sample-addon", "--url", srv.URL, "--space", "tools", "--yes")
	if code != exitcode.OK || f.body(post, 0)["space"] != "tools" || f.body(post, 1)["space"] != "tools" {
		t.Fatalf("--space reaches the check and the install: %d %q", code, errs)
	}
	code, _, errs = run(t, "", false, "add", "http://sample-addon", "--url", srv.URL, "--space", "media", "--yes")
	if code != exitcode.NotOffered || !strings.Contains(errs, `no space "media" — its spaces: apps, tools`) {
		t.Fatalf("an unknown space: want 3 naming the spaces, got %d %q", code, errs)
	}
	if f.called(post) != 2 {
		t.Fatalf("an unknown space is refused before any check: %v", f.calls)
	}
}

// A portal-api older than the check installs when asked to check. zae says
// so — and a --dry-run that wrote is a failure, because it was asked not to.
func TestAddByAddressAgainstAPortalWithoutTheCheck(t *testing.T) {
	f, srv := newAddresses(t)
	f.oldPortal = true
	code, _, errs := run(t, "", false, "add", "http://sample-addon", "--url", srv.URL, "--dry-run")
	if code != exitcode.Failed || !strings.Contains(errs, "cannot check first") || !strings.Contains(errs, "zae addon remove sample") {
		t.Fatalf("a dry run that installed: want 1 saying so, got %d %q", code, errs)
	}
	f2, srv2 := newAddresses(t)
	f2.oldPortal = true
	code, out, _ := run(t, "", false, "add", "http://sample-addon", "--url", srv2.URL, "--yes")
	if code != exitcode.OK || !strings.Contains(out, "cannot check first") || f2.called(post) != 1 {
		t.Fatalf("an install it was asked for: want 0 after one call, got %d\n%s", code, out)
	}
}

// The portal's refusals reach the terminal in its own words.
func TestAddByAddressRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		code   int
	}{
		"no answer at the address": {0, "", exitcode.Failed},
		"an invalid manifest":      {http.StatusUnprocessableEntity, "the addon's manifest is invalid: components: exactly one primary is required, got 0", exitcode.Failed},
		"a chart addon":            {http.StatusConflict, `addon "sample" is installed from the chart oci://ghcr.io/example/charts/sample — upgrade or reconfigure it as a chart addon`, exitcode.Failed},
		"workloads unlisted":       {http.StatusServiceUnavailable, "the platform cannot list its workloads right now, so it cannot verify that no component is a platform deployment — try again shortly", exitcode.Undetermined},
	} {
		f, srv := newAddresses(t)
		f.refuse, f.refusal = c.status, c.body
		addr := "http://sample-addon"
		if c.status == 0 {
			addr = "http://nothing-here"
		}
		code, _, errs := run(t, "", false, "add", addr, "--url", srv.URL, "--yes")
		want := c.body
		if c.status == 0 {
			want = "the addon did not answer at http://nothing-here"
		}
		if code != c.code || !strings.Contains(errs, want) {
			t.Errorf("%s: want %d with the portal's words, got %d %q", name, c.code, code, errs)
		}
		if f.installs != 0 {
			t.Errorf("%s: installed anyway", name)
		}
	}
}

func TestAddByAddressUsage(t *testing.T) {
	f, srv := newAddresses(t)
	chart := "oci://ghcr.io/example/charts/example:1.2.0"
	for name, args := range map[string][]string{
		"a version for an address":    {"add", "http://sample-addon", "--version", "1.2.0", "--url", srv.URL, "--yes"},
		"values for an address":       {"add", "http://sample-addon", "--set", "a=1", "--url", srv.URL, "--yes"},
		"a wait for an address":       {"add", "http://sample-addon", "--wait", "--url", srv.URL, "--yes"},
		"a secret for an address":     {"add", "http://sample-addon", "--set-secret", "a=b", "--url", srv.URL, "--yes"},
		"a check of a chart":          {"add", chart, "--dry-run", "--url", srv.URL, "--yes"},
		"a space for a chart":         {"add", chart, "--space", "apps", "--url", srv.URL, "--yes"},
		"json for an install":         {"add", "http://sample-addon", "--json", "--url", srv.URL, "--yes"},
		"credentials in an address":   {"add", "http://user:pw@sample-addon", "--url", srv.URL, "--yes"},
		"a query in an address":       {"add", "http://sample-addon?x=1", "--url", srv.URL, "--yes"},
		"no terminal, no --yes":       {"add", "http://sample-addon", "--url", srv.URL},
		"an http chart archive":       {"add", "http://example.org/charts/example-1.2.0.tgz", "--url", srv.URL, "--yes"},
		"refresh, no terminal":        {"refresh", "sample", "--url", srv.URL},
		"refresh, two names":          {"refresh", "a", "b", "--url", srv.URL, "--yes"},
		"refresh, not a name":         {"refresh", "Sample", "--url", srv.URL, "--yes"},
		"remove, not a name":          {"remove", "Sample", "--url", srv.URL, "--yes"},
		"status, not a name":          {"status", "Sample!", "--url", srv.URL},
		"refresh without an instance": {"refresh", "sample", "--yes"},
	} {
		if code, _, errs := run(t, "", false, args...); code != exitcode.Usage {
			t.Errorf("%s: want 2, got %d %q", name, code, errs)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("usage errors must not reach the portal: %v", f.calls)
	}
}

func TestRefreshAnAddonAddedByItsAddress(t *testing.T) {
	f, srv := newAddresses(t)
	f.sample("1.1.0")
	code, out, errs := run(t, "", false, "refresh", "sample", "--url", srv.URL, "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if mustJSON(t, f.body(post, 0)) != `{"dryRun":true,"proxyUrl":"http://sample-addon"}` || mustJSON(t, f.body(post, 1)) != `{"proxyUrl":"http://sample-addon"}` {
		t.Fatalf("a refresh is a check, then an install from the recorded address: %v", f.bodies[post])
	}
	for _, s := range []string{
		"status      already installed; installing again refreshes it",
		"manifest    changed — the addon serves a different one than the one installed",
		"refreshed sample: 1 tile, 1 slot row, 2 CLI commands, 1 container",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	// The portal keeps no record of the space an addon was added to: say
	// where its tiles go, unless --space names it.
	if !strings.Contains(errs, "a refresh places its tile in the launchpad's first space unless --space names another") {
		t.Errorf("the refresh must say where the tiles go: %q", errs)
	}
	_, _, errs = run(t, "", false, "refresh", "sample", "--url", srv.URL, "--yes", "--space", "tools")
	if f.body(post, 3)["space"] != "tools" || strings.Contains(errs, "first space") {
		t.Errorf("--space reaches the install, and the note goes: %v %q", f.body(post, 3), errs)
	}

	f.addons["chart-one"] = &recorded{key: "chart-one", chart: "oci://ghcr.io/example/charts/chart-one", title: "c"}
	if code, _, errs := run(t, "", false, "refresh", "chart-one", "--url", srv.URL, "--yes"); code != exitcode.Usage || !strings.Contains(errs, "zae addon upgrade chart-one") {
		t.Fatalf("a chart addon is not refreshed by hand: want 2, got %d %q", code, errs)
	}
	if code, _, _ := run(t, "", false, "refresh", "nosuch", "--url", srv.URL, "--yes"); code != exitcode.NotOffered {
		t.Fatalf("an unknown addon: want 3, got %d", code)
	}
	code, out, _ = run(t, "n\n", true, "refresh", "sample", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(out, "not refreshed") || !strings.Contains(out, "refresh sample on "+srv.URL+"? [y/N]") {
		t.Fatalf("declined: want 1, got %d\n%s", code, out)
	}
}

func TestAddressAddonsNeedTheAdminRole(t *testing.T) {
	f, srv := newAddresses(t)
	f.sample("1.2.0")
	f.token = "admin-bearer"
	t.Setenv(instance.TokenEnv, "")
	for _, args := range [][]string{
		{"add", "http://sample-addon", "--url", srv.URL, "--dry-run"},
		{"add", "http://sample-addon", "--url", srv.URL, "--yes"},
		{"refresh", "sample", "--url", srv.URL, "--yes"},
	} {
		if code, _, errs := run(t, "", false, args...); code != exitcode.Forbidden || !strings.Contains(errs, "managing addons needs the platform's admin role") {
			t.Errorf("%v: want 5, got %d %q", args, code, errs)
		}
	}
	if f.installs != 0 || f.addons["sample"] == nil {
		t.Fatal("a refused call changed something")
	}
}

func TestIsAddress(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://sample-addon":                      true,
		"http://sample-addon:8080":                 true,
		"http://sample-addon.zaentrum.svc/":        true,
		"https://sample-addon":                     true,
		"http://sample-addon/api":                  true,
		"https://example.org/charts/x-1.0.0.tgz":   false,
		"http://example.org/charts/x-1.0.0.tgz":    false, // a chart archive over http: refused as one
		"http://example.org/charts/x-1.0.0.tar.gz": false,
		"oci://ghcr.io/example/charts/x":           false,
		"sample-addon":                             false,
		"http:///no-host":                          false,
	} {
		if got := isAddress(raw); got != want {
			t.Errorf("isAddress(%q) = %v, want %v", raw, got, want)
		}
	}
}
