package addon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// served is what an addon's address serves: its manifest, as the portal
// reads it.
type served struct {
	service, title, version string
	tiles, slots, commands  int
	workload                string
	setup                   map[string]any // the manifest's setup, nil for none
	ownSpace                bool           // the manifest brings a launchpad space
}

// recorded is an addon the portal holds: added by its address, or — chart set
// — registered from a chart.
type recorded struct {
	key, address, title, version string
	tiles, slots                 int
	workload                     string
	setup                        map[string]any
	chart                        string
	installedAt, refreshedAt     time.Time
}

// fakeAddresses is portal-api's address-addon API in miniature: the check and
// the install (one call, told apart by dryRun), the addons list, removal, the
// launchpad's spaces, an addon's own setup answer through the app proxy — and
// the chart API's "no such addon", which status and remove ask first.
type fakeAddresses struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	bodies map[string][]string

	token     string
	serves    map[string]served    // address → manifest
	addons    map[string]*recorded // key → what the portal holds
	spaces    []string
	setups    map[string]string // key → the addon's own setup answer
	oldPortal bool              // predates the check: ignores dryRun, and installs
	noCharts  bool              // predates the chart API: its routes are the router's 404
	// chartsUnavailable is the note of a cluster that cannot install charts:
	// the chart routes answer 503 with it, and the probe says so.
	chartsUnavailable string
	// chartsFail: the chart API fails for a moment.
	chartsFail bool
	refuse     int // the status every install answers with, its body in refusal
	refusal    string
	installs   int
	// olderRead is a portal-api older than GET /addons/{key}: "405" serves
	// only its DELETE, as the router then answers; "404" serves neither.
	olderRead string
}

func newAddresses(t *testing.T) (*fakeAddresses, *httptest.Server) {
	f := &fakeAddresses{t: t, bodies: map[string][]string{}, serves: map[string]served{}, addons: map[string]*recorded{},
		spaces: []string{"apps", "tools"}, setups: map[string]string{}}
	f.serves["http://sample-addon"] = served{service: "sample", title: "Sample addon", version: "1.2.0",
		tiles: 1, slots: 1, commands: 2, workload: "sample-addon"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+addonsPath, f.list)
	mux.HandleFunc("GET "+addonsPath+"/{key}", f.one)
	mux.HandleFunc("POST "+addonsPath, f.install)
	mux.HandleFunc("DELETE "+addonsPath+"/{key}", f.remove)
	mux.HandleFunc("GET "+spacesPath, func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for _, s := range f.spaces {
			out = append(out, map[string]any{"key": s, "title": strings.ToUpper(s[:1]) + s[1:]})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /api/portal/apps/{key}/", func(w http.ResponseWriter, r *http.Request) {
		if a, ok := f.setups[r.PathValue("key")]; ok {
			fmt.Fprint(w, a)
			return
		}
		http.Error(w, "no such app", http.StatusNotFound)
	})
	mux.HandleFunc("GET "+chartsPath+"/{name}", func(w http.ResponseWriter, r *http.Request) {
		if f.chartsUnavailable != "" {
			http.Error(w, f.chartsUnavailable, http.StatusServiceUnavailable)
			return
		}
		if f.chartsFail {
			http.Error(w, "etcdserver: request timed out", http.StatusInternalServerError)
			return
		}
		http.Error(w, "no such addon", http.StatusNotFound)
	})
	mux.HandleFunc("GET "+chartsPath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"available": f.chartsUnavailable == "", "note": f.chartsUnavailable})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		f.calls = append(f.calls, key)
		f.bodies[key] = append(f.bodies[key], string(body))
		r.Body = io.NopCloser(bytes.NewReader(body))
		if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "forbidden: requires the zaentrum-admin role", http.StatusForbidden)
			return
		}
		if f.noCharts && strings.HasPrefix(r.URL.Path, chartsPath) {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAddresses) row(a *recorded) map[string]any {
	refresh := false
	if m, ok := f.serves[a.address]; ok && m.version != a.version {
		refresh = true // it serves another manifest than the one installed
	}
	comp := map[string]any{"name": a.key, "workload": a.workload, "role": "primary", "summary": "serves the sample",
		"phase": "ready", "ready": 1, "desired": 1, "restarts": 0, "reason": ""}
	row := map[string]any{"key": a.key, "title": a.title, "proxyUrl": a.address, "version": a.version,
		"installedAt": a.installedAt, "refreshedAt": a.refreshedAt, "tiles": a.tiles, "slots": a.slots,
		"components": []any{comp}, "setup": a.setup, "refreshAvailable": refresh,
		"chart": nil, "suspended": false, "registered": true}
	if a.chart != "" {
		row["chart"] = map[string]any{"ref": a.chart, "version": "1.0.0", "lastApplied": map[string]any{"ref": a.chart, "version": "1.0.0"}}
		row["phase"], row["refreshAvailable"] = "Ready", false
	}
	return row
}

func sortedKeys(m map[string]*recorded) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (f *fakeAddresses) list(w http.ResponseWriter, r *http.Request) {
	out := []any{}
	for _, k := range sortedKeys(f.addons) {
		out = append(out, f.row(f.addons[k]))
	}
	_ = json.NewEncoder(w).Encode(out)
}

// one is GET /api/portal/addons/{key}: the addon's row of the list, exactly,
// or the portal's own 404.
func (f *fakeAddresses) one(w http.ResponseWriter, r *http.Request) {
	switch f.olderRead {
	case "405":
		w.Header().Set("Allow", http.MethodDelete)
		w.WriteHeader(http.StatusMethodNotAllowed) // the router's, with no body
		return
	case "404":
		http.NotFound(w, r)
		return
	}
	a := f.addons[r.PathValue("key")]
	if a == nil {
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(f.row(a))
}

// install is POST /api/portal/addons, check and install both.
func (f *fakeAddresses) install(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProxyURL       string `json:"proxyUrl"`
		Space          string `json:"space"`
		DryRun         bool   `json:"dryRun"`
		ReplaceAddress bool   `json:"replaceAddress"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProxyURL == "" {
		http.Error(w, "proxyUrl is required (the addon's in-cluster address, e.g. http://example)", http.StatusBadRequest)
		return
	}
	if f.refuse != 0 {
		http.Error(w, f.refusal, f.refuse)
		return
	}
	m, ok := f.serves[body.ProxyURL]
	if !ok {
		http.Error(w, "the addon did not answer at "+body.ProxyURL+": dial tcp: lookup: no such host", http.StatusBadGateway)
		return
	}
	dry := body.DryRun && !f.oldPortal
	existing := f.addons[m.service]
	if existing != nil && existing.chart != "" {
		http.Error(w, fmt.Sprintf("addon %q is installed from the chart %s — upgrade or reconfigure it as a chart addon", m.service, existing.chart), http.StatusConflict)
		return
	}
	previous := ""
	if existing != nil && existing.address != body.ProxyURL {
		previous = existing.address
	}
	if previous != "" && !body.ReplaceAddress && !dry {
		http.Error(w, fmt.Sprintf("addon %q is installed from %s — installing it from %s moves it there", m.service, previous, body.ProxyURL), http.StatusConflict)
		return
	}
	out := map[string]any{"key": m.service, "app": map[string]any{"key": m.service, "title": m.title, "proxyUrl": body.ProxyURL},
		"space": nil, "tiles": m.tiles, "slots": m.slots, "commands": m.commands, "checks": 0, "version": m.version,
		"components": []any{map[string]any{"name": m.service, "workload": m.workload, "role": "primary", "summary": "serves the sample",
			"phase": "ready", "ready": 1, "desired": 1, "restarts": 0, "reason": ""}},
		"setup": m.setup, "refresh": existing != nil, "adopt": false, "previousAddress": previous}
	if m.ownSpace {
		out["space"] = map[string]any{"key": "sample", "title": "Sample"}
	}
	if !f.oldPortal {
		out["dryRun"] = dry
	}
	if !dry {
		f.installs++
		now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
		rec := &recorded{key: m.service, address: body.ProxyURL, title: m.title, version: m.version, tiles: m.tiles,
			slots: m.slots, workload: m.workload, setup: m.setup, installedAt: now, refreshedAt: now}
		if existing != nil {
			rec.installedAt = existing.installedAt
		}
		f.addons[m.service] = rec
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeAddresses) remove(w http.ResponseWriter, r *http.Request) {
	a := f.addons[r.PathValue("key")]
	if a == nil {
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	if a.chart != "" {
		http.Error(w, "addon is installed from a chart — remove it as a chart addon", http.StatusConflict)
		return
	}
	delete(f.addons, a.key)
	_ = json.NewEncoder(w).Encode(map[string]any{"removed": map[string]any{"tiles": a.tiles, "rows": a.slots, "space": ""},
		"remainingWorkloads": []string{a.workload}})
}

// forget drops the calls recorded so far.
func (f *fakeAddresses) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeAddresses) called(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

func (f *fakeAddresses) body(key string, i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies[key]) <= i {
		f.t.Fatalf("no body %d for %s (calls: %v)", i, key, f.calls)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(f.bodies[key][i]), &m); err != nil {
		f.t.Fatalf("body of %s is not JSON: %v", key, err)
	}
	return m
}

// sample seeds the sample addon, installed from its address at version.
func (f *fakeAddresses) sample(version string) *recorded {
	at := time.Date(2026, 9, 14, 16, 44, 0, 0, time.UTC)
	a := &recorded{key: "sample", address: "http://sample-addon", title: "Sample addon", version: version,
		tiles: 1, slots: 1, workload: "sample-addon", installedAt: at, refreshedAt: at}
	f.addons["sample"] = a
	return a
}

const post = "POST " + addonsPath

// zae addon status sample: the chart API has no addon by that name, so the
// addons list answers — with what the console shows of it, its setup asked of
// the addon itself.
func TestStatusOfAnAddonAddedByItsAddress(t *testing.T) {
	f, srv := newAddresses(t)
	a := f.sample("1.1.0") // the address serves 1.2.0 now: a refresh waits
	a.setup = map[string]any{"path": "/api/setup", "sections": []any{
		map[string]any{"key": "storage", "title": "Storage", "required": true, "description": "where files go"},
		map[string]any{"key": "mail", "title": "Mail", "required": false},
	}}
	f.setups["sample"] = `{"state":"needs-setup","sections":[{"key":"storage","state":"needs-setup","summary":"no bucket set \u001b[31mred"},{"key":"mail","state":"ready"}]}`
	code, out, errs := run(t, "", false, "status", "sample", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	for _, re := range []string{
		`^sample — Sample addon · added by its address$`,
		`^  address     http://sample-addon$`,
		`^  version     1\.1\.0$`,
		`^  installed   2026-09-14 16:44 UTC$`,
		`^  refresh     available — the addon serves a different manifest than the one installed: zae addon refresh sample --url `,
		`^  registers   1 tile, 1 slot row$`,
		`^    sample\s+primary\s+sample-addon\s+ready\s+1/1`,
		`^  setup       needs-setup$`,
		`^    Storage\s+required\s+needs-setup\s+no bucket set \?\[31mred$`,
		`^    Mail\s+optional\s+ready$`,
	} {
		if !regexpLine(out, re) {
			t.Errorf("status lacks a line matching %s:\n%s", re, out)
		}
	}
	if f.called("GET "+chartsPath+"/sample") != 1 || f.called("GET /api/portal/apps/sample/api/setup") != 1 {
		t.Errorf("the chart API is asked first, the addon's setup through the proxy: %v", f.calls)
	}
	if f.called("GET "+addonsPath+"/sample") != 1 || f.called("GET "+addonsPath) != 0 {
		t.Errorf("the addon's own row is read, not the list: %v", f.calls)
	}

	// --json is the portal's own row.
	code, out, _ = run(t, "", false, "status", "sample", "--url", srv.URL, "--json")
	var row map[string]any
	if code != exitcode.OK || json.Unmarshal([]byte(out), &row) != nil || row["proxyUrl"] != "http://sample-addon" || row["refreshAvailable"] != true {
		t.Fatalf("--json: %d %q", code, out)
	}

	// A portal-api without the chart API has address addons all the same.
	f.noCharts = true
	if code, out, _ := run(t, "", false, "status", "sample", "--url", srv.URL); code != exitcode.OK || !strings.Contains(out, "added by its address") {
		t.Fatalf("no chart API: want the address addon, got %d\n%s", code, out)
	}
	f.noCharts = false

	// Nothing by that name in either: exit 3, as before.
	code, _, errs = run(t, "", false, "status", "nosuch", "--url", srv.URL)
	if code != exitcode.NotOffered || !strings.Contains(errs, `has no addon "nosuch"`) {
		t.Fatalf("absent: want 3, got %d %q", code, errs)
	}

	// An addon that does not answer its setup check reads as unknown, and the
	// status is still a status.
	delete(f.setups, "sample")
	code, out, _ = run(t, "", false, "status", "sample", "--url", srv.URL)
	if code != exitcode.OK || !regexpLine(out, `^  setup       unknown — the addon did not answer its setup check at /api/setup$`) ||
		!regexpLine(out, `^    Storage\s+required\s+where files go$`) {
		t.Errorf("a setup that did not answer: %d\n%s", code, out)
	}
}

// status reads the one addon's row, GET /addons/{key}, rather than the whole
// list — the portal's own 404 there is "no such addon", and the list is not
// read to find out again. A portal-api older than the read answers 405 (it
// serves only the DELETE) or its router's 404, and the list answers as before.
func TestStatusReadsTheAddonsOwnRow(t *testing.T) {
	f, srv := newAddresses(t)
	f.sample("1.2.0")
	code, out, errs := run(t, "", false, "status", "sample", "--url", srv.URL, "--json")
	var row map[string]any
	if code != exitcode.OK || json.Unmarshal([]byte(out), &row) != nil || row["key"] != "sample" || row["proxyUrl"] != "http://sample-addon" {
		t.Fatalf("--json is the portal's row for it: %d %q %q", code, out, errs)
	}
	if f.called("GET "+addonsPath+"/sample") != 1 || f.called("GET "+addonsPath) != 0 {
		t.Fatalf("one row is read, not the list: %v", f.calls)
	}

	f.forget()
	code, _, errs = run(t, "", false, "status", "nosuch", "--url", srv.URL)
	if code != exitcode.NotOffered || !strings.Contains(errs, `has no addon "nosuch"`) || f.called("GET "+addonsPath) != 0 {
		t.Fatalf("the portal's own 404: want 3 without the list, got %d %q %v", code, errs, f.calls)
	}

	for _, older := range []string{"405", "404"} {
		f.olderRead = older
		f.forget()
		code, out, errs := run(t, "", false, "status", "sample", "--url", srv.URL)
		if code != exitcode.OK || !strings.Contains(out, "added by its address") || f.called("GET "+addonsPath) != 1 {
			t.Errorf("an older portal-api (%s): want the list's row, got %d %v\n%s\n%s", older, code, f.calls, out, errs)
		}
		code, _, errs = run(t, "", false, "status", "nosuch", "--url", srv.URL)
		if code != exitcode.NotOffered || !strings.Contains(errs, `has no addon "nosuch"`) {
			t.Errorf("an older portal-api (%s), no such addon: want 3, got %d %q", older, code, errs)
		}
	}
}

func TestRemoveAnAddonAddedByItsAddress(t *testing.T) {
	f, srv := newAddresses(t)
	f.sample("1.2.0")
	code, out, _ := run(t, "n\n", true, "remove", "sample", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(out, "nothing removed") || f.called("DELETE "+addonsPath+"/sample") != 0 {
		t.Fatalf("declined: want 1 without a DELETE, got %d\n%s", code, out)
	}
	for _, s := range []string{
		"sample — Sample addon, added by its address http://sample-addon",
		"removing deletes from the portal: its app, 1 tile, 1 slot row, and any space it brought",
		"the platform does not delete containers",
		"sample-addon  (sample · primary · ready)",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("the removal lacks %q:\n%s", s, out)
		}
	}
	if code, _, errs := run(t, "", false, "remove", "sample", "--url", srv.URL, "--keep-values", "--yes"); code != exitcode.Usage || !strings.Contains(errs, "has none") {
		t.Fatalf("--keep-values for an address addon: want 2, got %d %q", code, errs)
	}
	code, out, errs := run(t, "", false, "remove", "sample", "--url", srv.URL, "--yes")
	if code != exitcode.OK || !strings.Contains(out, "removed sample: 1 tile, 1 slot row — still running, remove through your deployment channel: sample-addon") {
		t.Fatalf("want 0 naming what still runs, got %d\n%s\n%s", code, out, errs)
	}
	if code, _, _ := run(t, "", false, "remove", "sample", "--url", srv.URL, "--yes"); code != exitcode.NotOffered {
		t.Fatalf("removing it again: want 3, got %d", code)
	}
}

func TestStatusAndRemoveOfAnAddressAddonNeedTheAdminRole(t *testing.T) {
	f, srv := newAddresses(t)
	f.sample("1.2.0")
	f.token = "admin-bearer"
	t.Setenv(instance.TokenEnv, "")
	for _, args := range [][]string{
		{"status", "sample", "--url", srv.URL},
		{"remove", "sample", "--url", srv.URL, "--yes"},
		{"list", "--url", srv.URL},
	} {
		if code, _, errs := run(t, "", false, args...); code != exitcode.Forbidden || !strings.Contains(errs, "managing addons needs the platform's admin role") {
			t.Errorf("%v: want 5, got %d %q", args, code, errs)
		}
	}
	if f.addons["sample"] == nil {
		t.Fatal("a refused removal removed")
	}
}

// A cluster that cannot install addons from charts can still have addons
// added by their address: status and remove find them all the same.
func TestAddressAddonsWhereChartsCannotBeInstalled(t *testing.T) {
	f, srv := newAddresses(t)
	f.sample("1.2.0")
	f.chartsUnavailable = "this cluster serves no ZaentrumAddon resource — update the zaentrum-operator to install addons from charts"
	if code, out, errs := run(t, "", false, "status", "sample", "--url", srv.URL); code != exitcode.OK || !strings.Contains(out, "added by its address") {
		t.Fatalf("status: want the address addon, got %d\n%s\n%s", code, out, errs)
	}
	if code, out, errs := run(t, "", false, "remove", "sample", "--url", srv.URL, "--yes"); code != exitcode.OK || f.addons["sample"] != nil {
		t.Fatalf("remove: want it removed, got %d\n%s\n%s", code, out, errs)
	}
}

// What an addon says about its setup is its own: a path that would climb out
// of its proxy is not asked, and a state zae has not heard of reads unknown.
func TestAnAddonsSetupAnswerIsReadWithCare(t *testing.T) {
	f, srv := newAddresses(t)
	a := f.sample("1.2.0")
	a.setup = map[string]any{"path": "/../../operator", "sections": []any{map[string]any{"key": "storage", "title": "Storage", "required": true}}}
	f.setups["sample"] = `{"state":"ready"}`
	_, out, _ := run(t, "", false, "status", "sample", "--url", srv.URL)
	for _, c := range f.calls {
		if strings.HasPrefix(c, "GET /api/portal/apps/") || strings.Contains(c, "operator") {
			t.Fatalf("a setup path that climbs out was asked: %v", f.calls)
		}
	}
	if !regexpLine(out, `^  setup       unknown — the addon did not answer its setup check`) {
		t.Errorf("a setup that was not asked reads unknown:\n%s", out)
	}

	a.setup["path"] = "/api/setup"
	f.setups["sample"] = `{"state":"rebooting","sections":[{"key":"storage","state":"exploded"}]}`
	_, out, _ = run(t, "", false, "status", "sample", "--url", srv.URL)
	if !regexpLine(out, `^  setup       unknown$`) || !regexpLine(out, `^    Storage\s+required\s+unknown$`) {
		t.Errorf("states zae has not heard of read unknown:\n%s", out)
	}
}

// The chart API's own 503 asks it whether charts can be installed; the
// address routes' 503 is the portal unable to list its workloads for a
// moment, and says nothing about charts.
func TestA503FromTheAddressRoutesIsAMoment(t *testing.T) {
	f, srv := newAddresses(t)
	f.chartsUnavailable = "this cluster serves no ZaentrumAddon resource"
	f.refuse, f.refusal = http.StatusServiceUnavailable, "the platform cannot list its workloads right now — try again shortly"
	code, _, errs := run(t, "", false, "add", "http://sample-addon", "--url", srv.URL, "--dry-run")
	if code != exitcode.Undetermined || strings.Contains(errs, "cannot install addons from charts") || !strings.Contains(errs, "try again shortly") {
		t.Fatalf("want 4 in the portal's words, got %d %q", code, errs)
	}
}

// A refusal that quotes what an addon served reaches the terminal without
// its escape sequences.
func TestARefusalCannotDriveTheTerminal(t *testing.T) {
	f, srv := newAddresses(t)
	f.refuse, f.refusal = http.StatusUnprocessableEntity, "the addon's manifest is invalid: service \"\x1b]0;owned\x07sample\" is not a DNS label"
	code, _, errs := run(t, "", false, "add", "http://sample-addon", "--url", srv.URL, "--dry-run")
	if code != exitcode.Failed || strings.ContainsAny(errs, "\x1b\x07") || !strings.Contains(errs, "is not a DNS label") {
		t.Fatalf("want 1 with the refusal, minus its control characters, got %d %q", code, errs)
	}
}

// Only "no such addon" sends status and remove to the addons list. A chart
// API that failed said nothing about the addon, and an addon registered from
// a chart is not removed by its rows.
func TestOnlyNoSuchAddonFallsBackToTheList(t *testing.T) {
	f, srv := newAddresses(t)
	f.sample("1.2.0")
	f.chartsFail = true
	code, out, errs := run(t, "", false, "status", "sample", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(errs, "etcdserver: request timed out") ||
		f.called("GET "+addonsPath) != 0 || f.called("GET "+addonsPath+"/sample") != 0 {
		t.Fatalf("a failing chart API: want 1 without reading the addon's row, got %d\n%s\n%s", code, out, errs)
	}
	f.chartsFail = false

	f.addons["chart-one"] = &recorded{key: "chart-one", chart: "oci://ghcr.io/example/charts/chart-one", title: "c"}
	code, _, errs = run(t, "", false, "remove", "chart-one", "--url", srv.URL, "--yes")
	if code != exitcode.NotOffered || f.called("DELETE "+addonsPath+"/chart-one") != 0 {
		t.Fatalf("a chart addon's rows are its chart's to remove: want 3 and no DELETE, got %d %q %v", code, errs, f.calls)
	}
}
