package debug

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// A credential the portal should have redacted and, playing an older
// portal-api, did not. Distinctive on purpose: a substring search for it is
// the whole test.
const leaked = "sk-9f8a7b6c5d4e3f2a1b0c9d8e7f6a"

// fakePortal is the portal's debug API in miniature: the pods of a namespace,
// each container's log with the timestamps the cluster puts on every line, the
// event tap and the support bundle.
type fakePortal struct {
	mu    sync.Mutex
	calls []string
	logQ  []url.Values // the query of every log read, in order

	token  string // required bearer, "" for none
	pods   []Pod
	logs   map[string][]string // pod/container → lines, timestamp first
	broken map[string]string   // pod/container → a 500 the log read answers
	// logs503 answers every log read with 503 and this body.
	logs503 string

	topology, events, bundle string

	// onList runs on every pods listing: a follow's rounds.
	lists  int
	onList func(p *fakePortal, n int)
}

func newPortal(t *testing.T) (*fakePortal, *httptest.Server) {
	p := &fakePortal{logs: map[string][]string{}, broken: map[string]string{},
		topology: `{"available":true,"brokers":["kafka:9092"],"topics":[{"topic":"platform.item.added","partitions":1},{"topic":"platform.item.removed","partitions":1}],"groups":["workers"]}`,
		events:   `[]`}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+podsPath, func(w http.ResponseWriter, r *http.Request) {
		p.lists++
		if p.onList != nil {
			p.onList(p, p.lists)
		}
		writeJSON(w, p.pods)
	})
	mux.HandleFunc("GET "+logsPath, p.log)
	mux.HandleFunc("GET "+topologyPath, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, p.topology) })
	mux.HandleFunc("GET "+eventsPath, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, p.events) })
	mux.HandleFunc("GET "+bundlePath, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, p.bundle) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls = append(p.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		if p.token != "" && r.Header.Get("Authorization") != "Bearer "+p.token {
			http.Error(w, "forbidden: requires the zaentrum-admin role", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return p, srv
}

// log answers one container's log as the portal does: text, a line per
// line, the last tail of them (500 when none is asked for).
func (p *fakePortal) log(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p.logQ = append(p.logQ, q)
	if p.logs503 != "" {
		http.Error(w, p.logs503, http.StatusServiceUnavailable)
		return
	}
	key := q.Get("pod") + "/" + q.Get("container")
	if msg, ok := p.broken[key]; ok {
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	lines, ok := p.logs[key]
	if !ok {
		http.Error(w, fmt.Sprintf("k8s 404 NotFound: pods %q not found", q.Get("pod")), http.StatusInternalServerError)
		return
	}
	tail := 500
	if n, err := strconv.Atoi(q.Get("tail")); err == nil && n > 0 {
		tail = n
	}
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func (p *fakePortal) called(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// at is a line's timestamp, s seconds after a fixed moment.
func at(s int) string {
	return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC).Add(time.Duration(s) * time.Second).Format(time.RFC3339Nano)
}

// line is a log line as the cluster stamps it.
func line(s int, text string) string { return at(s) + " " + text }

// exampleNamespace is what the portal lists for a platform's namespace: a
// Deployment of two replicas, another whose name starts like it, a pod with
// two containers, a StatefulSet's pod and a Job's.
func exampleNamespace(p *fakePortal) {
	p.pods = []Pod{
		{Pod: "chino-api-7d79fd4c4b-xprff", Phase: "Running", Containers: []string{"app"}},
		{Pod: "chino-api-7d79fd4c4b-qfmtj", Phase: "Running", Containers: []string{"app"}},
		{Pod: "chino-api-worker-5c5b77b969-rhbqb", Phase: "Running", Containers: []string{"app"}},
		{Pod: "chino-web-58f8548947-ljdwb", Phase: "Running", Containers: []string{"app"}},
		{Pod: "portal-api-554bd55786-krtkx", Phase: "Running", Containers: []string{"app", "proxy"}},
		{Pod: "postgres-0", Phase: "Running", Containers: []string{"postgres"}},
		{Pod: "zaentrum-verify-fzzgp-gsg9z", Phase: "Succeeded", Containers: []string{"doctor"}},
	}
	p.logs = map[string][]string{
		"chino-api-7d79fd4c4b-xprff/app":        {line(1, "first replica starts"), line(4, "first replica serves")},
		"chino-api-7d79fd4c4b-qfmtj/app":        {line(2, "second replica starts"), line(3, "second replica serves")},
		"chino-api-worker-5c5b77b969-rhbqb/app": {line(1, "the worker, not the api")},
		"chino-web-58f8548947-ljdwb/app":        {line(1, "web")},
		"portal-api-554bd55786-krtkx/app":       {line(1, "portal app")},
		"portal-api-554bd55786-krtkx/proxy":     {line(2, "portal proxy")},
		"postgres-0/postgres":                   {line(1, "database system is ready")},
		"zaentrum-verify-fzzgp-gsg9z/doctor":    {line(1, "doctor: no failures")},
	}
}

// Credentials are ambient: a stored session, or a ZAE_TOKEN in the shell
// running `go test`, would change what these tests see.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zae-debug-test-config-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Unsetenv(instance.TokenEnv)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var (
	sigMu      sync.Mutex
	sigChannel chan<- os.Signal
)

// interrupt is Ctrl-C, delivered to the running command.
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

func run(t *testing.T, args ...string) (code int, out, errs string) {
	t.Helper()
	var ob, eb bytes.Buffer
	stdout, stderr = &ob, &eb
	pollInterval = time.Millisecond
	notifySignals = func(c chan<- os.Signal) func() {
		sigMu.Lock()
		sigChannel = c
		sigMu.Unlock()
		return func() {}
	}
	defer func() {
		stdout, stderr, pollInterval = os.Stdout, os.Stderr, 2*time.Second
		sigMu.Lock()
		sigChannel = nil
		sigMu.Unlock()
	}()
	code = Run(args, "v9.9.9-test")
	return code, ob.String(), eb.String()
}

// The contract every read keeps: an admin's bearer, the portal's words for a
// refusal, and exit codes a script can branch on.
func TestLogsRefusalsAndOldPortals(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	p.token = "admin-bearer"
	code, _, errs := run(t, "logs", "postgres", "--url", srv.URL)
	if code != exitcode.Forbidden || !strings.Contains(errs, "supply a bearer via "+instance.TokenEnv) || !strings.Contains(errs, adminNeed) {
		t.Errorf("without a bearer: want 5 with the hint, got %d %q", code, errs)
	}
	t.Setenv(instance.TokenEnv, "admin-bearer")
	if code, _, errs := run(t, "logs", "postgres", "--url", srv.URL); code != exitcode.OK {
		t.Fatalf("with the admin's bearer: want 0, got %d %q", code, errs)
	}
	t.Setenv(instance.TokenEnv, "")

	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	if code, _, errs := run(t, "logs", "postgres", "--url", old.URL); code != exitcode.NotOffered || !strings.Contains(errs, "does not serve "+podsPath) {
		t.Errorf("a portal-api without the debug API: want 3, got %d %q", code, errs)
	}
	if code, _, errs := run(t, "logs", "postgres", "--url", "http://127.0.0.1:1"); code != exitcode.Undetermined || !strings.Contains(errs, "not concluding") {
		t.Errorf("unreachable: want 4, got %d %q", code, errs)
	}
}

// usageCases runs invocations that must each fail as usage, before anything
// reaches the instance.
func usageCases(t *testing.T, p *fakePortal, cases map[string][]string) {
	t.Helper()
	for name, args := range cases {
		if code, _, errs := run(t, args...); code != exitcode.Usage {
			t.Errorf("%s: want 2, got %d %q", name, code, errs)
		}
	}
	if len(p.calls) != 0 {
		t.Fatalf("usage errors reach no instance: %v", p.calls)
	}
}

func TestDebugUsage(t *testing.T) {
	p, _ := newPortal(t)
	usageCases(t, p, map[string][]string{
		"no command":      {},
		"unknown command": {"tail"},
	})
	if code, out, _ := run(t, "help"); code != exitcode.OK || !strings.Contains(out, "zae debug logs <workload|pod>") {
		t.Fatalf("help: want 0 on stdout, got %d\n%s", code, out)
	}
}
