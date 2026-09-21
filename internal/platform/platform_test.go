package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/creds"
	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// fakePortal is the portal's operator console in miniature. It answers the
// same shapes portal-api answers — including refusing a body field it does
// not know, which is how these tests prove zae never sends one — and it
// advances the world on every read, so a wait has something to converge to.
type fakePortal struct {
	mu     sync.Mutex
	calls  []string
	bodies map[string][]string
	auth   []string

	available bool
	op        Operator
	workloads []Workload
	listError string

	token string // required bearer, "" for none

	// legacy is a portal-api from before the exact wait: every write answers
	// 204 with nothing, and the console reports no generations. zae has to
	// keep working against it, and has to say that its wait is weaker.
	legacy bool
	// noTotal is the half-way portal: generations, but no total pod count.
	noTotal bool
	// stamps counts the restart stamps written, so each is a new one.
	stamps int

	reads       int
	onGet       func(p *fakePortal, n int)
	onWrite     func(p *fakePortal)
	beforeApply func(p *fakePortal)
}

func newPortal(t *testing.T) (*fakePortal, *httptest.Server) {
	p := &fakePortal{
		bodies:    map[string][]string{},
		available: true,
		op: Operator{Present: true, Name: "zaentrum", Channel: "stable", Version: "1.4.0",
			UpdateMode: "manual", Hostname: "media.example.org", Phase: "Ready",
			CurrentVersion: "1.4.0", AvailableUpdate: "1.5.0", Generation: 7, ObservedGeneration: 7},
		workloads: exampleWorkloads(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+operatorPath, p.get)
	mux.HandleFunc("PATCH "+operatorPath, p.patch)
	mux.HandleFunc("POST "+applyPath, p.applyUpdate)
	mux.HandleFunc("POST "+operatorPath+"/instances/{name}/scale", p.scale)
	mux.HandleFunc("POST "+operatorPath+"/instances/{name}/restart", p.restart)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		defer p.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		p.calls = append(p.calls, key)
		p.bodies[key] = append(p.bodies[key], string(body))
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		if p.token != "" && r.Header.Get("Authorization") != "Bearer "+p.token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return p, srv
}

// exampleWorkloads is what the portal lists, in the order it lists them: by
// name, with nothing grouped. Grouping them is zae's job.
func exampleWorkloads() []Workload {
	return []Workload{
		{Name: "chino-api", Image: "ghcr.io/example/chino-api:1.4.0", DesiredReplicas: 2, Replicas: pods(2),
			ReadyReplicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2, Phase: PhaseReady,
			OperatorManaged: true, Group: GroupPlatform,
			Generation: 3, ObservedGeneration: 3, RestartedAt: "2026-09-20T08:00:00Z"},
		{Name: "example-worker", Image: "ghcr.io/example/worker:2.0.0", DesiredReplicas: 1, Replicas: pods(1),
			ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, Phase: PhaseReady,
			Group: GroupAddon, Addon: "example", Generation: 1, ObservedGeneration: 1},
		{Name: "katalog-api", Image: "ghcr.io/example/katalog-api@sha256:" + strings.Repeat("a", 64),
			DesiredReplicas: 1, Replicas: pods(1), ReadyReplicas: 0, UpdatedReplicas: 1, Phase: PhaseDegraded,
			OperatorManaged: true, Group: GroupPlatform, Reason: "ImagePullBackOff",
			Generation: 2, ObservedGeneration: 2},
		{Name: "leftover", Image: "ghcr.io/example/leftover", DesiredReplicas: 1, Replicas: pods(1),
			ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, Phase: PhaseReady, Group: GroupOther,
			Generation: 1, ObservedGeneration: 1},
		{Name: "postgres", Image: "postgres:16", DesiredReplicas: 1, Replicas: pods(1), ReadyReplicas: 1,
			UpdatedReplicas: 1, AvailableReplicas: 1, Phase: PhaseReady, OperatorManaged: true,
			Group: GroupPlatform, Protected: true, Generation: 1, ObservedGeneration: 1},
	}
}

func pods(n int) *int { return &n }

func (p *fakePortal) get(w http.ResponseWriter, r *http.Request) {
	p.reads++
	if p.onGet != nil {
		p.onGet(p, p.reads)
	}
	if !p.available {
		writeJSON(w, map[string]any{"available": false, "instances": []any{},
			"operator": map[string]any{"present": false, "note": noManagement + " (not running in a cluster)"}})
		return
	}
	out := map[string]any{"available": true, "operator": p.strip(p.op), "instances": p.instances()}
	if p.listError != "" {
		out["error"] = p.listError
		out["instances"] = []any{}
	}
	writeJSON(w, out)
}

// instances renders the workload list the way this portal serves it — a legacy
// one has never heard of the rollout fields and sends none.
func (p *fakePortal) instances() []any {
	out := make([]any, 0, len(p.workloads))
	for _, wl := range p.workloads {
		out = append(out, p.strip(wl))
	}
	return out
}

// strip round-trips a DTO through JSON and, for a legacy portal, removes the
// fields it never had. Absent is not the same as zero on the wire, and zae's
// fallback has to be driven by what a real old portal sends.
func (p *fakePortal) strip(v any) map[string]any {
	raw, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if p.legacy {
		for _, k := range []string{"generation", "observedGeneration", "restartedAt", "replicas"} {
			delete(m, k)
		}
	}
	if p.noTotal {
		// The portal that reports generations but not the total pod count:
		// the one zae itself shipped first, and the one the live restart was
		// still declared finished against.
		delete(m, "replicas")
	}
	return m
}

// answer writes what a write produced, or nothing at all when this portal
// predates saying so.
func (p *fakePortal) answer(w http.ResponseWriter, v any) {
	if p.legacy {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, v)
}

func (p *fakePortal) patch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version    *string `json:"version"`
		Channel    *string `json:"channel"`
		UpdateMode *string `json:"updateMode"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Version == nil && body.Channel == nil && body.UpdateMode == nil {
		http.Error(w, "nothing to change", http.StatusBadRequest)
		return
	}
	if body.Version != nil {
		p.op.Version = *body.Version
	}
	if body.Channel != nil {
		p.op.Channel = *body.Channel
	}
	if body.UpdateMode != nil {
		p.op.UpdateMode = *body.UpdateMode
	}
	p.op.Generation++
	p.wrote()
	p.answer(w, updated{Version: p.op.Version, Generation: p.op.Generation})
}

func (p *fakePortal) applyUpdate(w http.ResponseWriter, r *http.Request) {
	// beforeApply is somebody else moving the shelf between the read zae based
	// its question on and the write that answers it.
	if p.beforeApply != nil {
		p.beforeApply(p)
	}
	// The body is optional, and a legacy portal ignores it entirely.
	var body struct {
		Version string `json:"version"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}
	switch {
	case p.op.AvailableUpdate == "":
		http.Error(w, "no update available", http.StatusBadRequest)
		return
	case !p.legacy && body.Version != "" && body.Version != p.op.AvailableUpdate:
		http.Error(w, fmt.Sprintf("the available update changed: %s is on the %s channel now, not %s",
			p.op.AvailableUpdate, p.op.Channel, body.Version), http.StatusConflict)
		return
	}
	p.op.Version = p.op.AvailableUpdate
	p.op.Generation++
	p.wrote()
	p.answer(w, updated{Version: p.op.Version, Generation: p.op.Generation})
}

func (p *fakePortal) scale(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Replicas int `json:"replicas"`
	}
	if !decode(w, r, &body) {
		return
	}
	wl := p.workload(r.PathValue("name"))
	switch {
	case wl == nil:
		http.Error(w, fmt.Sprintf("no such workload: %s", r.PathValue("name")), http.StatusNotFound)
	case wl.Protected:
		http.Error(w, fmt.Sprintf("%q is a protected (stateful) service and cannot be scaled from here",
			wl.Name), http.StatusBadRequest)
	case body.Replicas < 0 || body.Replicas > 20:
		http.Error(w, "replicas must be between 0 and 20", http.StatusBadRequest)
	default:
		// The count asked for moves; the pods do not, yet.
		wl.DesiredReplicas = body.Replicas
		wl.Phase = PhaseProgressing
		wl.Generation++
		p.wrote()
		p.answer(w, write{Name: wl.Name, Generation: wl.Generation, RestartedAt: wl.RestartedAt})
	}
}

// restart models the cluster exactly as it behaves, which is the whole reason
// this work exists: the write stamps the pod template and bumps the
// generation, and NOTHING about the replica counters changes. For the next
// several seconds they describe the pods from before the restart — ready,
// updated, available, all of it true of the wrong pods.
func (p *fakePortal) restart(w http.ResponseWriter, r *http.Request) {
	wl := p.workload(r.PathValue("name"))
	switch {
	case wl == nil:
		http.Error(w, fmt.Sprintf("no such workload: %s", r.PathValue("name")), http.StatusNotFound)
	case wl.Protected:
		http.Error(w, fmt.Sprintf("%q is a protected (stateful) service and cannot be restarted from here",
			wl.Name), http.StatusBadRequest)
	default:
		p.stamps++
		wl.Generation++
		wl.RestartedAt = fmt.Sprintf("2026-09-21T10:0%d:00Z", p.stamps)
		p.wrote()
		p.answer(w, write{Name: wl.Name, Generation: wl.Generation, RestartedAt: wl.RestartedAt})
	}
}

func (p *fakePortal) workload(name string) *Workload {
	for i := range p.workloads {
		if p.workloads[i].Name == name {
			return &p.workloads[i]
		}
	}
	return nil
}

func (p *fakePortal) wrote() {
	if p.onWrite != nil {
		p.onWrite(p)
	}
}

// ready brings every workload to its desired replica count and marks the
// cluster as having acted on the spec it has, as a rollout that finished
// would.
func (p *fakePortal) ready() {
	p.readyCounters()
	for i := range p.workloads {
		p.workloads[i].ObservedGeneration = p.workloads[i].Generation
	}
}

// readyCounters makes every counter say ready while leaving the generations
// where they are — the state a rollout that has not started yet reports,
// because those counters are about the pods from before it.
func (p *fakePortal) readyCounters() {
	for i := range p.workloads {
		w := &p.workloads[i]
		w.ReadyReplicas, w.UpdatedReplicas, w.AvailableReplicas = w.DesiredReplicas, w.DesiredReplicas, w.DesiredReplicas
		w.Replicas = pods(w.DesiredReplicas)
		w.Phase, w.Reason = PhaseReady, ""
		if w.DesiredReplicas == 0 {
			w.Phase = PhaseStopped
		}
	}
}

// surge is the live failure, reproduced: the rollout has created the new pod
// and the old one is still serving, so the TOTAL is one more than was asked
// for while every other counter reads exactly right.
func (p *fakePortal) surge(name string) {
	w := p.workload(name)
	w.ObservedGeneration = w.Generation
	w.Replicas = pods(w.DesiredReplicas + 1)
	w.UpdatedReplicas, w.ReadyReplicas, w.AvailableReplicas = w.DesiredReplicas, w.DesiredReplicas, w.DesiredReplicas
	w.Phase, w.Reason = PhaseReady, ""
}

func (p *fakePortal) called(key string) int {
	n := 0
	for _, c := range p.calls {
		if c == key {
			n++
		}
	}
	return n
}

// body decodes the n-th request body sent to key.
func (p *fakePortal) body(t *testing.T, key string, n int) map[string]any {
	t.Helper()
	bodies := p.bodies[key]
	if n >= len(bodies) {
		t.Fatalf("no request %d to %s (%d sent)", n, key, len(bodies))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(bodies[n]), &m); err != nil {
		t.Fatalf("%s body %d is not JSON: %v (%q)", key, n, err, bodies[n])
	}
	return m
}

// decode mirrors portal-api's own decoder, unknown fields refused and all.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// decodeOptional mirrors the decoder for a body a caller may leave out.
func decodeOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// Credentials are ambient: a stored session, or a ZAE_TOKEN in the shell
// running `go test`, would change what these tests see. Point the credentials
// file at a directory that starts empty and cannot be the developer's own.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zae-platform-test-config-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Unsetenv(instance.TokenEnv)
	code := m.Run()
	os.RemoveAll(dir) // os.Exit skips defers
	os.Exit(code)
}

// run executes `zae platform …` with stdin input and a terminal, or not.
func run(t *testing.T, input string, terminal bool, args ...string) (code int, out, errs string) {
	t.Helper()
	var ob, eb bytes.Buffer
	stdout, stderr, stdin = &ob, &eb, strings.NewReader(input)
	canAsk = func() bool { return terminal }
	pollInterval = time.Millisecond
	defer func() {
		stdout, stderr, stdin = os.Stdout, os.Stderr, os.Stdin
		canAsk, pollInterval = stdinIsTerminal, 2*time.Second
	}()
	return Run(args), ob.String(), eb.String()
}

func regexpLine(out, re string) bool { return regexp.MustCompile("(?m)" + re).MatchString(out) }

func TestStatusRendersThePlatformAndItsWorkloads(t *testing.T) {
	p, srv := newPortal(t)
	p.token = "admin-bearer"
	t.Setenv(instance.TokenEnv, "admin-bearer")

	code, out, errs := run(t, "", false, "status", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	for _, s := range []string{
		"version      1.4.0 — pinned",
		"channel      stable",
		"update mode  manual — an update is applied when someone asks for it",
		"phase        Ready",
		"running      1.4.0",
		"1.5.0 available — apply it with: zae platform update --apply --url " + srv.URL,
		"host         media.example.org",
		"NAME", "GROUP", "IMAGE", "READY", "PHASE", "REASON",
		"Not covered here: the operator's own controller image.",
		"applying its install bundle",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("status lacks %q:\n%s", s, out)
		}
	}
	// The tag, and the head of a digest where there is no tag.
	if !regexpLine(out, `chino-api\s+platform\s+1\.4\.0\s+2/2\s+ready`) {
		t.Errorf("chino-api row:\n%s", out)
	}
	if !regexpLine(out, `katalog-api\s+platform\s+sha256:a{12}\s+0/1\s+degraded\s+ImagePullBackOff`) {
		t.Errorf("katalog-api row (short digest, reason):\n%s", out)
	}
	if !regexpLine(out, `example-worker\s+addon:example\s+2\.0\.0\s+1/1`) {
		t.Errorf("addon row:\n%s", out)
	}
	if !regexpLine(out, `leftover\s+other\s+latest\s+1/1`) {
		t.Errorf("unclaimed row, image with no tag:\n%s", out)
	}
	// Platform first, addons after, unclaimed last — whatever order the
	// portal listed them in.
	order := []string{"chino-api", "katalog-api", "postgres", "example-worker", "leftover"}
	at := -1
	for _, name := range order {
		i := strings.Index(out, "\n"+name)
		if i <= at {
			t.Fatalf("%s is out of order (platform, addon, unclaimed):\n%s", name, out)
		}
		at = i
	}
	if p.called("GET "+operatorPath) != 1 {
		t.Fatalf("status must read the console once: %v", p.calls)
	}
}

// Both halves of "there is nothing to drive" — a portal that manages no
// workloads, and one that does but has no operator resource — exit 3 and say
// which, in the portal's own words.
func TestStatusWithoutAnOperatorConsole(t *testing.T) {
	for name, setup := range map[string]struct {
		prepare func(p *fakePortal)
		want    string
	}{
		"not in a cluster": {func(p *fakePortal) { p.available = false }, noManagement},
		"no operator": {func(p *fakePortal) {
			p.op = Operator{Present: false, Note: "no operator detected — managing deployments directly"}
		}, "no operator detected"},
	} {
		p, srv := newPortal(t)
		setup.prepare(p)
		code, out, errs := run(t, "", false, "status", "--url", srv.URL)
		if code != exitcode.NotOffered {
			t.Errorf("%s: want 3, got %d\n%s\n%s", name, code, out, errs)
		}
		if !strings.Contains(errs, "not offered:") || !strings.Contains(errs, setup.want) {
			t.Errorf("%s: the note must be printed, got %q", name, errs)
		}
		if strings.Contains(out, "NAME") {
			t.Errorf("%s: no table without a console:\n%s", name, out)
		}

		// --json still prints the portal's document, and still exits 3.
		code, out, _ = run(t, "", false, "status", "--url", srv.URL, "--json")
		if code != exitcode.NotOffered {
			t.Errorf("%s --json: want 3, got %d", name, code)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Errorf("%s --json: not JSON: %v (%q)", name, err, out)
		}
	}
}

// --json is the portal's own document: a script reads the API's shape, not
// zae's rendering of it.
func TestStatusJSONIsThePortalsOwnDocument(t *testing.T) {
	_, srv := newPortal(t)
	code, out, errs := run(t, "", false, "status", "--url", srv.URL, "--json")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d %s", code, errs)
	}
	var got Console
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json is not the console document: %v\n%s", err, out)
	}
	if !got.Available || !got.Operator.Present || got.Operator.AvailableUpdate != "1.5.0" {
		t.Errorf("--json lost fields: %+v", got.Operator)
	}
	if len(got.Instances) != 5 || got.Instances[0].Name != "chino-api" || !got.Instances[0].OperatorManaged {
		t.Errorf("--json lost the workloads: %+v", got.Instances)
	}
	if !got.Instances[4].Protected {
		t.Errorf("--json lost `protected`: %+v", got.Instances[4])
	}
	// Nothing of the rendering leaks into the machine-readable form.
	for _, s := range []string{"NAME", "Not covered here", "— pinned"} {
		if strings.Contains(out, s) {
			t.Errorf("--json carries rendered text %q:\n%s", s, out)
		}
	}
}

func TestUpdateVersionChannelAndMode(t *testing.T) {
	p, srv := newPortal(t)
	code, out, errs := run(t, "", false, "update", "--url", srv.URL,
		"--version", "1.6.0", "--channel", "edge", "--mode", "auto", "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	want := map[string]any{"version": "1.6.0", "channel": "edge", "updateMode": "auto"}
	if got := p.body(t, "PATCH "+operatorPath, 0); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("PATCH body:\n got %v\nwant %v", got, want)
	}
	if p.called("POST "+applyPath) != 0 {
		t.Fatalf("no --apply, so apply-update must not be called: %v", p.calls)
	}
	for _, s := range []string{"version      1.4.0 → 1.6.0", "channel      stable → edge", "update mode  manual → auto"} {
		if !strings.Contains(out, s) {
			t.Errorf("the change summary lacks %q:\n%s", s, out)
		}
	}
}

// --version "" takes the pin off; not passing --version at all leaves it
// alone. The difference has to survive to the request body.
func TestUpdateCanTakeThePinOff(t *testing.T) {
	p, srv := newPortal(t)
	code, out, errs := run(t, "", false, "update", "--url", srv.URL, "--version", "", "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	body := p.body(t, "PATCH "+operatorPath, 0)
	if v, ok := body["version"]; !ok || v != "" {
		t.Fatalf("--version \"\" must be sent as an empty version: %v", body)
	}
	if len(body) != 1 {
		t.Fatalf("only what was asked for is sent: %v", body)
	}
	if !strings.Contains(out, "1.4.0 → latest") {
		t.Errorf("taking the pin off is `latest`:\n%s", out)
	}
}

func TestUpdateApply(t *testing.T) {
	p, srv := newPortal(t)
	code, out, errs := run(t, "", false, "update", "--url", srv.URL, "--apply", "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if p.called("POST "+applyPath) != 1 || p.called("PATCH "+operatorPath) != 0 {
		t.Fatalf("--apply alone posts apply-update and patches nothing: %v", p.calls)
	}
	if !strings.Contains(out, "version      1.4.0 → 1.5.0") {
		t.Errorf("--apply must name the version it pins:\n%s", out)
	}
	if !strings.Contains(out, "rolls") {
		t.Errorf("a version change rolls the workloads the operator manages:\n%s", out)
	}

	// Nothing discovered: say so before writing, not after a 400.
	p2, srv2 := newPortal(t)
	p2.op.AvailableUpdate = ""
	code, _, errs = run(t, "", false, "update", "--url", srv2.URL, "--apply", "--yes")
	if code != exitcode.Failed || !strings.Contains(errs, "no update to apply") {
		t.Fatalf("no update: want 1, got %d %q", code, errs)
	}
	if p2.called("POST "+applyPath) != 0 {
		t.Fatalf("nothing to apply must not be posted: %v", p2.calls)
	}
}

// --mode auto with --apply is not a contradiction: what the operator does by
// itself from now on, and what it is pinned to now, are different questions.
func TestUpdateApplyWithModeIsAllowed(t *testing.T) {
	p, srv := newPortal(t)
	code, out, errs := run(t, "", false, "update", "--url", srv.URL, "--apply", "--mode", "auto", "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if body := p.body(t, "PATCH "+operatorPath, 0); len(body) != 1 || body["updateMode"] != "auto" {
		t.Fatalf("the patch carries only the mode: %v", body)
	}
	if p.called("POST "+applyPath) != 1 {
		t.Fatalf("the update is still applied: %v", p.calls)
	}
}

func TestUpdateRefusesContradictions(t *testing.T) {
	p, srv := newPortal(t)
	for name, args := range map[string][]string{
		"nothing asked":     {"update", "--url", srv.URL, "--yes"},
		"apply and version": {"update", "--url", srv.URL, "--apply", "--version", "1.6.0", "--yes"},
		"apply and channel": {"update", "--url", srv.URL, "--apply", "--channel", "edge", "--yes"},
		"unknown mode":      {"update", "--url", srv.URL, "--mode", "sometimes", "--yes"},
		"empty channel":     {"update", "--url", srv.URL, "--channel", "  ", "--yes"},
		"no url":            {"update", "--version", "1.6.0", "--yes"},
		"a positional":      {"update", "everything", "--url", srv.URL, "--version", "1.6.0", "--yes"},
		"zero timeout":      {"update", "--url", srv.URL, "--apply", "--yes", "--wait", "--timeout", "0"},
	} {
		code, _, errs := run(t, "", false, args...)
		if code != exitcode.Usage {
			t.Errorf("%s: want 2, got %d %q", name, code, errs)
		}
	}
	if len(p.calls) != 0 {
		t.Fatalf("a contradiction must not reach the portal: %v", p.calls)
	}
}

// zae asks only a person. Without a terminal on stdin the invocation needs
// --yes, and fails as usage before anything is written.
func TestUpdateWillNotAskWithoutATerminal(t *testing.T) {
	p, srv := newPortal(t)
	code, _, errs := run(t, "y\n", false, "update", "--url", srv.URL, "--apply")
	if code != exitcode.Usage || !strings.Contains(errs, "stdin is not a terminal") {
		t.Fatalf("want 2 saying so, got %d %q", code, errs)
	}
	if len(p.calls) != 0 {
		t.Fatalf("nothing may be read or written first: %v", p.calls)
	}

	// With a terminal, the question is asked — and a no changes nothing.
	code, out, _ := run(t, "n\n", true, "update", "--url", srv.URL, "--apply")
	if code != exitcode.Failed || !strings.Contains(out, "nothing changed") {
		t.Fatalf("declined: want 1, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "apply this to "+srv.URL+"? [y/N]") {
		t.Errorf("the question must name the instance:\n%s", out)
	}
	if p.called("POST "+applyPath) != 0 {
		t.Fatalf("a declined update must not be applied: %v", p.calls)
	}

	// And a yes goes through, after the summary.
	code, out, _ = run(t, "y\n", true, "update", "--url", srv.URL, "--apply")
	if code != exitcode.OK || p.called("POST "+applyPath) != 1 {
		t.Fatalf("confirmed: want 0 and one apply, got %d %v\n%s", code, p.calls, out)
	}
	if strings.Index(out, "version      1.4.0 → 1.5.0") > strings.Index(out, "apply this to") {
		t.Errorf("what changes must be printed before the question:\n%s", out)
	}
}

func TestUpdateWaitsUntilTheVersionIsRunningAndEverythingIsReady(t *testing.T) {
	p, srv := newPortal(t)
	// The operator needs three reads to report the new version, and the
	// rollout another two after that.
	after := 0
	p.onWrite = func(p *fakePortal) { after = p.reads }
	p.onGet = func(p *fakePortal, n int) {
		if after == 0 {
			return
		}
		switch {
		case n >= after+5:
			p.op.Phase, p.op.CurrentVersion, p.op.AvailableUpdate = "Ready", "1.5.0", ""
			p.op.ObservedGeneration = p.op.Generation
			p.ready()
		case n >= after+3:
			// The version is reported, and every workload reads ready — but the
			// operator has not reconciled this write yet. Before generations,
			// this is where a wait declared victory.
			p.op.Phase, p.op.CurrentVersion = "Reconciling", "1.5.0"
			p.readyCounters()
		default:
			p.op.Phase = "Reconciling"
		}
	}
	code, out, errs := run(t, "", false, "update", "--url", srv.URL, "--apply", "--yes", "--wait", "--timeout", "10s")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	for _, s := range []string{
		"waiting for the platform to report 1.5.0",
		"the platform reports 1.5.0, and every workload the operator manages is ready",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("the wait lacks %q:\n%s", s, out)
		}
	}
	if !strings.Contains(out, "katalog-api 0/1 degraded: ImagePullBackOff") {
		t.Errorf("progress must name what is not ready yet:\n%s", out)
	}
	// Sparingly: a line per change of state, not per poll.
	if lines := strings.Count(out, "\n  "); lines > 6 {
		t.Errorf("%d progress lines is not sparing:\n%s", lines, out)
	}
}

func TestUpdateWaitTimesOut(t *testing.T) {
	p, srv := newPortal(t)
	p.onWrite = func(p *fakePortal) { p.op.Phase, p.op.CurrentVersion = "Reconciling", "1.4.0" }
	code, out, errs := run(t, "", false, "update", "--url", srv.URL, "--apply", "--yes", "--wait", "--timeout", "40ms")
	if code != exitcode.Failed {
		t.Fatalf("want 1, got %d\n%s\n%s", code, out, errs)
	}
	for _, s := range []string{"still waiting for", "1.5.0", "katalog-api 0/1 degraded: ImagePullBackOff"} {
		if !strings.Contains(errs, s) {
			t.Errorf("the timeout must say what was not ready, lacks %q: %q", s, errs)
		}
	}
}

func TestRestartAndScale(t *testing.T) {
	p, srv := newPortal(t)
	code, out, errs := run(t, "", true, "restart", "chino-api", "--url", srv.URL, "--yes")
	if code != exitcode.OK {
		t.Fatalf("restart: want 0, got %d\n%s\n%s", code, out, errs)
	}
	if p.called("POST "+operatorPath+"/instances/chino-api/restart") != 1 {
		t.Fatalf("restart was not posted once: %v", p.calls)
	}
	if !strings.Contains(out, "chino-api — platform · 1.4.0 · 2/2 ready") {
		t.Errorf("the workload must be described first:\n%s", out)
	}

	// scale sends exactly the body the portal decodes — it refuses any other.
	code, out, errs = run(t, "", true, "scale", "chino-api", "3", "--url", srv.URL, "--yes")
	if code != exitcode.OK {
		t.Fatalf("scale: want 0, got %d\n%s\n%s", code, out, errs)
	}
	if body := p.body(t, "POST "+operatorPath+"/instances/chino-api/scale", 0); len(body) != 1 || body["replicas"] != float64(3) {
		t.Fatalf("scale body: %v", body)
	}

	// The question names both counts, and a no scales nothing.
	p2, srv2 := newPortal(t)
	code, out, _ = run(t, "n\n", true, "scale", "chino-api", "5", "--url", srv2.URL)
	if code != exitcode.Failed || !strings.Contains(out, "scale chino-api from 2 to 5 on "+srv2.URL+"? [y/N]") {
		t.Fatalf("declined scale: want 1 with the question, got %d\n%s", code, out)
	}
	if p2.called("POST "+operatorPath+"/instances/chino-api/scale") != 0 {
		t.Fatalf("a declined scale must not be posted: %v", p2.calls)
	}
}

// The platform owns the rule and its wording. zae sends the call and reports
// the refusal it gets, rather than inventing a policy of its own.
func TestProtectedWorkloadIsRefusedByThePlatform(t *testing.T) {
	for action, refusal := range map[string]string{
		"restart": `"postgres" is a protected (stateful) service and cannot be restarted from here`,
		"scale":   `"postgres" is a protected (stateful) service and cannot be scaled from here`,
	} {
		_, srv := newPortal(t)
		args := []string{action, "postgres", "--url", srv.URL, "--yes"}
		if action == "scale" {
			args = []string{action, "postgres", "2", "--url", srv.URL, "--yes"}
		}
		code, out, errs := run(t, "", true, args...)
		if code != exitcode.Failed {
			t.Errorf("%s protected: want 1, got %d %q", action, code, errs)
		}
		if !strings.Contains(errs, refusal) {
			t.Errorf("%s protected: the platform's own reason must be printed, got %q", action, errs)
		}
		if !strings.Contains(out, "protected by the platform") {
			t.Errorf("%s protected: the workload line must say so:\n%s", action, out)
		}
		// It is refused, so there is nothing to confirm — and asking anyway,
		// on a terminal, without --yes, still ends in the platform's reason.
		code, out, errs = run(t, "", true, args[:len(args)-1]...)
		if code != exitcode.Failed || strings.Contains(out, "[y/N]") {
			t.Errorf("%s protected: no question for a call that changes nothing: %d\n%s\n%s", action, code, out, errs)
		}
	}
}

func TestUnknownWorkloadIsNotOffered(t *testing.T) {
	p, srv := newPortal(t)
	for _, args := range [][]string{
		{"restart", "nosuch", "--url", srv.URL, "--yes"},
		{"scale", "nosuch", "2", "--url", srv.URL, "--yes"},
	} {
		code, _, errs := run(t, "", false, args...)
		if code != exitcode.NotOffered {
			t.Errorf("%v: want 3, got %d %q", args, code, errs)
		}
		if !strings.Contains(errs, `runs no workload named "nosuch"`) {
			t.Errorf("%v: %q", args, errs)
		}
	}
	if n := p.called("POST " + operatorPath + "/instances/nosuch/restart"); n != 0 {
		t.Fatalf("an unknown workload must not be written to: %v", p.calls)
	}
}

func TestScaleWaitsForTheReplicaCount(t *testing.T) {
	p, srv := newPortal(t)
	p.onGet = func(p *fakePortal, n int) {
		if p.called("POST "+operatorPath+"/instances/chino-api/scale") > 0 && n > 3 {
			p.ready()
		}
	}
	code, out, errs := run(t, "", false, "scale", "chino-api", "4", "--url", srv.URL, "--yes", "--wait", "--timeout", "10s")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "chino-api runs 4") {
		t.Errorf("the wait must end by saying so:\n%s", out)
	}

	// And a restart waits for the workload to come back.
	p2, srv2 := newPortal(t)
	restarted := 0
	p2.onWrite = func(p *fakePortal) { restarted = p.reads }
	p2.onGet = func(p *fakePortal, n int) {
		if restarted > 0 && n > restarted+2 {
			p.ready()
		}
	}
	code, out, errs = run(t, "", false, "restart", "chino-api", "--url", srv2.URL, "--yes", "--wait", "--timeout", "10s")
	if code != exitcode.OK || !strings.Contains(out, "chino-api is ready") {
		t.Fatalf("restart --wait: want 0, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "the cluster has not acted on this change yet") {
		t.Errorf("the wait must show the rollout it waited through:\n%s", out)
	}
}

// The live failure, in a test: a restart is asked for, the cluster has not
// acted on it yet, and every replica counter says ready — of the pod from
// before the restart. The wait must not believe them.
func TestRestartWaitsForTheNewRolloutAndNotForTheOldPods(t *testing.T) {
	p, srv := newPortal(t)
	restarted, done := 0, 0
	p.onWrite = func(p *fakePortal) {
		// Exactly what the cluster does: the spec moved, the counters did not.
		restarted = p.reads
	}
	p.onGet = func(p *fakePortal, n int) {
		if restarted > 0 && n >= restarted+3 {
			done = n
			p.ready()
		}
	}
	code, out, errs := run(t, "", false, "restart", "chino-api", "--url", srv.URL, "--yes", "--wait", "--timeout", "10s")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "the cluster has not acted on this change yet") {
		t.Fatalf("the wait must refuse the pods from before the restart:\n%s", out)
	}
	if done == 0 || p.reads < done {
		t.Fatalf("the wait ended before the rollout was observed (reads=%d, rollout at %d)", p.reads, done)
	}
	// And it was not the old stamp it settled on.
	if wl := p.workload("chino-api"); wl.RestartedAt == "2026-09-20T08:00:00Z" {
		t.Errorf("the restart stamp must have moved: %q", wl.RestartedAt)
	}
	// Nothing about a readiness-gate-only wait is said, because this one is not.
	if strings.Contains(out, "readiness gate") {
		t.Errorf("an exact wait must not warn about a weaker one:\n%s", out)
	}
}

// The live sequence, exactly as it was measured on the demo: one replica, so
// the rollout surges — the new pod is created first, and while it starts up
// the cluster reports updated 1, ready 1, available 1 and observedGeneration
// caught up. Every one of those is the right number about the wrong pod. Only
// the total says so: 2 during the surge, 1 when the old pod is gone.
func TestRestartDoesNotFinishWhileTheOldPodIsStillThere(t *testing.T) {
	p, srv := newPortal(t)
	surging, finished := 0, 0
	p.onWrite = func(p *fakePortal) { surging = p.reads }
	p.onGet = func(p *fakePortal, n int) {
		switch {
		case surging == 0:
		case n >= surging+5:
			// The old pod is gone: one pod, from the new revision, available.
			finished = n
			p.ready()
		default:
			p.surge("chino-api")
		}
	}
	code, out, errs := run(t, "", false, "restart", "chino-api", "--url", srv.URL, "--yes", "--wait", "--timeout", "10s")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if finished == 0 || p.reads < finished {
		t.Fatalf("the wait ended during the surge (reads=%d, old pod gone at %d)\n%s", p.reads, finished, out)
	}
	if !strings.Contains(out, "the older ones are still there") {
		t.Fatalf("the wait must say what it is waiting for:\n%s", out)
	}
	// And it must have taken more than one look: a gate that passed on the
	// first reading is the bug, whatever it printed afterwards.
	if lines := strings.Count(out, "\n  "); lines < 2 {
		t.Errorf("the wait went through one state only:\n%s", out)
	}
}

// Ready and updated at the size asked for, with an old pod still running, is
// the exact state that used to pass. It must not, and it must be told apart
// from the states that legitimately do.
func TestSettledRules(t *testing.T) {
	base := func() Workload {
		return Workload{Name: "w", DesiredReplicas: 1, Replicas: pods(1), UpdatedReplicas: 1,
			ReadyReplicas: 1, AvailableReplicas: 1, Phase: PhaseReady, Generation: 5, ObservedGeneration: 5}
	}
	with := func(f func(*Workload)) Workload { w := base(); f(&w); return w }
	for name, tc := range map[string]struct {
		w    Workload
		want bool
	}{
		"rolled out":              {base(), true},
		"mid-surge, old pod left": {with(func(w *Workload) { w.Replicas = pods(2) }), false},
		"new pod not created yet": {with(func(w *Workload) { w.UpdatedReplicas, w.Replicas = 0, pods(1) }), false},
		"new pod not available":   {with(func(w *Workload) { w.AvailableReplicas = 0 }), false},
		"spec not acted on":       {with(func(w *Workload) { w.ObservedGeneration = 4 }), false},
		"stopped at zero":         {Workload{Name: "w", Replicas: pods(0), Phase: PhaseStopped, Generation: 2, ObservedGeneration: 2}, true},
		// Without the total, the same surge cannot be seen — which is why zae
		// says so rather than calling that wait exact.
		"surge is invisible without the total": {with(func(w *Workload) { w.Replicas = nil }), true},
		"degraded is never settled":            {with(func(w *Workload) { w.Phase = PhaseDegraded }), false},
	} {
		if got := tc.w.settled(); got != tc.want {
			t.Errorf("%s: settled() = %v, want %v (%s)", name, got, tc.want, tc.w.pending())
		}
	}
}

// A portal that reports generations but not the total pod count — the one zae
// shipped first — still works, and says which half it is missing.
func TestWaitNamesTheTotalItCannotSee(t *testing.T) {
	p, srv := newPortal(t)
	p.noTotal = true
	restarted := 0
	p.onWrite = func(p *fakePortal) { restarted = p.reads }
	p.onGet = func(p *fakePortal, n int) {
		if restarted > 0 && n > restarted+1 {
			p.ready()
		}
	}
	code, out, errs := run(t, "", false, "restart", "chino-api", "--url", srv.URL, "--yes", "--wait", "--timeout", "10s")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "no total replica count") || !strings.Contains(out, "readiness gate") {
		t.Fatalf("the note must name the field it cannot see:\n%s", out)
	}
	if strings.Contains(out, "no rollout generation") {
		t.Errorf("this portal does report generations:\n%s", out)
	}
}

// Against a portal-api that predates the rollout fields, zae still works —
// and says, once, that its wait is the weaker one. Silently falling back is
// the thing that made the original bug invisible.
func TestWaitFallsBackAndSaysSoOnAnOlderPortal(t *testing.T) {
	p, srv := newPortal(t)
	p.legacy = true

	code, out, errs := run(t, "", false, "restart", "chino-api", "--url", srv.URL, "--yes", "--wait", "--timeout", "10s")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "readiness gate") || !strings.Contains(out, "predates the exact wait") {
		t.Fatalf("zae must say the wait is weaker here:\n%s", out)
	}
	if n := strings.Count(out, "readiness gate"); n != 1 {
		t.Errorf("said %d times, want once:\n%s", n, out)
	}
	// The write still happened, and an update still works the same way.
	if p.called("POST "+operatorPath+"/instances/chino-api/restart") != 1 {
		t.Fatalf("the restart must still be sent: %v", p.calls)
	}
	p2, srv2 := newPortal(t)
	p2.legacy = true
	p2.onGet = func(p *fakePortal, n int) {
		if p.called("POST "+applyPath) > 0 {
			p.op.CurrentVersion, p.op.Phase = "1.5.0", "Ready"
			p.ready()
		}
	}
	code, out, errs = run(t, "", false, "update", "--url", srv2.URL, "--apply", "--yes", "--wait", "--timeout", "10s")
	if code != exitcode.OK || !strings.Contains(out, "readiness gate") {
		t.Fatalf("update against an older portal: want 0 with the note, got %d\n%s\n%s", code, out, errs)
	}
}

// --apply names the update it decided on, so the platform can refuse one that
// moved underneath rather than rolling to a version nobody chose.
func TestApplySendsTheVersionItDecidedOn(t *testing.T) {
	p, srv := newPortal(t)
	code, out, errs := run(t, "", false, "update", "--url", srv.URL, "--apply", "--yes")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	body := p.body(t, "POST "+applyPath, 0)
	if body["version"] != "1.5.0" {
		t.Fatalf("--apply must name the update it read: %v", body)
	}

	// It moved between the read zae based its question on and the write that
	// answers it: the platform refuses, and zae reports that refusal with what
	// to do about it.
	p2, srv2 := newPortal(t)
	p2.beforeApply = func(p *fakePortal) { p.op.AvailableUpdate = "2.0.0-rc1" }
	code, out, errs = run(t, "", false, "update", "--url", srv2.URL, "--apply", "--yes")
	if code != exitcode.Failed {
		t.Fatalf("a moved update: want 1, got %d\n%s\n%s", code, out, errs)
	}
	for _, s := range []string{"2.0.0-rc1", "look again with: zae platform status"} {
		if !strings.Contains(errs, s) {
			t.Errorf("the refusal must carry %q: %q", s, errs)
		}
	}
	if p2.op.Version != "1.4.0" {
		t.Errorf("a refused apply must change nothing: %q", p2.op.Version)
	}
}

func TestScaleWaitTimesOut(t *testing.T) {
	_, srv := newPortal(t)
	code, out, errs := run(t, "", false, "scale", "chino-api", "4", "--url", srv.URL, "--yes", "--wait", "--timeout", "40ms")
	if code != exitcode.Failed {
		t.Fatalf("want 1, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(errs, "still waiting for chino-api to run 4") {
		t.Errorf("the timeout must name what it waited for: %q", errs)
	}
}

func TestForbiddenUnreachableAndOldPortals(t *testing.T) {
	p, srv := newPortal(t)
	p.token = "the-admin-bearer"
	t.Setenv(instance.TokenEnv, "")
	code, _, errs := run(t, "", false, "status", "--url", srv.URL)
	if code != exitcode.Forbidden || !strings.Contains(errs, "supply a bearer via "+instance.TokenEnv) {
		t.Fatalf("no bearer: want 5 naming %s, got %d %q", instance.TokenEnv, code, errs)
	}
	// A write is refused the same way, and never reported as "not offered".
	code, _, errs = run(t, "", false, "restart", "chino-api", "--url", srv.URL, "--yes")
	if code != exitcode.Forbidden {
		t.Fatalf("refused write: want 5, got %d %q", code, errs)
	}

	code, _, errs = run(t, "", false, "status", "--url", "http://127.0.0.1:1")
	if code != exitcode.Undetermined || !strings.Contains(errs, "not concluding") {
		t.Fatalf("unreachable: want 4, got %d %q", code, errs)
	}

	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	code, _, errs = run(t, "", false, "status", "--url", old.URL)
	if code != exitcode.NotOffered || !strings.Contains(errs, "does not serve "+operatorPath) {
		t.Fatalf("a portal without the console: want 3, got %d %q", code, errs)
	}
}

// The bearer comes from the session `zae login` stored for this instance —
// not from the developer's own credentials file, which these tests never read.
func TestTheBearerComesFromTheStoredSession(t *testing.T) {
	p, srv := newPortal(t)
	p.token = "stored-session-bearer"
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(instance.TokenEnv, "")
	if err := creds.Put(srv.URL, creds.Entry{Issuer: "https://issuer.example.org", ClientID: "zae",
		AccessToken: "stored-session-bearer", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "", false, "status", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if len(p.auth) == 0 || p.auth[0] != "Bearer stored-session-bearer" {
		t.Fatalf("the stored session must be sent: %q", p.auth)
	}
	// And the token is never printed.
	if strings.Contains(out+errs, "stored-session-bearer") {
		t.Fatalf("the bearer was printed:\n%s\n%s", out, errs)
	}
}

func TestUsageErrors(t *testing.T) {
	p, srv := newPortal(t)
	for name, args := range map[string][]string{
		"no command":          {},
		"unknown command":     {"upgrade"},
		"status with a name":  {"status", "chino-api", "--url", srv.URL},
		"restart without one": {"restart", "--url", srv.URL, "--yes"},
		"restart two":         {"restart", "a", "b", "--url", srv.URL, "--yes"},
		"scale without count": {"scale", "chino-api", "--url", srv.URL, "--yes"},
		"scale not a number":  {"scale", "chino-api", "many", "--url", srv.URL, "--yes"},
		"scale no url":        {"scale", "chino-api", "2", "--yes"},
		"not an address":      {"status", "--url", "media.example.org"},
	} {
		code, _, errs := run(t, "", false, args...)
		if code != exitcode.Usage {
			t.Errorf("%s: want 2, got %d %q", name, code, errs)
		}
	}
	if len(p.calls) != 0 {
		t.Fatalf("usage errors must not reach the portal: %v", p.calls)
	}
}

// Flags and positionals in any order, and `help` on stdout.
func TestArgumentOrderAndHelp(t *testing.T) {
	p, srv := newPortal(t)
	code, _, errs := run(t, "", true, "scale", "--url", srv.URL, "chino-api", "--yes", "3")
	if code != exitcode.OK {
		t.Fatalf("flags before positionals: want 0, got %d %q", code, errs)
	}
	if body := p.body(t, "POST "+operatorPath+"/instances/chino-api/scale", 0); body["replicas"] != float64(3) {
		t.Fatalf("scale body: %v", body)
	}
	code, out, _ := run(t, "", false, "help")
	if code != exitcode.OK || !strings.Contains(out, "zae platform status") {
		t.Fatalf("help: want 0 on stdout, got %d\n%s", code, out)
	}
}

func TestImageTag(t *testing.T) {
	for image, want := range map[string]string{
		"ghcr.io/example/api:1.4.0":                             "1.4.0",
		"ghcr.io/example/api":                                   "latest",
		"ghcr.io/example/api@sha256:" + strings.Repeat("b", 64): "sha256:bbbbbbbbbbbb",
		"registry.example.org:5000/example/api":                 "latest",
		"registry.example.org:5000/example/api:2.0":             "2.0",
		"": "-",
	} {
		if got := imageTag(image); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", image, got, want)
		}
	}
}

// A portal that reached the operator but not the workloads still renders, and
// says which half is missing rather than showing an empty platform.
func TestWorkloadListMissing(t *testing.T) {
	p, srv := newPortal(t)
	p.listError = "cannot list deployments: etcdserver: request timed out"
	code, out, errs := run(t, "", false, "status", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d %q", code, errs)
	}
	if !strings.Contains(out, "could not list them: cannot list deployments") {
		t.Errorf("the missing list must be named:\n%s", out)
	}
}
