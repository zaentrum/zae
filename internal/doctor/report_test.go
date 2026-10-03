package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readReport(t *testing.T, path string) reportDoc {
	t.Helper()
	var doc reportDoc
	if err := json.Unmarshal(mustRead(t, path), &doc); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	return doc
}

// The shape the operator reads, exactly: short keys, the counts, and each
// check's status spelled ok|warn|fail|skip.
func TestReportShape(t *testing.T) {
	b := buildReport("v1.2.3", "https://media.example.org", []Result{
		{Name: "tls", Status: OK, Detail: "certificate valid, 52 days left"},
		{Name: "routes", Status: Fail, Detail: "not serving: /portal (502)", Fix: "check the route map"},
		{Name: "issuer identity", Status: Warn, Detail: "discovery names itself x"},
		{Name: "sign-in", Status: Skip, Detail: "no credentials"},
	})
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "zae", "url", "passed", "failed", "warned", "skipped", "checks"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("the report lacks %q: %s", k, b)
		}
	}
	var doc reportDoc
	_ = json.Unmarshal(b, &doc)
	if doc.V != 1 || doc.Zae != "v1.2.3" || doc.URL != "https://media.example.org" ||
		doc.Passed != 1 || doc.Failed != 1 || doc.Warned != 1 || doc.Skipped != 1 {
		t.Fatalf("the header: %+v", doc)
	}
	want := []reportCheck{
		{N: "tls", S: "ok", D: "certificate valid, 52 days left"},
		{N: "routes", S: "fail", D: "not serving: /portal (502) · fix: check the route map"},
		{N: "issuer identity", S: "warn", D: "discovery names itself x"},
		{N: "sign-in", S: "skip", D: "no credentials"},
	}
	if fmt.Sprint(doc.Checks) != fmt.Sprint(want) {
		t.Fatalf("checks:\n got %v\nwant %v", doc.Checks, want)
	}
	if !strings.HasSuffix(string(b), "}\n") || strings.Count(string(b), "\n") != 1 {
		t.Errorf("one line, newline-terminated: %q", b)
	}
}

// The kubelet keeps 4096 bytes of a termination message and cuts the rest
// wherever it falls. However many checks there are and however long their
// details, the report fits — and every failure is in it, with its counts.
func TestReportFitsATerminationMessageAndKeepsEveryFailure(t *testing.T) {
	long := strings.Repeat("a detail that goes on — and on, ünïcödé included — ", 20)
	var results []Result
	for i := 0; i < 60; i++ {
		st := []Status{OK, OK, Skip, Warn, OK, Fail}[i%6]
		results = append(results, Result{Name: fmt.Sprintf("check %02d", i), Status: st, Detail: long, Fix: "a fix that is long too " + long})
	}
	b := buildReport("dev", "https://media.example.org", results)
	if len(b) > reportLimit {
		t.Fatalf("the report is %d bytes, over %d", len(b), reportLimit)
	}
	if !utf8.Valid(b) {
		t.Fatal("a cut fell inside a character")
	}
	var doc reportDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("a report that does not parse is worth nothing: %v", err)
	}
	fails := 0
	for _, c := range doc.Checks {
		if c.S == "fail" {
			fails++
		}
	}
	if fails != 10 || doc.Failed != 10 {
		t.Fatalf("every failure is kept: %d of %d in the list (counted %d)", fails, 10, doc.Failed)
	}
	if doc.Passed != 30 || doc.Skipped != 10 || doc.Warned != 10 {
		t.Errorf("the counts are the run's own, whatever was left out: %+v", doc)
	}
}

// What goes first is what matters least: a passing check's detail, before
// anything of a failure.
func TestReportGivesUpPassingDetailsFirst(t *testing.T) {
	long := strings.Repeat("x", 250)
	var results []Result
	for i := 0; i < 14; i++ {
		results = append(results, Result{Name: fmt.Sprintf("ok %02d", i), Status: OK, Detail: long})
	}
	results = append(results, Result{Name: "the failure", Status: Fail, Detail: "chino-api answered HTTP 502", Fix: "the catalog is down"})
	var doc reportDoc
	if err := json.Unmarshal(buildReport("dev", "https://media.example.org", results), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Checks) != 15 {
		t.Fatalf("there is room for every line once the passing details go: %d", len(doc.Checks))
	}
	for _, c := range doc.Checks {
		switch c.S {
		case "ok":
			if c.D != "" {
				t.Errorf("a passing detail kept while room was short: %+v", c)
			}
		case "fail":
			if c.D != "chino-api answered HTTP 502 · fix: the catalog is down" {
				t.Errorf("the failure kept all it said: %+v", c)
			}
		}
	}

	// And when there is room, nothing is given up.
	small := buildReport("dev", "https://media.example.org", results[:3])
	if !strings.Contains(string(small), long) {
		t.Errorf("a report with room keeps the details: %s", small)
	}
}

// Even a run of nothing but failures fits; the ones that do not are left out
// last, and the count still says how many there were.
func TestReportOfOnlyFailures(t *testing.T) {
	var results []Result
	for i := 0; i < 400; i++ {
		results = append(results, Result{Name: fmt.Sprintf("failing check number %03d", i), Status: Fail, Detail: "broken"})
	}
	b := buildReport("dev", "https://media.example.org", results)
	var doc reportDoc
	if len(b) > reportLimit || json.Unmarshal(b, &doc) != nil {
		t.Fatalf("%d bytes, or not JSON", len(b))
	}
	if doc.Failed != 400 || len(doc.Checks) == 0 || len(doc.Checks) >= 400 {
		t.Fatalf("count %d, listed %d", doc.Failed, len(doc.Checks))
	}
}

// --report writes the file whatever the outcome, and replaces what was there.
func TestReportFlagWritesTheFile(t *testing.T) {
	path := t.TempDir() + "/termination-log"
	if err := os.WriteFile(path, []byte(strings.Repeat("old ", 2000)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := doctor(t, "--url", "https://media.example.org", "--report", path)
	if code != 0 {
		t.Fatalf("want 0, got %d\n%s", code, out)
	}
	doc := readReport(t, path)
	if doc.Passed != 1 || len(doc.Checks) != 1 || doc.Checks[0].N != "tls" {
		t.Fatalf("the report of the run: %+v", doc)
	}
	if strings.Contains(string(mustRead(t, path)), "old ") {
		t.Fatal("what was in the file before must be gone")
	}

	// A path that cannot be written is said; the verdict stays the run's.
	code, _, errs := doctor(t, "--url", "https://media.example.org", "--report", t.TempDir()+"/no/such/dir/log")
	if code != 0 || !strings.Contains(errs, "could not write the report") {
		t.Fatalf("want 0 and the error said, got %d %q", code, errs)
	}
}

// A word that is not a flag would make every flag after it be ignored — a
// --sign-in among them. That is refused, not swallowed.
func TestDoctorRefusesStrayArguments(t *testing.T) {
	code, _, errs := doctor(t, "--url", "https://media.example.org", "now", "--sign-in")
	if code != 2 || !strings.Contains(errs, `"now"`) {
		t.Fatalf("want 2 naming the word, got %d %q", code, errs)
	}
	if code, _, _ := doctor(t, "--sign-in"); code != 2 {
		t.Fatalf("--url is still required: %d", code)
	}
}

// The report carries the signed-in checks, and no credential.
func TestTheReportOfASignedInRun(t *testing.T) {
	w := newWorld(t)
	w.password = "something else"
	signedIn(t)
	path := t.TempDir() + "/termination-log"
	code, _, _ := doctor(t, "--url", w.srv.URL, "--sign-in", "--report", path)
	if code != 1 {
		t.Fatalf("want 1, got %d", code)
	}
	doc := readReport(t, path)
	noSecrets(t, "the report", string(mustRead(t, path)))
	found := false
	for _, c := range doc.Checks {
		if c.N == "sign-in: login form" {
			found = c.S == "fail" && strings.Contains(c.D, "wrong password") && strings.Contains(c.D, "fix:")
		}
	}
	if !found || doc.Failed != 1 || doc.V != 1 || doc.Zae != "test" || doc.URL != w.srv.URL {
		t.Fatalf("the report: %+v", doc)
	}
}

// "<" stays "<": escaped it costs six bytes of a budget that has none to
// spare. And a cut never falls inside a character — not even where every
// character is more than one byte.
func TestReportSpendsItsBytesOnText(t *testing.T) {
	b := buildReport("dev", "https://media.example.org", []Result{
		{Name: "app /chino/", Status: Fail, Detail: "answered <html> & a page"},
	})
	if !strings.Contains(string(b), `"answered <html> & a page"`) {
		t.Fatalf("the detail was escaped: %s", b)
	}
	wide := strings.Repeat("ü", 2000)
	var results []Result
	for i := 0; i < 40; i++ {
		results = append(results, Result{Name: fmt.Sprintf("c%02d", i), Status: Fail, Detail: wide})
	}
	b = buildReport("dev", "https://media.example.org", results)
	if strings.Contains(string(b), "\uFFFD") || !utf8.Valid(b) {
		t.Fatalf("a cut fell inside a character: %q", b[:200])
	}
	var doc reportDoc
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Checks) != 40 {
		t.Fatalf("every failure, as JSON: %v, %d", err, len(doc.Checks))
	}
	for _, c := range doc.Checks {
		if d := strings.TrimSuffix(c.D, "…"); strings.Trim(d, "ü") != "" {
			t.Fatalf("a detail cut to something else than its own characters: %q", c.D)
		}
	}
}
