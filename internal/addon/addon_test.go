package addon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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

const (
	secretValue = "s3cret-value-never-printed"
	digestA     = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakePortal is portal-api and the operator in miniature. Writes behave like
// portal-api's: they answer with the generation they made, and every change to
// the spec — secret inputs included — bumps it. Reads advance the operator: a
// generation is observed after stepsToPlan reads (until then the status from
// before is served), an installed addon is Installing for stepsToReady reads,
// then Ready, then registered after stepsToRegister more.
type fakePortal struct {
	t      *testing.T
	mu     sync.Mutex
	addons map[string]*fakeAddon
	calls  []string
	bodies map[string][]string

	stepsToPlan, stepsToReady, stepsToRegister int
	hideCreated                                int    // 404s served for a just-created addon, like a lagging cache
	token                                      string // required bearer, "" for none
	listing                                    string
	unavailable                                string // a note: every chart endpoint answers 503 with it
	registrationError                          string // set instead of registering
	deleteWarnings                             []string

	planFor func(f *fakeAddon) *Plan
	onGet   func(f *fakeAddon, n int) // n counts reads of that addon
	onPatch func(f *fakeAddon)
}

type fakeAddon struct {
	name        string
	chart       Chart
	values      json.RawMessage
	secrets     map[string]string // dotted path → where it is set ("value" or a ref)
	suspend     bool
	generation  int64
	observed    int64
	phase       string
	message     string
	plan        *Plan
	components  []Component
	lastApplied *Chart
	registered  bool
	regError    string
	steps       int
	reads       int
	hidden      int
}

func (f *fakeAddon) doc() Addon {
	chart := f.chart
	keys := make([]string, 0, len(f.secrets))
	for k := range f.secrets {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return Addon{Name: f.name, Chart: &chart, Suspended: f.suspend, Phase: f.phase, Message: f.message,
		Plan: f.plan, Components: f.components, LastAppliedChart: f.lastApplied, Values: f.values,
		SecretKeys: keys, Generation: f.generation, ObservedGeneration: f.observed,
		Registered: f.registered, RegistrationError: f.regError}
}

func (f *fakeAddon) accepted() accepted {
	chart := f.chart
	return accepted{Name: f.name, Generation: f.generation, ObservedGeneration: f.observed, Chart: &chart}
}

func (f *fakeAddon) bump() { f.generation++; f.steps = 0 }

func newPortal(t *testing.T) (*fakePortal, *httptest.Server) {
	p := &fakePortal{t: t, addons: map[string]*fakeAddon{}, bodies: map[string][]string{}, planFor: examplePlan,
		stepsToPlan: 1, stepsToReady: 1, stepsToRegister: 1, listing: "[]"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+chartsPath, p.probe)
	mux.HandleFunc("POST "+chartsPath, p.create)
	mux.HandleFunc("GET "+chartsPath+"/{name}", p.get)
	mux.HandleFunc("PATCH "+chartsPath+"/{name}", p.patch)
	mux.HandleFunc("DELETE "+chartsPath+"/{name}", p.remove)
	mux.HandleFunc("POST "+chartsPath+"/{name}/install", p.install)
	mux.HandleFunc("GET "+addonsPath, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, p.listing) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		defer p.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		p.calls = append(p.calls, key)
		p.bodies[key] = append(p.bodies[key], string(body))
		if p.token != "" && r.Header.Get("Authorization") != "Bearer "+p.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if p.unavailable != "" && !(r.Method == http.MethodGet && r.URL.Path == chartsPath) {
			http.Error(w, p.unavailable, http.StatusServiceUnavailable)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return p, srv
}

// examplePlan renders the example chart for the addon's spec: two workloads,
// and a values error while the required secret input is missing.
func examplePlan(f *fakeAddon) *Plan {
	p := &Plan{
		Chart: PlanChart{Name: "example", Version: f.chart.Version, AppVersion: "4.1.0",
			Description: "An example addon", Digest: digestA},
		ValuesSchema: schemaDoc(exampleSchema),
		Objects: []Object{{Kind: "Deployment", Name: "example"}, {Kind: "Deployment", Name: "example-worker"},
			{Kind: "Service", Name: "example"}, {Kind: "Secret", Name: "example"}},
		Workloads: []Workload{
			{Kind: "Deployment", Name: "example", Images: []string{"ghcr.io/example/example:" + f.chart.Version},
				Ports: []json.RawMessage{json.RawMessage(`8080`)}},
			{Kind: "Deployment", Name: "example-worker", Images: []string{"ghcr.io/example/worker:" + f.chart.Version}},
		},
	}
	if f.lastApplied != nil && f.lastApplied.Version != f.chart.Version {
		p.Changes.Images = []string{fmt.Sprintf("Deployment/example: ghcr.io/example/example:%s → ghcr.io/example/example:%s", f.lastApplied.Version, f.chart.Version)}
	}
	if _, ok := f.secrets["database.password"]; !ok {
		p.ValuesErrors = []string{"database: password is required"}
	}
	return p
}

func (p *fakePortal) probe(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, `{"available":%t,"note":%q}`, p.unavailable == "", p.unavailable)
}

func (p *fakePortal) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name         string               `json:"name"`
		Chart        string               `json:"chart"`
		Version      string               `json:"version"`
		Digest       string               `json:"digest"`
		Values       json.RawMessage      `json:"values"`
		SecretValues map[string]string    `json:"secretValues"`
		SecretRefs   map[string]secretRef `json:"secretRefs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || body.Chart == "" {
		http.Error(w, "name and chart are required", http.StatusBadRequest)
		return
	}
	f := p.addons[body.Name]
	if f == nil {
		f = &fakeAddon{name: body.Name, secrets: map[string]string{}, hidden: p.hideCreated}
		p.addons[body.Name] = f
	}
	f.chart = Chart{Ref: body.Chart, Version: body.Version, Digest: body.Digest}
	f.values = body.Values
	for k := range body.SecretValues {
		f.secrets[k] = "value"
	}
	for k, ref := range body.SecretRefs {
		f.secrets[k] = ref.Name + "/" + ref.Key
	}
	f.suspend = true
	f.bump()
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(f.accepted())
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
	f.reads++
	if p.onGet != nil {
		p.onGet(f, f.reads)
	}
	switch {
	case f.observed < f.generation:
		if f.steps++; f.steps <= p.stepsToPlan {
			break // the operator has not reached this generation yet
		}
		f.observed = f.generation
		f.plan = p.planFor(f)
		blocked := len(f.plan.Violations)+len(f.plan.ValuesErrors) > 0
		switch {
		case f.suspend && blocked:
			f.phase = PhasePlanFailed
		case f.suspend:
			f.phase = PhasePlanned
		case blocked:
			f.phase = PhaseFailed
		default:
			f.phase, f.steps, f.registered = PhaseInstalling, 0, false
			f.components = []Component{{Name: "example", Kind: "Deployment", Desired: 1},
				{Name: "example-worker", Kind: "Deployment", Desired: 1, Reason: "ContainerCreating"}}
		}
	case f.phase == PhaseInstalling:
		if f.steps++; f.steps > p.stepsToReady {
			f.phase, f.steps = PhaseReady, 0
			applied := f.chart
			applied.Digest = digestA
			f.lastApplied = &applied
			f.components = []Component{{Name: "example", Kind: "Deployment", Ready: 1, Desired: 1},
				{Name: "example-worker", Kind: "Deployment", Ready: 1, Desired: 1}}
		}
	case f.phase == PhaseReady && !f.registered:
		if f.steps++; f.steps > p.stepsToRegister {
			if p.registrationError != "" {
				f.regError = p.registrationError
			} else {
				f.registered = true
			}
		}
	}
	_ = json.NewEncoder(w).Encode(f.doc())
}

func (p *fakePortal) install(w http.ResponseWriter, r *http.Request) {
	f := p.addons[r.PathValue("name")]
	if f == nil {
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	if f.plan == nil || f.observed != f.generation || len(f.plan.Violations)+len(f.plan.ValuesErrors) > 0 {
		http.Error(w, "the current plan cannot be installed", http.StatusConflict)
		return
	}
	if f.suspend {
		f.suspend = false
		f.bump()
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(f.accepted())
}

func (p *fakePortal) patch(w http.ResponseWriter, r *http.Request) {
	f := p.addons[r.PathValue("name")]
	if f == nil {
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	var body struct {
		Chart, Version, Digest *string
		Values                 json.RawMessage
		SecretValues           map[string]string
		ClearSecrets           []string
		Suspend                *bool
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	before := fmt.Sprintf("%+v|%s|%v|%t", f.chart, f.values, len(f.secrets), f.suspend)
	next := f.chart
	if body.Chart != nil {
		next.Ref = *body.Chart
	}
	if body.Version != nil {
		next.Version = *body.Version
	}
	if body.Digest != nil {
		next.Digest = *body.Digest
	} else if next.Ref != f.chart.Ref || next.Version != f.chart.Version {
		next.Digest = "" // a digest pins one archive; another chart is not it
	}
	chartChanged := next != f.chart
	f.chart = next
	if body.Values != nil {
		if string(body.Values) == "null" {
			f.values = nil
		} else {
			f.values = body.Values
		}
	}
	secretsWritten := len(body.SecretValues) > 0 || len(body.ClearSecrets) > 0
	for k := range body.SecretValues {
		f.secrets[k] = "value"
	}
	for _, k := range body.ClearSecrets {
		delete(f.secrets, k)
	}
	switch {
	case body.Suspend != nil:
		f.suspend = *body.Suspend
	case chartChanged:
		f.suspend = true
	}
	// A secret write creates a new Secret and repoints valuesFrom: always a change.
	if secretsWritten || before != fmt.Sprintf("%+v|%s|%v|%t", f.chart, f.values, len(f.secrets), f.suspend) {
		f.bump()
	}
	if p.onPatch != nil {
		p.onPatch(f)
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(f.accepted())
}

func (p *fakePortal) remove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if p.addons[name] == nil {
		http.Error(w, "no such addon", http.StatusNotFound)
		return
	}
	delete(p.addons, name)
	keep := r.URL.Query().Get("keepValues") == "true"
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "resource": true, "keptValues": keep, "warnings": p.deleteWarnings})
}

func (p *fakePortal) called(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c == key {
			n++
		}
	}
	return n
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

func (p *fakePortal) addon(name string) *fakeAddon {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addons[name]
}

// running seeds an addon that is installed, Ready and registered at version.
func (p *fakePortal) running(name, version string) *fakeAddon {
	chart := Chart{Ref: "oci://ghcr.io/example/charts/example", Version: version, Digest: digestA}
	applied := chart
	f := &fakeAddon{name: name, chart: chart, lastApplied: &applied, phase: PhaseReady, registered: true,
		generation: 3, observed: 3, values: json.RawMessage(`{"worker":{"replicas":2}}`),
		secrets: map[string]string{"database.password": "value"},
		components: []Component{{Name: "example", Kind: "Deployment", Ready: 1, Desired: 1},
			{Name: "example-worker", Kind: "Deployment", Ready: 1, Desired: 1}}}
	f.plan = examplePlan(f)
	p.addons[name] = f
	return f
}

// Test doubles for the terminal and the signals.
var (
	sigMu      sync.Mutex
	sigChannel chan<- os.Signal
)

func interrupt() {
	sigMu.Lock()
	defer sigMu.Unlock()
	if sigChannel != nil {
		select {
		case sigChannel <- os.Interrupt:
		default:
		}
	}
}

// run executes `zae addon …` with stdin input, a terminal or not, and the
// answers a secret prompt would get.
func run(t *testing.T, input string, terminal bool, args ...string) (code int, out, errs string) {
	t.Helper()
	var ob, eb bytes.Buffer
	stdout, stderr, stdin = &ob, &eb, strings.NewReader(input)
	canAsk = func() bool { return terminal }
	pollInterval, notFoundGrace = time.Millisecond, 200*time.Millisecond
	notifySignals = func(c chan<- os.Signal) func() {
		sigMu.Lock()
		sigChannel = c
		sigMu.Unlock()
		return func() {}
	}
	defer func() {
		stdout, stderr, stdin = os.Stdout, os.Stderr, os.Stdin
		canAsk, pollInterval, notFoundGrace = stdinIsTerminal, 2*time.Second, 15*time.Second
		sigMu.Lock()
		sigChannel = nil
		sigMu.Unlock()
	}()
	code = Run(args)
	return code, ob.String(), eb.String()
}

func regexpLine(out, re string) bool { return regexp.MustCompile("(?m)" + re).MatchString(out) }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAddPlansConfirmsInstallsAndWaitsUntilRegistered(t *testing.T) {
	p, srv := newPortal(t)
	p.token = "admin-bearer"
	p.stepsToPlan, p.stepsToReady, p.stepsToRegister = 2, 2, 2
	t.Setenv(instance.TokenEnv, "admin-bearer")
	valuesFile := writeFile(t, "values.json", `{"worker":{"logLevel":"info"}}`)
	keyFile := writeFile(t, "key", "file-secret-value\n")

	code, out, errs := run(t, "y\n", true, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL,
		"--values", valuesFile, "--set", "worker.replicas=2",
		"--set-secret", "database.password="+secretValue, "--set-secret-file", "api.key="+keyFile, "--wait")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	sent := p.body("POST "+chartsPath, 0)
	want := map[string]any{
		"name": "example", "chart": "oci://ghcr.io/example/charts/example", "version": "1.2.0",
		"values":       map[string]any{"worker": map[string]any{"logLevel": "info", "replicas": float64(2)}},
		"secretValues": map[string]any{"database.password": secretValue, "api.key": "file-secret-value"},
	}
	if got := mustJSON(t, sent); got != mustJSON(t, want) {
		t.Fatalf("POST body:\n got %s\nwant %s", got, mustJSON(t, want))
	}
	for _, s := range []string{
		"plan for example — chart example 1.2.0 — Planned",
		"ghcr.io/example/example:1.2.0", "ports 8080", "ghcr.io/example/worker:1.2.0",
		"objects     4 (Deployment 2, Secret 1, Service 1)",
		"config.key", "generated by the operator",
		"install example (chart example 1.2.0) on " + srv.URL + "? [y/N]",
		"registering in the portal",
		"example is Ready and registered",
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
	if strings.Contains(out+errs, secretValue) || strings.Contains(out+errs, "file-secret-value") {
		t.Fatalf("a secret value was printed:\n%s\n%s", out, errs)
	}
	if p.called("POST "+chartsPath+"/example/install") != 1 {
		t.Fatalf("install was not called exactly once: %v", p.calls)
	}
}

// The prompt is the input no other local user can read: nothing on the
// command line, nothing echoed.
func TestSecretInputsFromAPromptAndFromStdin(t *testing.T) {
	p, srv := newPortal(t)
	var prompts []string
	readSecret = func(s *session, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "typed-secret", nil
	}
	defer func() { readSecret = readSecretOriginal }()

	code, out, errs := run(t, "", true, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL,
		"--set-secret", "database.password", "--yes")
	if code != exitcode.OK || len(prompts) != 1 || !strings.Contains(prompts[0], "database.password") {
		t.Fatalf("want one prompt and 0, got %d prompts=%q\n%s\n%s", code, prompts, out, errs)
	}
	if got := p.body("POST "+chartsPath, 0)["secretValues"]; mustJSON(t, got) != `{"database.password":"typed-secret"}` {
		t.Fatalf("prompted secret not sent: %s", mustJSON(t, got))
	}

	p2, srv2 := newPortal(t)
	code, out, errs = run(t, `{"database.password": "piped-secret"}`, false, "add", "oci://ghcr.io/example/charts/example:1.2.0",
		"--url", srv2.URL, "--secret-values", "-", "--yes")
	if code != exitcode.OK {
		t.Fatalf("--secret-values -: want 0, got %d\n%s\n%s", code, out, errs)
	}
	if got := p2.body("POST "+chartsPath, 0)["secretValues"]; mustJSON(t, got) != `{"database.password":"piped-secret"}` {
		t.Fatalf("secret values from stdin not sent: %s", mustJSON(t, got))
	}
	if strings.Contains(out+errs, "piped-secret") {
		t.Fatal("a secret value from stdin was printed")
	}
}

var readSecretOriginal = readSecret

func TestSecretRefsReuseKeptSecrets(t *testing.T) {
	p, srv := newPortal(t)
	code, out, errs := run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL,
		"--secret-ref", "database.password=zaentrum-addon-example-values-x7k2p/database.password", "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	want := `{"database.password":{"key":"database.password","name":"zaentrum-addon-example-values-x7k2p"}}`
	if got := mustJSON(t, p.body("POST "+chartsPath, 0)["secretRefs"]); got != want {
		t.Fatalf("secretRefs: %s", got)
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
		"still to give: --set-secret database.password"} {
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
	if p.called("POST "+chartsPath+"/example/install") != 0 {
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
	if !strings.Contains(out, "✗ Deployment/example-worker: hostPath volume not allowed") {
		t.Errorf("violations must be printed:\n%s", out)
	}
	if p.called("POST "+chartsPath+"/example/install") != 0 {
		t.Fatal("--yes must not install a plan the guardrails refused")
	}
}

// Without a terminal, or with stdin needed twice, zae must not prompt — and
// must fail before it writes anything, so a script's mistake leaves nothing.
func TestStdinConflictsAreUsageAndWriteNothing(t *testing.T) {
	p, srv := newPortal(t)
	chart := "oci://ghcr.io/example/charts/example:1.2.0"
	for name, tc := range map[string]struct {
		terminal bool
		args     []string
		want     string
	}{
		"add, no terminal":          {false, []string{"add", chart, "--url", srv.URL}, "--yes"},
		"add, values stdin":         {true, []string{"add", chart, "--url", srv.URL, "--values", "-"}, "--yes"},
		"add, secret values stdin":  {true, []string{"add", chart, "--url", srv.URL, "--secret-values", "-"}, "--yes"},
		"add, stdin twice":          {true, []string{"add", chart, "--url", srv.URL, "--values", "-", "--secret-values", "-", "--yes"}, "cannot both"},
		"prompt without a terminal": {false, []string{"add", chart, "--url", srv.URL, "--set-secret", "database.password", "--yes"}, "--set-secret-file"},
		"prompt with a stdin file":  {true, []string{"add", chart, "--url", srv.URL, "--set-secret", "a", "--values", "-", "--yes"}, "stdin carries a document"},
		"upgrade, no terminal":      {false, []string{"upgrade", "example", "--version", "1.3.0", "--url", srv.URL}, "--yes"},
		"remove, no terminal":       {false, []string{"remove", "example", "--url", srv.URL}, "--yes"},
	} {
		code, _, errs := run(t, `{"worker":{}}`, tc.terminal, tc.args...)
		if code != exitcode.Usage || !strings.Contains(errs, tc.want) {
			t.Errorf("%s: want 2 mentioning %q, got %d %q", name, tc.want, code, errs)
		}
	}
	if len(p.calls) != 0 {
		t.Fatalf("nothing may be sent before the invocation is valid: %v", p.calls)
	}
}

func TestValuesFromStdin(t *testing.T) {
	p, srv := newPortal(t)
	code, out, errs := run(t, `{"worker": {"replicas": 3, "logLevel": "debug"}}`, false, "add",
		"oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL, "--values", "-", "--set-secret", "database.password=x", "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if got := mustJSON(t, p.body("POST "+chartsPath, 0)["values"]); got != `{"worker":{"logLevel":"debug","replicas":3}}` {
		t.Fatalf("values from stdin not sent as given: %s", got)
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
	if p.called("POST "+chartsPath+"/example/install") != 0 {
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
	if p.called("POST "+chartsPath) != 0 {
		t.Fatal("add must not rewrite an installed addon")
	}
}

// Adding a planned addon again makes a new generation; its old plan, still
// served until the operator catches up, must never be shown as the new one.
func TestStalePlanIsNotShown(t *testing.T) {
	p, srv := newPortal(t)
	f := &fakeAddon{name: "example", chart: Chart{Ref: "oci://ghcr.io/example/charts/example", Version: "1.1.0"},
		secrets: map[string]string{"database.password": "value"}, suspend: true, generation: 4, observed: 4, phase: PhasePlanned}
	f.plan = examplePlan(f)
	p.addons["example"] = f
	p.stepsToPlan = 4
	code, out, errs := run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL, "--yes")
	if code != exitcode.OK || !strings.Contains(out, "chart example 1.2.0") || strings.Contains(out, "chart example 1.1.0") {
		t.Fatalf("want the 1.2.0 plan only, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "secret inputs already set stay") {
		t.Errorf("adding again must say secret inputs are kept:\n%s", out)
	}
}

func TestSomeoneElsesChangeEndsTheWait(t *testing.T) {
	p, srv := newPortal(t)
	p.stepsToPlan = 5
	p.onGet = func(f *fakeAddon, n int) {
		if n == 2 {
			f.bump() // another admin writes the addon while zae waits
		}
	}
	code, _, errs := run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL,
		"--set-secret", "database.password=x", "--yes")
	if code != exitcode.Failed || !strings.Contains(errs, "changed while zae waited") {
		t.Fatalf("want 1 saying the addon changed, got %d %q", code, errs)
	}
	if p.called("POST "+chartsPath+"/example/install") != 0 {
		t.Fatal("zae installed a plan for someone else's change")
	}
}

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
		"--set-secret", "database.password=x", "--yes", "--wait", "--timeout", "150ms")
	if code != exitcode.Failed || !strings.Contains(errs, "is not Ready after 150ms (last phase Installing)") {
		t.Fatalf("want 1 after the timeout naming the phase, got %d %q", code, errs)
	}
	if !strings.Contains(out, "example-worker 0/1 (ContainerCreating)") {
		t.Errorf("progress must show component readiness:\n%s", out)
	}
}

// Ready is not done: until the portal has registered the addon, it has no app,
// tiles, rows or commands.
func TestWaitNeedsRegistration(t *testing.T) {
	p, srv := newPortal(t)
	p.registrationError = "the manifest at http://example names service \"other\""
	code, out, errs := run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL,
		"--set-secret", "database.password=x", "--yes", "--wait", "--timeout", "300ms")
	if code != exitcode.Failed || !strings.Contains(errs, "Ready but not registered") || !strings.Contains(errs, `names service "other"`) {
		t.Fatalf("want 1 with the registration error, got %d %q\n%s", code, errs, out)
	}
}

func TestList(t *testing.T) {
	p, srv := newPortal(t)
	p.listing = `[
	  {"key":"example","proxyUrl":"http://example","version":"4.1.0",
	   "chart":{"ref":"oci://ghcr.io/example/charts/example","version":"1.3.0",
	            "lastApplied":{"ref":"oci://ghcr.io/example/charts/example","version":"1.2.0"}},
	   "phase":"Planned","suspended":true,"registered":true,
	   "components":[{"name":"example","ready":1,"desired":1},{"name":"example-worker","ready":0,"desired":1}]},
	  {"key":"fresh","chart":{"ref":"https://example.org/charts/fresh-0.1.0.tgz","version":"","lastApplied":null},
	   "phase":"Ready","registered":false,"components":[]},
	  {"key":"other","proxyUrl":"http://other","version":"0.4.0","registered":true,
	   "components":[{"name":"other","ready":null,"desired":null}]}
	]`
	code, out, errs := run(t, "", false, "list", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d %q", code, errs)
	}
	for _, re := range []string{
		`NAME\s+SOURCE\s+VERSION\s+PHASE\s+READY`,
		`example\s+oci://ghcr.io/example/charts/example\s+1\.3\.0 \(runs 1\.2\.0\)\s+Planned \(suspended\)\s+1/2`,
		`fresh\s+https://example.org/charts/fresh-0\.1\.0\.tgz\s+- \(not running\)\s+Ready, not registered\s+-`,
		`other\s+http://other\s+0\.4\.0\s+installed\s+0/1`,
	} {
		if !regexpLine(out, re) {
			t.Errorf("list lacks a line matching %s:\n%s", re, out)
		}
	}
	code, out, _ = run(t, "", false, "list", "--url", srv.URL, "--json")
	var rows []map[string]any
	if code != exitcode.OK || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 3 {
		t.Fatalf("--json must print the portal's JSON: %d %q", code, out)
	}
}

func TestStatus(t *testing.T) {
	p, srv := newPortal(t)
	f := p.running("example", "1.2.0")
	f.chart.Version, f.suspend = "1.3.0", true
	code, out, errs := run(t, "", false, "status", "example", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d %q", code, errs)
	}
	for _, re := range []string{
		`^example — Ready · suspended`,
		`chart\s+oci://ghcr.io/example/charts/example 1\.3\.0`,
		`running\s+oci://ghcr.io/example/charts/example 1\.2\.0 \(sha256:a+\)`,
		`registered\s+yes`,
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
	if got := mustJSON(t, p.body("PATCH "+chartsPath+"/example", 0)); got != `{"suspend":true,"version":"1.3.0"}` {
		t.Fatalf("PATCH body: %s", got)
	}
	for _, s := range []string{
		"plan for example — chart example 1.3.0 — Planned",
		"~ Deployment/example: ghcr.io/example/example:1.2.0 → ghcr.io/example/example:1.3.0",
		"example is Ready and registered",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "chart example 1.2.0") {
		t.Errorf("the plan from before the change was shown:\n%s", out)
	}
	if v := p.addon("example").lastApplied.Version; v != "1.3.0" {
		t.Fatalf("Ready was accepted before the new version ran: %s", v)
	}
}

func TestUpgradeValuesAndSecretInputs(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	code, out, errs := run(t, "", false, "upgrade", "example", "--url", srv.URL, "--set", "worker.logLevel=debug",
		"--set-secret", "api.key=new-secret", "--clear-secret", "old.token", "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	want := `{"clearSecrets":["old.token"],"secretValues":{"api.key":"new-secret"},"suspend":true,"values":{"worker":{"logLevel":"debug","replicas":2}}}`
	if got := mustJSON(t, p.body("PATCH "+chartsPath+"/example", 0)); got != want {
		t.Fatalf("PATCH body:\n got %s\nwant %s", got, want)
	}
	if strings.Contains(out+errs, "new-secret") {
		t.Fatal("a secret value was printed")
	}
}

// Declining puts back everything the upgrade changed that zae could read:
// chart reference, version, digest, values and suspension.
func TestDeclinedUpgradePutsTheWholeSpecBack(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	code, out, errs := run(t, "no\n", true, "upgrade", "example", "--version", "1.3.0", "--digest", digestB,
		"--set", "worker.replicas=5", "--set-secret", "api.key=x", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(out, "not upgraded") || !strings.Contains(out, "example is back at oci://ghcr.io/example/charts/example 1.2.0") {
		t.Fatalf("declined upgrade: want 1 and the addon put back, got %d\n%s\n%s", code, out, errs)
	}
	want := `{"chart":"oci://ghcr.io/example/charts/example","digest":"` + digestA + `","suspend":false,"values":{"worker":{"replicas":2}},"version":"1.2.0"}`
	if got := mustJSON(t, p.body("PATCH "+chartsPath+"/example", 1)); got != want {
		t.Fatalf("restore PATCH:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(errs, "stay as written (api.key)") {
		t.Errorf("zae must say which secret inputs it cannot put back: %q", errs)
	}
	if p.called("POST "+chartsPath+"/example/install") != 0 {
		t.Fatal("a declined upgrade was installed")
	}
	if f := p.addon("example"); f.suspend || f.chart.Version != "1.2.0" || f.chart.Digest != digestA {
		t.Fatalf("the addon was not put back: %+v", f.chart)
	}
}

func TestRestoreLeavesSomeoneElsesChangeAlone(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	p.planFor = func(f *fakeAddon) *Plan {
		plan := examplePlan(f)
		plan.Violations = []string{"Service/example: Service type NodePort not allowed (ClusterIP only)"}
		return plan
	}
	p.onGet = func(f *fakeAddon, n int) {
		if f.observed == f.generation && f.phase == PhasePlanFailed {
			f.bump() // someone else writes after the plan was read, before the restore
		}
	}
	code, _, errs := run(t, "", false, "upgrade", "example", "--version", "1.3.0", "--url", srv.URL, "--yes")
	if code != exitcode.Failed || !strings.Contains(errs, "does not put it back") {
		t.Fatalf("want 1 and no restore, got %d %q", code, errs)
	}
	if n := p.called("PATCH " + chartsPath + "/example"); n != 1 {
		t.Fatalf("zae must not overwrite someone else's change: %d PATCHes", n)
	}
}

// A running addon asked for what it already runs, with the values it has:
// zae writes nothing — suspending it would re-plan and re-install for nothing.
func TestUpgradeToWhatItAsksForWritesNothing(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	code, out, _ := run(t, "", false, "upgrade", "example", "--version", "1.2.0", "--set", "worker.replicas=2", "--url", srv.URL, "--yes")
	if code != exitcode.OK || !strings.Contains(out, "nothing to upgrade") || p.called("PATCH "+chartsPath+"/example") != 0 {
		t.Fatalf("want 0 without a write, got %d\n%s", code, out)
	}
}

// The portal answers a PATCH that changed nothing with the generation the
// addon already had; zae stops there instead of waiting for a plan.
func TestUnchangedGenerationIsNothingToUpgrade(t *testing.T) {
	p, srv := newPortal(t)
	f := p.running("example", "1.2.0")
	f.suspend, f.lastApplied = true, nil
	gen := f.generation
	p.onPatch = func(a *fakeAddon) { a.generation, a.chart.Version = gen, "1.2.0" }
	code, out, errs := run(t, "", false, "upgrade", "example", "--version", "v1.2.0", "--url", srv.URL, "--yes")
	if code != exitcode.OK || !strings.Contains(out, "nothing to upgrade") {
		t.Fatalf("want 0 and nothing to upgrade, got %d\n%s\n%s", code, out, errs)
	}
	if n := p.called("GET " + chartsPath + "/example"); n != 1 {
		t.Fatalf("zae must not wait for a plan after a no-op PATCH: %d reads", n)
	}
}

func TestInterruptedUpgradeIsPutBack(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	p.stepsToPlan = 1 << 30 // the plan never comes; the interrupt does
	p.onPatch = func(f *fakeAddon) {
		if f.chart.Version == "1.3.0" {
			go func() { time.Sleep(20 * time.Millisecond); interrupt() }()
		}
	}
	code, out, errs := run(t, "", false, "upgrade", "example", "--version", "1.3.0", "--url", srv.URL, "--yes")
	if code != 130 || !strings.Contains(out, "interrupted — putting it back") || !strings.Contains(out, "example is back at") {
		t.Fatalf("want 130 after putting the addon back, got %d\n%s\n%s", code, out, errs)
	}
	if f := p.addon("example"); f.suspend || f.chart.Version != "1.2.0" {
		t.Fatalf("the addon was not put back: %+v suspended=%t", f.chart, f.suspend)
	}
}

func TestUpgradeOfAnArchiveNeedsANewLink(t *testing.T) {
	p, srv := newPortal(t)
	f := p.running("example", "1.2.0")
	f.chart = Chart{Ref: "https://example.org/charts/example-1.2.0.tgz"}
	f.lastApplied = &Chart{Ref: f.chart.Ref, Digest: digestA}
	code, _, errs := run(t, "", false, "upgrade", "example", "--version", "1.3.0", "--url", srv.URL, "--yes")
	if code != exitcode.Usage || !strings.Contains(errs, "--chart") || p.called("PATCH "+chartsPath+"/example") != 0 {
		t.Fatalf("an archive cannot move by version: want 2 pointing at --chart, got %d %q", code, errs)
	}
	code, out, errs := run(t, "", false, "upgrade", "example", "--chart", "https://example.org/charts/example-1.3.0.tgz",
		"--url", srv.URL, "--yes", "--wait")
	if code != exitcode.OK || !strings.Contains(out, "example is Ready and registered") {
		t.Fatalf("upgrade by link: want 0, got %d\n%s\n%s", code, out, errs)
	}
	if got := mustJSON(t, p.body("PATCH "+chartsPath+"/example", 0)); got != `{"chart":"https://example.org/charts/example-1.3.0.tgz","suspend":true}` {
		t.Fatalf("PATCH body: %s", got)
	}
}

func TestRemove(t *testing.T) {
	p, srv := newPortal(t)
	p.running("example", "1.2.0")
	code, out, _ := run(t, "n\n", true, "remove", "example", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(out, "nothing removed") || p.called("DELETE "+chartsPath+"/example") != 0 {
		t.Fatalf("declined remove: want 1 without a DELETE, got %d\n%s", code, out)
	}
	p.deleteWarnings = []string{"the addon's values Secrets could not be deleted: forbidden"}
	code, out, errs := run(t, "", false, "remove", "example", "--url", srv.URL, "--keep-values", "--yes")
	if code != exitcode.OK || !strings.Contains(out, "removed example — kept") {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	for _, s := range []string{"zaentrum-addon-example-values-", "zaentrum-addon-example-generated", "--secret-ref"} {
		if !strings.Contains(out, s) {
			t.Errorf("keeping must name both kept Secrets and how to reuse them (%q):\n%s", s, out)
		}
	}
	if !strings.Contains(errs, "zae: warning: the addon's values Secrets could not be deleted: forbidden") {
		t.Errorf("the portal's warnings must be printed: %q", errs)
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
			fmt.Fprintf(w, `{"name":"example","resource":true,"keptValues":%t,"warnings":[]}`, strings.Contains(query, "true"))
			return
		}
		fmt.Fprint(w, `{"name":"example","phase":"Ready"}`)
	}))
	defer srv.Close()
	for keep, want := range map[bool]string{true: "keepValues=true", false: "keepValues=false"} {
		args := []string{"remove", "example", "--url", srv.URL, "--yes"}
		if keep {
			args = append(args, "--keep-values")
		}
		code, out, errs := run(t, "", false, args...)
		if code != exitcode.OK || query != want {
			t.Errorf("keep=%v: want %q, got %q (exit %d %q)", keep, want, query, code, errs)
		}
		if !keep && !strings.Contains(out, "deleted with it") {
			t.Errorf("without --keep-values the Secrets go: %s", out)
		}
	}
}

// A cluster without the ZaentrumAddon resource is a definitive "cannot": exit
// 3, and no wait continues against it.
func TestNoChartAPIIsExit3(t *testing.T) {
	p, srv := newPortal(t)
	p.unavailable = "this cluster serves no ZaentrumAddon resource — update the zaentrum-operator to install addons from charts"
	code, _, errs := run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv.URL, "--yes")
	if code != exitcode.NotOffered || !strings.Contains(errs, "cannot install addons from charts") {
		t.Fatalf("want 3 with the portal's note, got %d %q", code, errs)
	}

	p2, srv2 := newPortal(t)
	p2.stepsToPlan = 1 << 30
	p2.onGet = func(f *fakeAddon, n int) {
		if n == 2 {
			p2.unavailable = "this cluster serves no ZaentrumAddon resource"
		}
	}
	start := time.Now()
	code, _, errs = run(t, "", false, "add", "oci://ghcr.io/example/charts/example:1.2.0", "--url", srv2.URL, "--yes", "--timeout", "5s")
	if code != exitcode.NotOffered || time.Since(start) > 2*time.Second {
		t.Fatalf("a wait must end at once: want 3 quickly, got %d after %s %q", code, time.Since(start), errs)
	}

	p3, srv3 := newPortal(t)
	p3.unavailable = "the cluster did not answer: timeout"
	code, _, _ = run(t, "", false, "status", "example", "--url", srv3.URL)
	if code != exitcode.Undetermined {
		t.Fatalf("a cluster that did not answer is undetermined: want 4, got %d", code)
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
	p, srv := newPortal(t)
	chart := "oci://ghcr.io/example/charts/example:1.2.0"
	long := strings.Repeat("a", 251)
	for name, args := range map[string][]string{
		"no command":             {},
		"unknown command":        {"install"},
		"no chart":               {"add", "--url", srv.URL, "--yes"},
		"no url":                 {"add", chart, "--yes"},
		"oci without version":    {"add", "oci://ghcr.io/example/charts/example", "--url", srv.URL, "--yes"},
		"oci digest in the ref":  {"add", "oci://ghcr.io/example/charts/example@" + digestA, "--url", srv.URL, "--yes"},
		"plain http chart":       {"add", "http://example.org/example-1.2.0.tgz", "--url", srv.URL, "--yes"},
		"bad digest":             {"add", chart, "--digest", "md5:x", "--url", srv.URL, "--yes"},
		"bad name":               {"add", chart, "--name", "Example", "--url", srv.URL, "--yes"},
		"name from the ref":      {"add", "https://example.org/charts/ex_ample-1.2.0.tgz", "--url", srv.URL, "--yes"},
		"platform values":        {"add", chart, "--set", "zaentrum.hostname=x", "--url", srv.URL, "--yes"},
		"value and secret":       {"add", chart, "--set", "a.b=1", "--set-secret", "a.b=2", "--url", srv.URL, "--yes"},
		"empty secret":           {"add", chart, "--set-secret", "a.b=", "--url", srv.URL, "--yes"},
		"secret path too long":   {"add", chart, "--set-secret", long + "=x", "--url", srv.URL, "--yes"},
		"secret file missing":    {"add", chart, "--set-secret-file", "a.b=/nonexistent/secret", "--url", srv.URL, "--yes"},
		"foreign secret ref":     {"add", chart, "--secret-ref", "a.b=platform-db/password", "--url", srv.URL, "--yes"},
		"ref and secret":         {"add", chart, "--secret-ref", "a.b=zaentrum-addon-example-values-x/a.b", "--set-secret", "a.b=2", "--url", srv.URL, "--yes"},
		"upgrade without change": {"upgrade", "example", "--url", srv.URL, "--yes"},
		"set and clear":          {"upgrade", "example", "--set-secret", "a.b=1", "--clear-secret", "a.b", "--url", srv.URL, "--yes"},
		"status two names":       {"status", "a", "b", "--url", srv.URL},
	} {
		code, _, errs := run(t, "", false, args...)
		if code != exitcode.Usage {
			t.Errorf("%s: want 2, got %d %q", name, code, errs)
		}
		if strings.Contains(errs, "hunter2") {
			t.Errorf("%s: a value was echoed", name)
		}
	}
	if len(p.calls) != 0 {
		t.Fatalf("usage errors must not reach the portal: %v", p.calls)
	}
}

// Each rule that keeps a stale or foreign status from being read as the answer.
func TestPlanForAndReadyFor(t *testing.T) {
	for name, tc := range map[string]struct {
		check   func(*Addon) (bool, error)
		doc     Addon
		want    bool
		changed bool
	}{
		"observed":                 {planFor("x", 5), Addon{Generation: 5, ObservedGeneration: 5, Suspended: true, Phase: PhasePlanned}, true, false},
		"not observed yet":         {planFor("x", 5), Addon{Generation: 5, ObservedGeneration: 4, Suspended: true, Phase: PhasePlanned}, false, false},
		"failed for this write":    {planFor("x", 5), Addon{Generation: 5, ObservedGeneration: 5, Suspended: true, Phase: PhaseFailed}, true, false},
		"someone else wrote":       {planFor("x", 5), Addon{Generation: 6, ObservedGeneration: 5, Suspended: true}, false, true},
		"no generation: planned":   {planFor("x", 0), Addon{Suspended: true, Phase: PhasePlanned}, true, false},
		"no generation: running":   {planFor("x", 0), Addon{Phase: PhaseReady}, false, false},
		"ready and registered":     {readyFor("x", 6), Addon{Generation: 6, ObservedGeneration: 6, Phase: PhaseReady, Registered: true}, true, false},
		"ready, not registered":    {readyFor("x", 6), Addon{Generation: 6, ObservedGeneration: 6, Phase: PhaseReady}, false, false},
		"ready from before":        {readyFor("x", 6), Addon{Generation: 6, ObservedGeneration: 5, Phase: PhaseReady, Registered: true}, false, false},
		"ready but changed since":  {readyFor("x", 6), Addon{Generation: 7, ObservedGeneration: 7, Phase: PhaseReady, Registered: true}, false, true},
		"still suspended at Ready": {readyFor("x", 6), Addon{Generation: 6, ObservedGeneration: 6, Phase: PhaseReady, Registered: true, Suspended: true}, false, false},
	} {
		got, err := tc.check(&tc.doc)
		_, isChanged := err.(*changedError)
		if got != tc.want || (err != nil) != tc.changed || isChanged != tc.changed {
			t.Errorf("%s: got %v, %v", name, got, err)
		}
	}
}

func TestTerminalIsNotAPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if terminal(r) {
		t.Fatal("a pipe is not a terminal")
	}
	if _, err := withoutEcho(r); err == nil {
		t.Fatal("echo cannot be turned off on a pipe")
	}
}
