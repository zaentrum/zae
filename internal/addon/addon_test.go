package addon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// exampleSchema is a chart's values.schema.json: one required secret input,
// one generated input, plain values with defaults, and the reserved platform
// key, which is not an input.
const exampleSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "required": ["database"],
  "properties": {
    "database": {
      "type": "object",
      "required": ["password"],
      "properties": {
        "password": {"type": "string", "writeOnly": true, "title": "Database password"},
        "host": {"type": "string", "default": "postgres"}
      }
    },
    "config": {
      "type": "object",
      "required": ["key"],
      "properties": {
        "key": {"type": "string", "writeOnly": true, "x-zaentrum-generate": "random-base64-32"}
      }
    },
    "worker": {
      "type": "object",
      "properties": {
        "replicas": {"type": "integer", "default": 1},
        "logLevel": {"type": "string", "enum": ["debug", "info"]}
      }
    },
    "zaentrum": {"type": "object"}
  }
}`

const secretValue = "s3cret-value-never-printed"

// fakePortal is portal-api and the operator in miniature. The chart API
// stores addons; every GET moves the operator one step: a suspended addon is
// planned after stepsToPlan reads (before that the status from before the
// write is still served), an installed one is Installing for stepsToReady
// reads and then Ready.
type fakePortal struct {
	t            *testing.T
	mu           sync.Mutex
	addons       map[string]*fakeAddon
	listing      string
	calls        []string
	bodies       map[string][]string
	stepsToPlan  int
	stepsToReady int
	hideCreated  int // 404s served for a just-created addon, like a lagging cache
	token        string
	planFor      func(f *fakeAddon) *Plan
}

type fakeAddon struct {
	Addon
	chart, version, digest string
	secretValues           map[string]string
	steps                  int
	installing             bool
	hidden                 int
}

func newPortal(t *testing.T) (*fakePortal, *httptest.Server) {
	p := &fakePortal{t: t, addons: map[string]*fakeAddon{}, bodies: map[string][]string{}, planFor: examplePlan,
		stepsToPlan: 1, stepsToReady: 1, listing: "[]"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+chartsPath, p.create)
	mux.HandleFunc("GET "+chartsPath+"/{name}", p.get)
	mux.HandleFunc("PATCH "+chartsPath+"/{name}", p.patch)
	mux.HandleFunc("DELETE "+chartsPath+"/{name}", p.remove)
	mux.HandleFunc("POST "+chartsPath+"/{name}/install", p.install)
	mux.HandleFunc("GET "+addonsPath, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, p.listing) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		p.mu.Lock()
		key := r.Method + " " + r.URL.Path
		p.calls = append(p.calls, key)
		p.bodies[key] = append(p.bodies[key], body)
		p.mu.Unlock()
		if p.token != "" && r.Header.Get("Authorization") != "Bearer "+p.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = readCloser(body)
		p.mu.Lock()
		defer p.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return p, srv
}

// examplePlan renders the example chart for the addon's spec: two workloads,
// and a values error while the required secret input is missing.
func examplePlan(f *fakeAddon) *Plan {
	p := &Plan{
		Chart: PlanChart{Name: "example", Version: f.version, AppVersion: "4.1.0",
			Description: "An example addon", Digest: "sha256:" + strings.Repeat("a", 64)},
		ValuesSchema: schemaDoc(exampleSchema),
		Objects: []Object{{Kind: "Deployment", Name: "example"}, {Kind: "Deployment", Name: "example-worker"},
			{Kind: "Service", Name: "example"}, {Kind: "Secret", Name: "example"}},
		Workloads: []Workload{
			{Kind: "Deployment", Name: "example", Images: []string{"ghcr.io/example/example:" + f.version},
				Ports: []json.RawMessage{json.RawMessage(`{"name":"http","containerPort":8080,"protocol":"TCP"}`)}},
			{Kind: "Deployment", Name: "example-worker", Images: []string{"ghcr.io/example/worker:" + f.version}},
		},
	}
	if f.LastAppliedChart != nil && f.LastAppliedChart.Version != f.version {
		p.Changes.Images = []string{fmt.Sprintf("Deployment/example: ghcr.io/example/example:%s → ghcr.io/example/example:%s", f.LastAppliedChart.Version, f.version)}
	}
	if _, ok := f.secretValues["database.password"]; !ok {
		p.ValuesErrors = []string{"database: password is required"}
	}
	return p
}

func (p *fakePortal) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name         string            `json:"name"`
		Chart        string            `json:"chart"`
		Version      string            `json:"version"`
		Digest       string            `json:"digest"`
		Values       map[string]any    `json:"values"`
		SecretValues map[string]string `json:"secretValues"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || body.Chart == "" {
		http.Error(w, "name and chart are required", http.StatusBadRequest)
		return
	}
	f := p.addons[body.Name]
	if f == nil {
		f = &fakeAddon{Addon: Addon{Name: body.Name, Phase: PhasePending}, hidden: p.hideCreated}
		p.addons[body.Name] = f
	}
	f.chart, f.version, f.digest = body.Chart, body.Version, body.Digest
	f.Values, f.secretValues = body.Values, body.SecretValues
	f.SecretKeys = nil
	for k := range body.SecretValues {
		f.SecretKeys = append(f.SecretKeys, k)
	}
	slices.Sort(f.SecretKeys)
	f.Suspended, f.steps = true, 0
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"name":%q}`, body.Name)
}

func (p *fakePortal) get(w http.ResponseWriter, r *http.Request) {
	f := p.addons[r.PathValue("name")]
	if f == nil {
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	if f.hidden > 0 {
		f.hidden--
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	f.steps++
	switch {
	case f.Suspended && f.steps > p.stepsToPlan:
		f.Plan = p.planFor(f)
		f.Phase = PhasePlanned
		if len(f.Plan.Violations)+len(f.Plan.ValuesErrors) > 0 {
			f.Phase = PhasePlanFailed
		}
	case !f.Suspended && f.installing && f.steps > p.stepsToReady:
		f.Phase, f.installing = PhaseReady, false
		f.LastAppliedChart = &Chart{Ref: f.chart, Version: f.version, Digest: "sha256:" + strings.Repeat("a", 64)}
		f.Components = []Component{{Name: "example", Kind: "Deployment", Ready: 1, Desired: 1},
			{Name: "example-worker", Kind: "Deployment", Ready: 1, Desired: 1}}
	case !f.Suspended && f.installing:
		f.Phase = PhaseInstalling
		f.Components = []Component{{Name: "example", Kind: "Deployment", Ready: 0, Desired: 1},
			{Name: "example-worker", Kind: "Deployment", Ready: 0, Desired: 1, Reason: "ContainerCreating"}}
	}
	_ = json.NewEncoder(w).Encode(f.Addon)
}

func (p *fakePortal) install(w http.ResponseWriter, r *http.Request) {
	f := p.addons[r.PathValue("name")]
	if f == nil {
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	if f.Plan == nil || len(f.Plan.Violations)+len(f.Plan.ValuesErrors) > 0 {
		http.Error(w, "the current plan cannot be installed", http.StatusConflict)
		return
	}
	f.Suspended, f.installing, f.steps = false, true, 0
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprint(w, `{}`)
}

func (p *fakePortal) patch(w http.ResponseWriter, r *http.Request) {
	f := p.addons[r.PathValue("name")]
	if f == nil {
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	var body struct {
		Chart, Version, Digest *string
		Suspend                *bool
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	changed := false
	for _, field := range []struct {
		from *string
		to   *string
	}{{body.Chart, &f.chart}, {body.Version, &f.version}, {body.Digest, &f.digest}} {
		if field.from != nil && *field.from != *field.to {
			*field.to, changed = *field.from, true
		}
	}
	switch {
	case body.Suspend != nil:
		f.Suspended = *body.Suspend
	case changed:
		f.Suspended = true
	}
	f.installing = !f.Suspended
	f.steps = 0
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprint(w, `{}`)
}

func (p *fakePortal) remove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if p.addons[name] == nil {
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	delete(p.addons, name)
	fmt.Fprint(w, `{}`)
}

func (p *fakePortal) called(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.calls, key)
}

func (p *fakePortal) body(key string, i int) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.bodies[key]) <= i {
		p.t.Fatalf("no body %d for %s (calls: %v)", i, key, p.calls)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(p.bodies[key][i]), &m); err != nil {
		p.t.Fatalf("body of %s is not JSON: %v", key, err)
	}
	return m
}

// running seeds an addon that is installed and Ready at version.
func (p *fakePortal) running(name, version string) *fakeAddon {
	f := &fakeAddon{Addon: Addon{Name: name, Phase: PhaseReady,
		LastAppliedChart: &Chart{Ref: "oci://ghcr.io/example/charts/example", Version: version, Digest: "sha256:" + strings.Repeat("a", 64)},
		Components:       []Component{{Name: "example", Kind: "Deployment", Ready: 1, Desired: 1}, {Name: "example-worker", Kind: "Deployment", Ready: 1, Desired: 1}},
		SecretKeys:       []string{"database.password"}},
		chart: "oci://ghcr.io/example/charts/example", version: version,
		secretValues: map[string]string{"database.password": secretValue}}
	f.Plan = examplePlan(f)
	p.addons[name] = f
	return f
}

func readAll(r *http.Request) (string, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(r.Body)
	return b.String(), err
}

type bodyReader struct{ *strings.Reader }

func (bodyReader) Close() error { return nil }

func readCloser(s string) bodyReader { return bodyReader{strings.NewReader(s)} }

// run executes `zae addon …` with stdin input and a terminal or not.
func run(t *testing.T, input string, terminal bool, args ...string) (code int, out, errs string) {
	t.Helper()
	var ob, eb bytes.Buffer
	stdout, stderr, stdin = &ob, &eb, strings.NewReader(input)
	canAsk = func() bool { return terminal }
	pollInterval, notFoundGrace = time.Millisecond, 200*time.Millisecond
	defer func() {
		stdout, stderr, stdin = os.Stdout, os.Stderr, os.Stdin
		canAsk, pollInterval, notFoundGrace = stdinIsTerminal, 2*time.Second, 15*time.Second
	}()
	code = Run(args)
	return code, ob.String(), eb.String()
}

func TestAddPlansConfirmsInstallsAndWaitsForReady(t *testing.T) {
	p, srv := newPortal(t)
	p.token = "admin-bearer"
	p.stepsToPlan, p.stepsToReady = 2, 2
	t.Setenv(instance.TokenEnv, "admin-bearer")
	valuesFile := filepath.Join(t.TempDir(), "values.json")
	if err := os.WriteFile(valuesFile, []byte(`{"worker":{"logLevel":"info"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errs := run(t, "y\n", true, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL,
		"--values", valuesFile, "--set", "worker.replicas=2", "--set-secret", "database.password="+secretValue, "--wait")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}

	sent := p.body("POST "+chartsPath, 0)
	want := map[string]any{
		"name": "example", "chart": "oci://ghcr.io/example/charts/example", "version": "1.2.0",
		"values":       map[string]any{"worker": map[string]any{"logLevel": "info", "replicas": float64(2)}},
		"secretValues": map[string]any{"database.password": secretValue},
	}
	if got, _ := json.Marshal(sent); string(got) != mustJSON(t, want) {
		t.Fatalf("POST body:\n got %s\nwant %s", got, mustJSON(t, want))
	}
	for _, s := range []string{
		"plan for example — chart example 1.2.0 — Planned",
		"Deployment/example", "ghcr.io/example/example:1.2.0", "ports http 8080/TCP",
		"Deployment/example-worker", "ghcr.io/example/worker:1.2.0",
		"objects     4 (Deployment 2, Secret 1, Service 1)",
		"database.password", "config.key", "generated by the operator",
		`worker.logLevel`, `"info"`,
		"install example (example 1.2.0) on " + srv.URL + "? [y/N]",
		"example is Ready",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if !regexpLine(out, `database\.password\s+secret, required\s+set`) {
		t.Errorf("the secret input must read only as set:\n%s", out)
	}
	if strings.Index(out, "plan for example") > strings.Index(out, "install example") {
		t.Errorf("the plan must be printed before the question:\n%s", out)
	}
	if strings.Contains(out+errs, secretValue) {
		t.Fatalf("a secret value was printed:\n%s\n%s", out, errs)
	}
	if !p.called("POST " + chartsPath + "/example/install") {
		t.Fatalf("install was never called: %v", p.calls)
	}
}

func TestPlanFailedIsNotInstalled(t *testing.T) {
	p, srv := newPortal(t)
	code, out, errs := run(t, "", false, "add", "oci://ghcr.io/example/charts/example", "--version", "1.2.0",
		"--url", srv.URL, "--yes")
	if code != exitcode.Failed {
		t.Fatalf("a plan with values errors: want 1, got %d\n%s\n%s", code, out, errs)
	}
	for _, s := range []string{"PlanFailed", "values errors — install is blocked", "database: password is required",
		"still to give: --set-secret database.password=…"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if !regexpLine(out, `database\.password\s+secret, required\s+missing`) {
		t.Errorf("the missing secret input must read as missing:\n%s", out)
	}
	if !strings.Contains(errs, "example is not installed: 1 values error") {
		t.Errorf("stderr must say why: %q", errs)
	}
	if p.called("POST " + chartsPath + "/example/install") {
		t.Fatal("a plan with values errors must never be installed")
	}
}

func TestViolationsBlockInstall(t *testing.T) {
	p, srv := newPortal(t)
	p.planFor = func(f *fakeAddon) *Plan {
		plan := examplePlan(f)
		plan.Violations = []string{"Deployment/example-worker: hostPath volume not allowed"}
		return plan
	}
	code, out, errs := run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL,
		"--set-secret", "database.password=x", "--yes", "--wait")
	if code != exitcode.Failed || !strings.Contains(errs, "1 object refused by the guardrails") {
		t.Fatalf("want 1 naming the refusal, got %d %q", code, errs)
	}
	if !strings.Contains(out, "refused by the guardrails — install is blocked") ||
		!strings.Contains(out, "✗ Deployment/example-worker: hostPath volume not allowed") {
		t.Errorf("violations must be printed:\n%s", out)
	}
	if p.called("POST " + chartsPath + "/example/install") {
		t.Fatal("--yes must not install a plan the guardrails refused")
	}
}

// Without a terminal and without --yes, zae must not prompt — and must fail
// before it writes anything, so a script's mistake leaves no plan behind.
func TestNoTerminalWithoutYesIsUsageAndWritesNothing(t *testing.T) {
	p, srv := newPortal(t)
	cases := map[string]struct {
		terminal bool
		args     []string
	}{
		"add, no terminal":     {false, []string{"add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL}},
		"add, values stdin":    {true, []string{"add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL, "--values", "-"}},
		"upgrade, no terminal": {false, []string{"upgrade", "example", "--version", "1.3.0", "--url", srv.URL}},
		"remove, no terminal":  {false, []string{"remove", "example", "--url", srv.URL}},
	}
	for name, tc := range cases {
		code, _, errs := run(t, `{"worker":{}}`, tc.terminal, tc.args...)
		if code != exitcode.Usage || !strings.Contains(errs, "--yes") {
			t.Errorf("%s: want 2 asking for --yes, got %d %q", name, code, errs)
		}
	}
	if len(p.calls) != 0 {
		t.Fatalf("nothing may be sent before the invocation is valid: %v", p.calls)
	}
}

func TestValuesFromStdin(t *testing.T) {
	p, srv := newPortal(t)
	values := `{"worker": {"replicas": 3, "logLevel": "debug"}}`
	code, out, errs := run(t, values, false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL,
		"--values", "-", "--set-secret", "database.password=x", "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	got := p.body("POST "+chartsPath, 0)["values"]
	if mustJSON(t, got) != `{"worker":{"logLevel":"debug","replicas":3}}` {
		t.Fatalf("values from stdin not sent as given: %s", mustJSON(t, got))
	}
	if !strings.Contains(out, "installing example — follow it with zae addon status example") {
		t.Errorf("without --wait, say how to follow the install:\n%s", out)
	}
}

func TestDeclinedIsNotInstalled(t *testing.T) {
	p, srv := newPortal(t)
	code, out, _ := run(t, "n\n", true, "add", "https://example.org/charts/example-1.2.0.tgz", "--url", srv.URL,
		"--set-secret", "database.password=x")
	if code != exitcode.Failed || !strings.Contains(out, "not installed — example stays planned") {
		t.Fatalf("declined: want 1 saying what remains, got %d\n%s", code, out)
	}
	if p.called("POST " + chartsPath + "/example/install") {
		t.Fatal("a declined plan was installed")
	}
	if sent := p.body("POST "+chartsPath, 0); sent["chart"] != "https://example.org/charts/example-1.2.0.tgz" || sent["version"] != nil {
		t.Fatalf("an https archive is sent as its link, without a version: %v", sent)
	}
}

func TestAddRefusesAnInstalledAddon(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	code, _, errs := run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.3.0", "--url", srv.URL, "--yes")
	if code != exitcode.Failed || !strings.Contains(errs, "already installed") || !strings.Contains(errs, "zae addon upgrade example") {
		t.Fatalf("want 1 pointing at upgrade, got %d %q", code, errs)
	}
	if p.called("POST " + chartsPath) {
		t.Fatal("add must not rewrite an installed addon")
	}
}

// A portal reading through a cache may 404 an addon it just created.
func TestPlanWaitRidesOutCacheLag(t *testing.T) {
	p, srv := newPortal(t)
	p.hideCreated = 3
	code, out, errs := run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL,
		"--set-secret", "database.password=x", "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0 despite early 404s, got %d\n%s\n%s", code, out, errs)
	}
}

func TestWaitTimesOutWhenNeverReady(t *testing.T) {
	p, srv := newPortal(t)
	p.stepsToReady = 1 << 30
	code, out, errs := run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL,
		"--set-secret", "database.password=x", "--yes", "--wait", "--timeout", "40ms")
	if code != exitcode.Failed || !strings.Contains(errs, "is not Ready after 40ms (last phase Installing)") {
		t.Fatalf("want 1 after the timeout naming the phase, got %d %q", code, errs)
	}
	if !strings.Contains(out, "example-worker 0/1 (ContainerCreating)") {
		t.Errorf("progress must show component readiness:\n%s", out)
	}
}

func TestList(t *testing.T) {
	p, srv := newPortal(t)
	p.listing = `[
	  {"key":"example","title":"example","proxyUrl":"http://example","version":"1.2.0",
	   "chart":{"ref":"oci://ghcr.io/example/charts/example","version":"1.2.0"},"phase":"Ready","suspended":false,
	   "components":[{"name":"example","ready":1,"desired":1},{"name":"worker","ready":0,"desired":1}]},
	  {"key":"other","title":"other","proxyUrl":"http://other","version":"0.4.0",
	   "components":[{"name":"other","ready":null,"desired":null}]}
	]`
	code, out, errs := run(t, "", false, "list", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d %q", code, errs)
	}
	for _, re := range []string{
		`NAME\s+SOURCE\s+VERSION\s+PHASE\s+READY`,
		`example\s+oci://ghcr.io/example/charts/example\s+1\.2\.0\s+Ready\s+1/2`,
		`other\s+http://other\s+0\.4\.0\s+installed\s+0/1`,
	} {
		if !regexpLine(out, re) {
			t.Errorf("list lacks a line matching %s:\n%s", re, out)
		}
	}
	code, out, _ = run(t, "", false, "list", "--url", srv.URL, "--json")
	var rows []map[string]any
	if code != exitcode.OK || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 2 {
		t.Fatalf("--json must print the portal's JSON: %d %q", code, out)
	}
}

func TestStatus(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	code, out, errs := run(t, "", false, "status", "example", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d %q", code, errs)
	}
	for _, re := range []string{
		`^example — Ready$`,
		`running\s+oci://ghcr.io/example/charts/example 1\.2\.0 \(sha256:a+\)`,
		`example-worker\s+Deployment\s+1/1`,
	} {
		if !regexpLine(out, re) {
			t.Errorf("status lacks a line matching %s:\n%s", re, out)
		}
	}
	code, _, errs = run(t, "", false, "status", "nosuch", "--url", srv.URL)
	if code != exitcode.NotOffered || !strings.Contains(errs, `has no addon "nosuch"`) {
		t.Fatalf("absent addon: want 3, got %d %q", code, errs)
	}
}

func TestUpgradeShowsChangesAndWaitsForTheNewVersion(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	p.stepsToPlan, p.stepsToReady = 3, 2
	code, out, errs := run(t, "", false, "upgrade", "example", "--version", "1.3.0", "--url", srv.URL, "--yes", "--wait")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if got := mustJSON(t, p.body("PATCH "+chartsPath+"/example", 0)); got != `{"version":"1.3.0"}` {
		t.Fatalf("PATCH body: %s", got)
	}
	for _, s := range []string{
		"plan for example — chart example 1.3.0 — Planned",
		"~ Deployment/example: ghcr.io/example/example:1.2.0 → ghcr.io/example/example:1.3.0",
		"example is Ready",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "chart example 1.2.0") {
		t.Errorf("the status from before the change was shown as the plan:\n%s", out)
	}
	if v := p.addons["example"].LastAppliedChart.Version; v != "1.3.0" {
		t.Fatalf("Ready was accepted before the new version ran: %s", v)
	}
}

// A plan-only addon already shows a plan: the plan for the old version must
// not be taken for the answer.
func TestUpgradeOfAPlannedAddonWaitsForTheNewPlan(t *testing.T) {
	p, srv := newPortal(t)
	f := p.running("example", "1.2.0")
	f.LastAppliedChart, f.Components, f.Suspended, f.Phase = nil, nil, true, PhasePlanned
	p.stepsToPlan = 4
	code, out, errs := run(t, "", false, "upgrade", "example", "--version", "1.3.0", "--url", srv.URL, "--yes")
	if code != exitcode.OK || !strings.Contains(out, "chart example 1.3.0") || strings.Contains(out, "chart example 1.2.0") {
		t.Fatalf("want the 1.3.0 plan only, got %d\n%s\n%s", code, out, errs)
	}
}

func TestDeclinedUpgradePutsTheRunningAddonBack(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	code, out, _ := run(t, "no\n", true, "upgrade", "example", "--version", "1.3.0", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(out, "not upgraded") || !strings.Contains(out, "example is back at") {
		t.Fatalf("declined upgrade: want 1 and the addon restored, got %d\n%s", code, out)
	}
	if got := mustJSON(t, p.body("PATCH "+chartsPath+"/example", 1)); got != `{"suspend":false,"version":"1.2.0"}` {
		t.Fatalf("restore PATCH: %s", got)
	}
	if p.called("POST " + chartsPath + "/example/install") {
		t.Fatal("a declined upgrade was installed")
	}
	if f := p.addons["example"]; f.Suspended || f.version != "1.2.0" {
		t.Fatalf("the addon was left suspended at the new version: %+v", f)
	}
}

func TestUpgradeOfAnArchiveNeedsANewLink(t *testing.T) {
	p, srv := newPortal(t)
	f := p.running("example", "1.2.0")
	f.LastAppliedChart.Ref, f.chart = "https://example.org/charts/example-1.2.0.tgz", "https://example.org/charts/example-1.2.0.tgz"
	code, _, errs := run(t, "", false, "upgrade", "example", "--version", "1.3.0", "--url", srv.URL, "--yes")
	if code != exitcode.Usage || !strings.Contains(errs, "--chart") || p.called("PATCH "+chartsPath+"/example") {
		t.Fatalf("an archive cannot move by version: want 2 pointing at --chart, got %d %q", code, errs)
	}
	code, out, errs := run(t, "", false, "upgrade", "example", "--chart", "https://example.org/charts/example-1.3.0.tgz",
		"--url", srv.URL, "--yes", "--wait")
	if code != exitcode.OK || !strings.Contains(out, "example is Ready") {
		t.Fatalf("upgrade by link: want 0, got %d\n%s\n%s", code, out, errs)
	}
	if got := mustJSON(t, p.body("PATCH "+chartsPath+"/example", 0)); got != `{"chart":"https://example.org/charts/example-1.3.0.tgz"}` {
		t.Fatalf("PATCH body: %s", got)
	}
}

func TestUpgradeToWhatRunsIsANoop(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	code, out, _ := run(t, "", false, "upgrade", "example", "--version", "1.2.0", "--url", srv.URL, "--yes")
	if code != exitcode.OK || !strings.Contains(out, "already runs") || p.called("PATCH "+chartsPath+"/example") {
		t.Fatalf("want 0 without a write, got %d\n%s", code, out)
	}
}

func TestRemove(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	code, out, _ := run(t, "n\n", true, "remove", "example", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(out, "nothing removed") || p.called("DELETE "+chartsPath+"/example") {
		t.Fatalf("declined remove: want 1 without a DELETE, got %d\n%s", code, out)
	}
	code, out, errs := run(t, "", false, "remove", "example", "--url", srv.URL, "--keep-values", "--yes")
	if code != exitcode.OK || !strings.Contains(out, "removed example") {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "zaentrum-addon-example-values is kept") || !strings.Contains(out, "a new install generates new ones") {
		t.Errorf("remove must say what goes and what stays:\n%s", out)
	}
	if !p.called("DELETE " + chartsPath + "/example") {
		t.Fatal("no DELETE was sent")
	}
	code, _, errs = run(t, "", false, "remove", "example", "--url", srv.URL, "--yes")
	if code != exitcode.NotOffered {
		t.Fatalf("removing an absent addon: want 3, got %d %q", code, errs)
	}
}

func TestRemoveSendsKeepValues(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			query = r.URL.RawQuery
		}
		fmt.Fprint(w, `{"name":"example","phase":"Ready"}`)
	}))
	defer srv.Close()
	for keep, want := range map[bool]string{true: "keepValues=true", false: "keepValues=false"} {
		args := []string{"remove", "example", "--url", srv.URL, "--yes"}
		if keep {
			args = append(args, "--keep-values")
		}
		if code, _, errs := run(t, "", false, args...); code != exitcode.OK || query != want {
			t.Errorf("keep=%v: want %q, got %q (exit %d %q)", keep, want, query, code, errs)
		}
	}
}

func TestForbiddenUnreachableAndOldPortals(t *testing.T) {
	p, srv := newPortal(t)
	p.token = "the-admin-bearer"
	t.Setenv(instance.TokenEnv, "")
	code, _, errs := run(t, "", false, "status", "example", "--url", srv.URL)
	if code != exitcode.Forbidden || !strings.Contains(errs, "supply a bearer via "+instance.TokenEnv) {
		t.Fatalf("no bearer: want 5 naming %s, got %d %q", instance.TokenEnv, code, errs)
	}

	code, _, errs = run(t, "", false, "list", "--url", "http://127.0.0.1:1")
	if code != exitcode.Undetermined || !strings.Contains(errs, "not concluding") {
		t.Fatalf("unreachable: want 4, got %d %q", code, errs)
	}

	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	code, _, errs = run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", old.URL, "--yes")
	if code != exitcode.NotOffered || !strings.Contains(errs, "does not serve "+chartsPath) {
		t.Fatalf("a portal without the chart API: want 3, got %d %q", code, errs)
	}
}

func TestUsageErrors(t *testing.T) {
	_, srv := newPortal(t)
	for name, args := range map[string][]string{
		"no command":             {},
		"unknown command":        {"install"},
		"no chart":               {"add", "--url", srv.URL, "--yes"},
		"no url":                 {"add", "oci://ghcr.io/example/charts/example:1.2.0", "--yes"},
		"oci without version":    {"add", "oci://ghcr.io/example/charts/example", "--url", srv.URL, "--yes"},
		"plain http chart":       {"add", "http://example.org/example-1.2.0.tgz", "--url", srv.URL, "--yes"},
		"bad digest":             {"add", "oci://ghcr.io/example/charts/example:1.2.0", "--digest", "md5:x", "--url", srv.URL, "--yes"},
		"bad name":               {"add", "oci://ghcr.io/example/charts/example:1.2.0", "--name", "Example", "--url", srv.URL, "--yes"},
		"platform values":        {"add", "oci://ghcr.io/example/charts/example:1.2.0", "--set", "zaentrum.hostname=x", "--url", srv.URL, "--yes"},
		"value and secret":       {"add", "oci://ghcr.io/example/charts/example:1.2.0", "--set", "a.b=1", "--set-secret", "a.b=2", "--url", srv.URL, "--yes"},
		"upgrade without target": {"upgrade", "example", "--url", srv.URL, "--yes"},
		"status two names":       {"status", "a", "b", "--url", srv.URL},
	} {
		if code, _, errs := run(t, "", false, args...); code != exitcode.Usage {
			t.Errorf("%s: want 2, got %d %q", name, code, errs)
		}
	}
}

// Each rule that keeps a stale status from being read as the new plan.
func TestPlanAnswered(t *testing.T) {
	plan := func(version string) *Plan { return &Plan{Chart: PlanChart{Name: "example", Version: version}} }
	failed := &Addon{Phase: PhaseFailed, Suspended: true}
	for name, tc := range map[string]struct {
		before  *Addon
		version string
		doc     Addon
		want    bool
	}{
		"planned":                        {nil, "1.2.0", Addon{Phase: PhasePlanned, Suspended: true, Plan: plan("1.2.0")}, true},
		"plan failed is an answer":       {nil, "", Addon{Phase: PhasePlanFailed, Suspended: true}, true},
		"pending":                        {nil, "", Addon{Phase: PhasePending, Suspended: true}, false},
		"not suspended yet":              {nil, "", Addon{Phase: PhasePlanned, Plan: plan("1.2.0")}, false},
		"running phase from before":      {nil, "1.3.0", Addon{Phase: PhaseReady, Suspended: true, Plan: plan("1.2.0")}, false},
		"plan for another version":       {nil, "1.3.0", Addon{Phase: PhasePlanned, Suspended: true, Plan: plan("1.2.0")}, false},
		"failed before and after":        {failed, "", Addon{Phase: PhaseFailed, Suspended: true}, false},
		"failed, plan names the version": {failed, "1.3.0", Addon{Phase: PhaseFailed, Suspended: true, Plan: plan("1.3.0")}, true},
		"generation says stale":          {nil, "", Addon{Phase: PhasePlanned, Suspended: true, Generation: 3, ObservedGeneration: 2}, false},
		"generation says current":        {failed, "", Addon{Phase: PhaseFailed, Suspended: true, Generation: 3, ObservedGeneration: 3}, true},
	} {
		if got := planAnswered(tc.before, tc.version)(&tc.doc); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
