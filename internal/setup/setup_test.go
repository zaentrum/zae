package setup

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
	"github.com/zaentrum/zae/internal/term"
)

// clock is the moment every test reads "3 min ago" against.
var clock = time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)

// fakePortal is the portal's setup API in miniature: the checklist as
// portal-api reads it, and — as each write lands — the writes beside it, with
// the rules portal-api keeps.
type fakePortal struct {
	mu     sync.Mutex
	calls  []string
	bodies map[string][]string
	auth   []string

	token string // required bearer, "" for none
	// old is a portal-api older than the checklist: its router answers 404
	// for every setup route.
	old bool
	// answer, when set, is sent for GET /setup instead of the document.
	answer string
	// catalog is how the catalog manager behind the portal takes a write:
	// "" it does, "none" there is none configured, "silent" it does not
	// answer, "refused" it refuses this admin, "failed" it answers an error.
	catalog string
	// echo is a portal that repeats the key it was sent — "refusal" in a
	// 400, "answer" in the step's state and note of its answer — which
	// portal-api never does, and zae must not rely on.
	echo string
	// key is the TMDB key the portal was sent, as it stores it.
	key string

	doc Doc
}

// catalogAnswers are what portal-api answers for a write the catalog manager
// does not take, word for word.
var catalogAnswers = map[string]struct {
	status int
	body   string
}{
	"none":    {http.StatusServiceUnavailable, "portal-api is pointed at no catalog manager (PORTAL_KATALOG_MANAGER_URL)"},
	"silent":  {http.StatusBadGateway, "the catalog manager did not answer: dial tcp 10.0.0.7:8080: i/o timeout"},
	"refused": {http.StatusBadGateway, "the catalog manager refused this admin: forbidden: settings requires the catalog-admin role"},
	"failed":  {http.StatusBadGateway, "the catalog manager answered: setting tmdb.api_key: database is read-only"},
}

func newPortal(t *testing.T, doc Doc) (*fakePortal, *httptest.Server) {
	p := &fakePortal{bodies: map[string][]string{}, doc: doc}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+setupPath, func(w http.ResponseWriter, r *http.Request) {
		if p.answer != "" {
			io.WriteString(w, p.answer)
			return
		}
		writeJSON(w, http.StatusOK, p.doc)
	})
	mux.HandleFunc("POST "+metadataPath, p.setKey)
	mux.HandleFunc("POST "+scanPath, p.scan)
	mux.HandleFunc("GET "+completePath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, completion{Completed: p.doc.Completed})
	})
	mux.HandleFunc("POST "+completePath, func(w http.ResponseWriter, r *http.Request) {
		// The first admin to mark it done is the one the record names.
		if p.doc.Completed == nil {
			p.doc.Completed = &Completion{At: clock, By: "admin"}
		}
		writeJSON(w, http.StatusOK, completion{Completed: p.doc.Completed})
	})
	mux.HandleFunc("DELETE "+completePath, func(w http.ResponseWriter, r *http.Request) {
		p.doc.Completed = nil
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		defer p.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		p.calls = append(p.calls, key)
		p.bodies[key] = append(p.bodies[key], string(body))
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		if p.token != "" && r.Header.Get("Authorization") != "Bearer "+p.token {
			http.Error(w, "forbidden: requires the zaentrum-admin role", http.StatusForbidden)
			return
		}
		if p.old && strings.HasPrefix(r.URL.Path, setupPath) {
			http.NotFound(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return p, srv
}

// setKey is POST /setup/metadata {"tmdbKey"}, with portal-api's rules: an
// unknown field refused, the key trimmed and refused when it is empty, too
// long or holds whitespace — and handed to the catalog manager, which may not
// take it.
func (p *fakePortal) setKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TMDBKey string `json:"tmdbKey"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(body.TMDBKey)
	switch {
	case p.echo == "refusal":
		http.Error(w, "tmdbKey "+key+" is not one this portal takes", http.StatusBadRequest)
		return
	case key == "":
		http.Error(w, "tmdbKey is empty — paste the API read access token from your TMDB account's API settings", http.StatusBadRequest)
		return
	case len(key) > 4096 || strings.ContainsAny(key, " \t\r\n"):
		http.Error(w, "tmdbKey is no TMDB token", http.StatusBadRequest)
		return
	}
	if a, ok := catalogAnswers[p.catalog]; ok {
		http.Error(w, a.body, a.status)
		return
	}
	p.key = key
	at := clock
	p.doc.Metadata = Metadata{Step: Step{State: stateDone}, Key: "setting", UpdatedAt: &at}
	if p.echo == "answer" {
		p.doc.Metadata = Metadata{Step: Step{State: "set to " + key, Note: "now " + key}, Key: key}
	}
	writeJSON(w, http.StatusOK, p.doc)
}

// scan is POST /setup/library/scan: a body, when one is sent, must be empty;
// the catalog manager starts the scan — or does not — and the answer is the
// checklist with the scan running in it, 202.
func (p *fakePortal) scan(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if a, ok := catalogAnswers[p.catalog]; ok {
		http.Error(w, a.body, a.status)
		return
	}
	at := clock
	p.doc.Library.State = stateWorking
	p.doc.Library.Scan = &ScanJob{ID: "scan-8", Status: "running", StartedAt: &at}
	writeJSON(w, http.StatusAccepted, p.doc)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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

// writes are the calls that were not reads.
func (p *fakePortal) writes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// freshBox is the checklist of a fresh appliance, as portal-api reads it: no
// key, nothing in the library, the pipeline off, plain http on a localhost
// name — and nodes portal-api's Role may not read.
func freshBox() Doc {
	return Doc{
		Metadata: Metadata{Step: Step{State: stateTodo}, Key: "none"},
		Library: Library{Step: Step{State: stateTodo}, Path: "/var/lib/katalog/media", Volume: "media", Folder: "media",
			Appliance: true},
		Processing: Processing{Step: Step{State: stateOptional}, Pipeline: ptr(false), GPU: ptr(false),
			GPUNote:    "portal-api's Role grants no reads of the cluster's nodes, so whether one offers a GPU cannot be told from here",
			Workers:    []Worker{},
			Switchable: true},
		Devices: Devices{Step: Step{State: stateTodo}, Origin: "http://zaentrum.localhost",
			Issuer: "http://zaentrum.localhost/auth/realms/zaentrum", LocalOnly: true, Source: "operator"},
		People: Step{State: stateInfo},
	}
}

// setUp is a platform with every step done but the record: the catalog
// manager's own key, a library scanned three minutes ago, the pipeline
// running, https — the live demo's shape.
func setUp() Doc {
	started, finished := clock.Add(-3*time.Minute-time.Second), clock.Add(-3*time.Minute)
	return Doc{
		Metadata: Metadata{Step: Step{State: stateDone}, Key: "environment"},
		Library: Library{Step: Step{State: stateDone}, Titles: 28, Path: "/var/lib/katalog/media", Volume: "media", Folder: "media",
			Scan: &ScanJob{ID: "scan-7", Status: "done", StartedAt: &started, FinishedAt: &finished, FilesSeen: 33, ItemsUpdated: 33}},
		Processing: Processing{Step: Step{State: stateDone}, Pipeline: ptr(true), GPU: ptr(true), GPUNodes: ptr(1),
			Workers: []Worker{
				{Name: "analyzer", Phase: "ready", Ready: 1, Desired: 1},
				{Name: "katalog-ingest", Phase: "ready", Ready: 1, Desired: 1},
				{Name: "packager", Phase: "ready", Ready: 2, Desired: 2},
				{Name: "transcoder", Phase: "ready", Ready: 1, Desired: 1},
			}, Switchable: true},
		Devices: Devices{Step: Step{State: stateDone}, Origin: "https://media.example.org", HTTPS: true,
			Issuer: "https://media.example.org/auth/realms/zaentrum", IssuerHTTPS: true, Source: "operator"},
		People: Step{State: stateInfo},
	}
}

// Credentials are ambient: a stored session, or a ZAE_TOKEN in the shell
// running `go test`, would change what these tests see. Point the credentials
// file at a directory that starts empty and cannot be the developer's own.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zae-setup-test-config-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Unsetenv(instance.TokenEnv)
	code := m.Run()
	os.RemoveAll(dir) // os.Exit skips defers
	os.Exit(code)
}

// run executes `zae setup …` against the fixed clock, with nothing on stdin
// and no terminal.
func run(t *testing.T, args ...string) (code int, out, errs string) {
	t.Helper()
	return runWith(t, "", false, args...)
}

// prompts are the prompts readHidden was asked to show, in order.
var prompts []string

// runWith executes `zae setup …` with input on stdin, which is a terminal or
// not. On a terminal, a hidden prompt reads its line from that input, as the
// question after it does.
func runWith(t *testing.T, input string, terminal bool, args ...string) (code int, out, errs string) {
	t.Helper()
	var ob, eb bytes.Buffer
	stdout, stderr, stdin = &ob, &eb, strings.NewReader(input)
	canAsk = func() bool { return terminal }
	now = func() time.Time { return clock }
	prompts = nil
	realHidden := readHidden
	readHidden = func(prompt string) (string, error) {
		prompts = append(prompts, prompt)
		fmt.Fprint(stderr, prompt)
		line, err := lineReader().ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	defer func() {
		stdout, stderr, stdin, now = os.Stdout, os.Stderr, os.Stdin, time.Now
		canAsk = func() bool { return term.Is(os.Stdin) }
		readHidden = realHidden
	}()
	return Run(args), ob.String(), eb.String()
}

func regexpLine(out, re string) bool { return regexp.MustCompile("(?m)" + re).MatchString(out) }

// A fresh box: every step says what is missing, and what to do about it is a
// command to type — or, where nothing here can do it, where to read how.
func TestChecklistOfAFreshBox(t *testing.T) {
	_, srv := newPortal(t, freshBox())
	code, out, errs := run(t, "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	u := srv.URL
	for _, s := range []string{
		u + " — first-run setup · 1 of 4 steps done",
		"  metadata     to do — no TMDB key — titles keep their file names, and get no posters or plots",
		"               next: zae setup metadata --tmdb-key-file FILE --url " + u,
		"               FILE holds TMDB's API read access token (v4, it starts with eyJ)",
		"  library      to do — no titles yet",
		"               no scan has run yet",
		"               copy files into the media/ folder of the media volume — the catalog reads it as /var/lib/katalog/media — then scan",
		"               read: " + docsFirstRun + " — copying files to the appliance",
		"               next: copy files there, then zae setup scan --url " + u,
		"  processing   optional — the media pipeline is off — files stream as they are",
		"               GPU: portal-api's Role grants no reads of the cluster's nodes, so whether one offers a GPU cannot be told from here",
		"               next: zae setup pipeline on --url " + u + " — it analyzes, transcodes and packages titles for adaptive streaming",
		"  devices      to do — zaentrum.localhost answers on this machine only — phones and TVs need a name they reach, served over https",
		"               read: " + docs,
		"  people       manual — accounts for the people who use this server are made in the identity provider's admin console",
		"  marked done  no — the launchpad shows this checklist to admins until it is",
		"               next, once you are done: zae setup done --url " + u,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("the checklist lacks %q:\n%s", s, out)
		}
	}
	// The steps in the launchpad's order.
	at := -1
	for _, name := range []string{"metadata", "library", "processing", "devices", "people", "marked done"} {
		i := strings.Index(out, "\n  "+name)
		if i <= at {
			t.Fatalf("%s is out of order:\n%s", name, out)
		}
		at = i
	}
}

// Every step done but the record: what is in effect, how the last scan went,
// the pipeline's workers — and what is left is to mark it done.
func TestChecklistOfASetUpPlatform(t *testing.T) {
	p, srv := newPortal(t, setUp())
	code, out, errs := run(t, "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	for _, s := range []string{
		"first-run setup · 4 of 4 steps done",
		"  metadata     done — the catalog manager's own TMDB key is in effect; one set with zae setup metadata takes its place",
		"  library      done — 28 titles",
		"               the last scan, 3 min ago: 33 files, 0 new, 33 updated",
		"               more files go into the media/ folder of the media volume (/var/lib/katalog/media to the catalog); scan again after copying",
		"  processing   done — the media pipeline runs",
		"               workers: analyzer ready 1/1 · katalog-ingest ready 1/1 · packager ready 2/2 · transcoder ready 1/1",
		"               GPU: 1 node offers one",
		"  devices      done — phones and TVs can sign in: media.example.org answers over https",
		"  marked done  no — the launchpad shows this checklist to admins until it is",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("the checklist lacks %q:\n%s", s, out)
		}
	}
	for _, s := range []string{"next: zae setup metadata", "next: zae setup scan", "next: zae setup pipeline", "  " + "read: " + docs + "\n"} {
		if strings.Contains(out, s) {
			t.Errorf("a step that is done offers %q:\n%s", s, out)
		}
	}

	// Marked done: by whom, when, and how to show it again.
	p.doc.Completed = &Completion{At: clock.Add(-50 * time.Hour), By: "admin"}
	_, out, _ = run(t, "--url", srv.URL)
	for _, s := range []string{
		"  marked done  yes, by admin, 2 days ago (2026-10-02 06:00 UTC)",
		"               zae setup reopen --url " + srv.URL + " shows the checklist again",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("the checklist lacks %q:\n%s", s, out)
		}
	}
	if n := p.called("GET " + setupPath); n != 2 || len(p.writes()) != 0 {
		t.Errorf("each checklist is one read, and no write: %v", p.calls)
	}
}

// What is under way, what broke, and what the portal cannot tell, in its
// words — which reach the terminal without their control characters.
func TestChecklistOfStepsUnderWayBrokenOrUnknown(t *testing.T) {
	d := setUp()
	started := clock.Add(-2 * time.Minute)
	d.Library = Library{Step: Step{State: stateWorking}, Titles: 28, Path: "/var/lib/katalog/media",
		Scan: &ScanJob{Status: "running", StartedAt: &started}}
	d.Processing = Processing{Step: Step{State: stateTodo}, Pipeline: ptr(true), GPUNodes: ptr(0), Switchable: true,
		Workers: []Worker{
			{Name: "analyzer", Phase: "ready", Ready: 1, Desired: 1},
			{Name: "transcoder", Phase: "progressing", Reason: "Unschedulable", Desired: 1},
		}}
	d.Metadata = Metadata{Step: Step{State: stateUnknown, Note: "the catalog manager did not answer: dial tcp: connection refused\x1b[2J"}, Key: "none"}
	d.Devices = Devices{Step: Step{State: stateTodo}, Origin: "https://media.example.org", HTTPS: true,
		Issuer: "http://sso.example.org/realms/media"}
	_, srv := newPortal(t, d)
	code, out, errs := run(t, "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("a checklist is a checklist, whatever its steps say: %d\n%s\n%s", code, out, errs)
	}
	for _, s := range []string{
		"first-run setup · 0 of 4 steps done",
		"  metadata     unknown — cannot tell whether a TMDB key is set: the catalog manager did not answer: dial tcp: connection refused?[2J",
		"  library      scanning — 28 titles",
		"               a scan is running, started 2 min ago — new titles appear as it finds them",
		"               more files go to /var/lib/katalog/media, where the catalog reads them; scan again after copying",
		"  processing   to do — the media pipeline is on, but the transcoder cannot run: no node offers the NVIDIA GPU it asks for (nvidia.com/gpu)",
		"               workers: analyzer ready 1/1 · transcoder progressing 0/1 (Unschedulable)",
		"               GPU: no node offers one, which the transcoder needs",
		"               next: fix what the cluster names — zae platform status --url " + srv.URL + " shows the workloads — or zae setup pipeline off --url " + srv.URL,
		"  devices      to do — sign-in (sso.example.org) answers over http, and phones and TVs sign in over https only",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("the checklist lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("a note drove the terminal:\n%q", out)
	}

	// A scan that has run for an hour has most likely stopped: scan again.
	started = clock.Add(-time.Hour)
	d.Library.Scan.StartedAt = &started
	d.Library.State = stateDone
	// A pipeline that is starting, and one whose state this zae has not
	// heard of, shown as it came.
	d.Processing = Processing{Step: Step{State: stateWorking}, Pipeline: ptr(true), Switchable: true,
		Workers: []Worker{{Name: "analyzer", Phase: "ready", Ready: 1, Desired: 1}, {Name: "packager", Phase: "absent"}, {Name: "transcoder", Phase: "progressing", Desired: 1}}}
	d.Devices.State = "deferred"
	_, srv2 := newPortal(t, d)
	_, out, _ = run(t, "--url", srv2.URL)
	for _, s := range []string{
		"               a scan started 1 h ago has not ended — it may have stopped; scan again",
		"               next: zae setup scan --url " + srv2.URL,
		"  processing   starting — the media pipeline is starting: packager and transcoder are not ready yet",
		"               workers: analyzer ready 1/1 · packager not running yet · transcoder progressing 0/1",
		"  devices      deferred — ",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("the checklist lacks %q:\n%s", s, out)
		}
	}
}

// --json is the portal's own document: a script reads the API's shape, not
// zae's rendering of it.
func TestChecklistJSONIsThePortalsOwnDocument(t *testing.T) {
	_, srv := newPortal(t, freshBox())
	code, out, errs := run(t, "--url", srv.URL, "--json")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d %s", code, errs)
	}
	var got Doc
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json is not the checklist: %v\n%s", err, out)
	}
	if got.Metadata.Key != "none" || got.Library.Path != "/var/lib/katalog/media" || !got.Processing.Switchable || !got.Devices.LocalOnly {
		t.Errorf("--json lost fields: %+v", got)
	}
	for _, s := range []string{"next:", "first-run setup", "to do"} {
		if strings.Contains(out, s) {
			t.Errorf("--json carries rendered text %q:\n%s", s, out)
		}
	}
}

// The contract every read keeps: an admin's bearer, the portal's words for a
// refusal, and exit codes a script can branch on.
func TestChecklistRefusalsAndOldPortals(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	p.token = "admin-bearer"
	code, _, errs := run(t, "--url", srv.URL)
	if code != exitcode.Forbidden || !strings.Contains(errs, "supply a bearer via "+instance.TokenEnv) || !strings.Contains(errs, adminNeed) {
		t.Errorf("without a bearer: want 5 with the hint, got %d %q", code, errs)
	}
	t.Setenv(instance.TokenEnv, "admin-bearer")
	if code, _, errs := run(t, "--url", srv.URL); code != exitcode.OK {
		t.Fatalf("with the admin's bearer: want 0, got %d %q", code, errs)
	}

	p.old = true
	if code, _, errs := run(t, "--url", srv.URL); code != exitcode.NotOffered || !strings.Contains(errs, "does not serve "+setupPath+" — its portal-api predates the setup checklist") {
		t.Errorf("a portal-api without the checklist: want 3, got %d %q", code, errs)
	}
	p.old = false

	p.answer = `{"available":true,"instances":[]}`
	if code, _, errs := run(t, "--url", srv.URL); code != exitcode.Undetermined || !strings.Contains(errs, "not the setup checklist") {
		t.Errorf("JSON that is not the checklist: want 4, got %d %q", code, errs)
	}
	if code, _, errs := run(t, "--url", "http://127.0.0.1:1"); code != exitcode.Undetermined || !strings.Contains(errs, "not concluding") {
		t.Errorf("unreachable: want 4, got %d %q", code, errs)
	}
}

// The bearer comes from the session `zae login` stored for this instance, and
// is never printed.
func TestTheBearerComesFromTheStoredSession(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	p.token = "stored-session-bearer"
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(instance.TokenEnv, "")
	if err := creds.Put(srv.URL, creds.Entry{Issuer: "https://issuer.example.org", ClientID: "zae",
		AccessToken: "stored-session-bearer", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "--url", srv.URL)
	if code != exitcode.OK || len(p.auth) == 0 || p.auth[0] != "Bearer stored-session-bearer" {
		t.Fatalf("the stored session must be sent: %d %q\n%s", code, p.auth, errs)
	}
	if strings.Contains(out+errs, "stored-session-bearer") {
		t.Fatalf("the bearer was printed:\n%s\n%s", out, errs)
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

func TestChecklistUsage(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	usageCases(t, p, map[string][]string{
		"nothing at all":  {},
		"no url":          {"--json"},
		"an argument":     {"--url", srv.URL, "everything"},
		"unknown command": {"status", "--url", srv.URL},
		"not an address":  {"--url", "media.example.org"},
		"unknown flag":    {"--url", srv.URL, "--verbose"},
	})
	if code, out, _ := run(t, "help"); code != exitcode.OK || !strings.Contains(out, "zae setup --url https://…") {
		t.Fatalf("help: want 0 on stdout, got %d\n%s", code, out)
	}
}
