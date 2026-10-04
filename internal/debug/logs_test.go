package debug

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
)

func TestLogsOfAWorkloadAreItsPodsMergedByTime(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	code, out, errs := run(t, "logs", "chino-api", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	want := strings.Join([]string{
		"[chino-api-7d79fd4c4b-xprff/app] " + line(1, "first replica starts"),
		"[chino-api-7d79fd4c4b-qfmtj/app] " + line(2, "second replica starts"),
		"[chino-api-7d79fd4c4b-qfmtj/app] " + line(3, "second replica serves"),
		"[chino-api-7d79fd4c4b-xprff/app] " + line(4, "first replica serves"),
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("both replicas, by time, each line with its pod:\n got:\n%s\nwant:\n%s", out, want)
	}
	// The pods of a workload whose name only starts like this one's are not
	// its pods: "worker" is not a template hash.
	if strings.Contains(out, "worker") || p.called("GET "+logsPath+"?container=app&pod=chino-api-worker") != 0 {
		t.Errorf("another workload's pod was read:\n%s", out)
	}
	// Every read names its container, and asks for nothing the flags did not.
	for _, q := range p.logQ {
		if q.Get("container") == "" || q.Has("tail") || q.Has("since") {
			t.Errorf("a read asked for %v", q)
		}
	}
}

// One container, one pod: the lines exactly as the portal sent them.
func TestLogsOfOneContainerAreTheLinesAsSent(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	for name, want := range map[string]string{
		"postgres":                    line(1, "database system is ready") + "\n", // a StatefulSet's pod
		"zaentrum-verify-fzzgp":       line(1, "doctor: no failures") + "\n",      // a Job's
		"chino-web-58f8548947-ljdwb":  line(1, "web") + "\n",                      // a pod by its own name
		"chino-api-7d79fd4c4b-qfmtj":  line(2, "second replica starts") + "\n" + line(3, "second replica serves") + "\n",
		"chino-api-worker":            line(1, "the worker, not the api") + "\n",
		"portal-api-554bd55786-krtkx": "[portal-api-554bd55786-krtkx/app] " + line(1, "portal app") + "\n[portal-api-554bd55786-krtkx/proxy] " + line(2, "portal proxy") + "\n",
	} {
		code, out, errs := run(t, "logs", name, "--url", srv.URL)
		if code != exitcode.OK || out != want {
			t.Errorf("%s: want 0 and\n%s\ngot %d\n%s\n%s", name, want, code, out, errs)
		}
	}
	code, out, _ := run(t, "logs", "portal-api", "--container", "proxy", "--url", srv.URL)
	if code != exitcode.OK || out != line(2, "portal proxy")+"\n" {
		t.Errorf("--container picks one: %d\n%s", code, out)
	}
}

func TestLogsSendsTailAndSince(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	code, out, errs := run(t, "logs", "postgres", "--url", srv.URL, "--tail", "20", "--since", "90s")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	q := p.logQ[0]
	if q.Get("tail") != "20" || q.Get("since") != "90" || q.Get("pod") != "postgres-0" || q.Get("container") != "postgres" {
		t.Fatalf("the read: %v", q)
	}
	// A part of a second is a whole one: the portal takes seconds, and a
	// window rounded down would leave out what was asked for.
	if _, _, _ = run(t, "logs", "postgres", "--url", srv.URL, "--since", "1500ms"); p.logQ[1].Get("since") != "2" {
		t.Fatalf("--since 1500ms: %v", p.logQ[1])
	}
}

// The portal redacts; zae redacts again, with the same rules — here the
// portal is an older one that let a credential through.
func TestLogsAreRedactedAgain(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	p.logs["postgres-0/postgres"] = []string{
		line(1, "connecting with Authorization: Bearer "+leaked),
		line(2, "dsn postgres://app:"+leaked+"@postgres:5432/app"),
		line(3, `config {"clientSecret":"`+leaked+`"}`),
	}
	for _, args := range [][]string{
		{"logs", "postgres", "--url", srv.URL},
		{"logs", "postgres", "--url", srv.URL, "--json"},
	} {
		code, out, errs := run(t, args...)
		if code != exitcode.OK || strings.Contains(out+errs, leaked) {
			t.Fatalf("%v: a credential reached the terminal (exit %d):\n%s", args, code, out)
		}
		if !strings.Contains(out, "REDACTED") || !strings.Contains(out, "@postgres:5432/app") {
			t.Errorf("%v: the lines must stay, with the credential replaced:\n%s", args, out)
		}
	}
}

// --json is one object per line, the timestamp apart from the text.
func TestLogsJSONIsOneObjectPerLine(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	p.logs["postgres-0/postgres"] = append(p.logs["postgres-0/postgres"], line(2, `a "quoted" <tag> & more`))
	code, out, errs := run(t, "logs", "postgres", "--url", srv.URL, "--json")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d %s", code, errs)
	}
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if len(rows) != 2 {
		t.Fatalf("want one object per line:\n%s", out)
	}
	var l logLine
	if err := json.Unmarshal([]byte(rows[1]), &l); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rows[1])
	}
	if l.Pod != "postgres-0" || l.Container != "postgres" || l.Time != at(2) || l.Line != `a "quoted" <tag> & more` {
		t.Fatalf("the object: %+v", l)
	}
	if !strings.Contains(rows[1], "<tag> & more") {
		t.Errorf("the text is written as it is, not HTML-escaped: %s", rows[1])
	}
}

// What is not there is exit 3, and the message says what is.
func TestLogsOfWhatIsNotThere(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	code, _, errs := run(t, "logs", "katalog-api", "--url", srv.URL)
	if code != exitcode.NotOffered {
		t.Fatalf("an unknown workload: want 3, got %d %q", code, errs)
	}
	// What runs, by the workloads the portal names: the verification's Job
	// by its own name, as `zae platform status` shows it.
	for _, s := range []string{`no workload named "katalog-api"`, "it runs chino-api, chino-api-worker, chino-web, portal-api, postgres, zaentrum-verify-fzzgp ("} {
		if !strings.Contains(errs, s) {
			t.Errorf("the message lacks %q: %q", s, errs)
		}
	}
	// An older portal-api names no owners, and the names are read off the
	// pods' — where a Job's generated suffix reads as a template hash.
	p.legacy = true
	if _, _, errs := run(t, "logs", "katalog-api", "--url", srv.URL); !strings.Contains(errs, "it runs chino-api, chino-api-worker, chino-web, portal-api, postgres, zaentrum-verify (") {
		t.Errorf("an older portal-api: the names its pods say: %q", errs)
	}
	p.legacy = false
	code, _, errs = run(t, "logs", "portal-api", "--container", "sidecar", "--url", srv.URL)
	if code != exitcode.NotOffered || !strings.Contains(errs, `no container "sidecar"`) || !strings.Contains(errs, "its containers are app, proxy") {
		t.Fatalf("an unknown container: want 3 naming the ones there, got %d %q", code, errs)
	}

	empty, srv2 := newPortal(t)
	empty.pods = []Pod{}
	code, _, errs = run(t, "logs", "chino-api", "--url", srv2.URL)
	if code != exitcode.NotOffered || !strings.Contains(errs, "not running in a cluster") {
		t.Fatalf("no pods at all: want 3, got %d %q", code, errs)
	}

	p3, srv3 := newPortal(t)
	exampleNamespace(p3)
	p3.logs503 = "log viewer is unavailable (not running in a cluster)"
	code, _, errs = run(t, "logs", "postgres", "--url", srv3.URL)
	if code != exitcode.NotOffered || !strings.Contains(errs, "has no pods to read") {
		t.Fatalf("the portal's own 503: want 3, got %d %q", code, errs)
	}
}

// A container the portal cannot read is said, and the others are printed:
// one pod that is still starting must not hide its siblings.
func TestLogsPrintsWhatItCouldRead(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	p.broken["chino-api-7d79fd4c4b-qfmtj/app"] = `k8s 400 BadRequest: container "app" in pod "chino-api-7d79fd4c4b-qfmtj" is waiting to start: ContainerCreating`
	code, out, errs := run(t, "logs", "chino-api", "--url", srv.URL)
	if code != exitcode.Failed {
		t.Fatalf("want 1, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "first replica serves") || !strings.Contains(errs, "is waiting to start") {
		t.Fatalf("the readable lines and the reason for the rest:\n%s\n%s", out, errs)
	}
}

func TestMatchAndWorkloadNames(t *testing.T) {
	for pod, want := range map[string]string{
		"chino-api-7d79fd4c4b-xprff":  "chino-api",
		"chino-api-worker-5c-rhbqb":   "chino-api-worker", // a one-character hash is a hash
		"postgres-0":                  "postgres",
		"postgres-12":                 "postgres",
		"seed-demo-content-xmmqz":     "seed-demo-content",
		"keycloak":                    "keycloak",
		"my-pod-abcde":                "my-pod-abcde",    // vowels: not generated
		"web-07":                      "web-07",          // not an ordinal
		"api-7d79fd4c4b9-xprff":       "api-7d79fd4c4b9", // eleven characters are no template hash
		"zaentrum-verify-fzzgp-gsg9z": "zaentrum-verify",
	} {
		if got := workloadOf(pod); got != want {
			t.Errorf("workloadOf(%q) = %q, want %q", pod, got, want)
		}
	}
	pods := []Pod{{Pod: "api-bcd-xprff", Containers: []string{"app"}}, {Pod: "api-bcd", Containers: []string{"app"}}}
	if srcs, exact := match("api-bcd", "", pods); !exact || len(srcs) != 1 || srcs[0].pod != "api-bcd" {
		t.Errorf("a pod's own name wins over a workload's: %v %v", srcs, exact)
	}
}

// The portal names what runs each pod, and a workload's pods are found by
// that — not by their names, which cannot tell a Job made from another
// workload's name apart from that workload: api-bcdfg's pods read like the
// Deployment api's. Live, the demo's migration Job postgres-migrate-b27zs reads
// like a Deployment postgres-migrate by its pod's name.
func TestLogsFindAWorkloadsPodsByTheOwnerThePortalNames(t *testing.T) {
	p, srv := newPortal(t)
	p.pods = []Pod{
		owned("api-7d79fd4c4b-xprff", "Deployment", "api", "app"),
		owned("api-bcdfg-qfmtj", "Job", "api-bcdfg", "job"),
		owned("postgres-migrate-b27zs-l9pbg", "Job", "postgres-migrate-b27zs", "migrate"),
		owned("debug-shell", "", "", "shell"),
	}
	p.logs = map[string][]string{
		"api-7d79fd4c4b-xprff/app":             {line(1, "the api")},
		"api-bcdfg-qfmtj/job":                  {line(2, "a job made from the api's name")},
		"postgres-migrate-b27zs-l9pbg/migrate": {line(3, "migrated")},
		"debug-shell/shell":                    {line(4, "a pod nothing owns")},
	}
	if code, out, errs := run(t, "logs", "api", "--url", srv.URL); code != exitcode.OK || out != line(1, "the api")+"\n" {
		t.Errorf("api is its Deployment's pods, and only those: %d\n%s\n%s", code, out, errs)
	}
	if code, out, _ := run(t, "logs", "postgres-migrate-b27zs", "--url", srv.URL); code != exitcode.OK || out != line(3, "migrated")+"\n" {
		t.Errorf("a Job, by its own name: %d\n%s", code, out)
	}
	code, _, errs := run(t, "logs", "postgres-migrate", "--url", srv.URL)
	if code != exitcode.NotOffered || !strings.Contains(errs, "it runs api, api-bcdfg, debug-shell, postgres-migrate-b27zs (") {
		t.Errorf("no workload of that name runs the Job's pod: want 3 naming what runs, got %d %q", code, errs)
	}
	if code, out, _ := run(t, "logs", "debug-shell", "--url", srv.URL); code != exitcode.OK || out != line(4, "a pod nothing owns")+"\n" {
		t.Errorf("a pod nothing owns, by its own name: %d\n%s", code, out)
	}

	// The same namespace from an older portal-api: the names are all there
	// is, and they are read as they always were.
	p.legacy = true
	if code, out, _ := run(t, "logs", "api", "--url", srv.URL); code != exitcode.OK || !strings.Contains(out, "[api-bcdfg-qfmtj/job] ") {
		t.Errorf("an older portal-api: every pod named for api: %d\n%s", code, out)
	}
	if code, out, _ := run(t, "logs", "postgres-migrate", "--url", srv.URL); code != exitcode.OK || out != line(3, "migrated")+"\n" {
		t.Errorf("an older portal-api: the pod named for postgres-migrate: %d\n%s", code, out)
	}
}

// --follow reads every round, prints each line once, and takes up the pods a
// rollout brings while it says which ones went.
func TestFollowPrintsEachLineOnceThroughARollout(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	p.onList = func(p *fakePortal, n int) {
		a := "chino-api-7d79fd4c4b-xprff/app"
		switch n {
		case 2:
			p.logs[a] = append(p.logs[a], line(5, "first replica, later"))
		case 3:
			// The rollout: one old replica is gone, its replacement is up.
			p.pods = append([]Pod{owned("chino-api-5f6d8c9b7d-wv2bn", "Deployment", "chino-api", "app")}, p.pods[1:]...)
			p.logs["chino-api-5f6d8c9b7d-wv2bn/app"] = []string{line(6, "new replica starts")}
		case 4:
			k := "chino-api-5f6d8c9b7d-wv2bn/app"
			p.logs[k] = append(p.logs[k], line(7, "new replica serves"))
		case 6:
			go interrupt()
		}
	}
	code, out, errs := run(t, "logs", "chino-api", "--url", srv.URL, "--follow")
	if code != 130 {
		t.Fatalf("Ctrl-C ends a follow with 130, got %d\n%s\n%s", code, out, errs)
	}
	want := []string{
		"[chino-api-7d79fd4c4b-xprff/app] " + line(1, "first replica starts"),
		"[chino-api-7d79fd4c4b-qfmtj/app] " + line(2, "second replica starts"),
		"[chino-api-7d79fd4c4b-qfmtj/app] " + line(3, "second replica serves"),
		"[chino-api-7d79fd4c4b-xprff/app] " + line(4, "first replica serves"),
		"[chino-api-7d79fd4c4b-xprff/app] " + line(5, "first replica, later"),
		"[chino-api-5f6d8c9b7d-wv2bn/app] " + line(6, "new replica starts"),
		"[chino-api-5f6d8c9b7d-wv2bn/app] " + line(7, "new replica serves"),
	}
	if got := strings.Split(strings.TrimSpace(out), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("each line once, in order:\n got:\n%s\nwant:\n%s", out, strings.Join(want, "\n"))
	}
	for _, s := range []string{"chino-api-7d79fd4c4b-xprff/app is gone", "following chino-api-5f6d8c9b7d-wv2bn/app too"} {
		if !strings.Contains(errs, s) {
			t.Errorf("the notes lack %q: %q", s, errs)
		}
	}
	// After the first read, each one asks for the window since the last —
	// a few seconds, not the whole log again — and as many lines as the
	// portal gives.
	for _, q := range p.logQ[2:] {
		since, _ := strconv.Atoi(q.Get("since"))
		if q.Get("tail") != strconv.Itoa(maxTail) || since < 1 || since > 8 {
			t.Errorf("a follow read asked for %v", q)
		}
	}
}

// A failure that passes is said once and followed through; a refusal ends
// the follow with its own exit code.
func TestFollowRidesOutAFailureAndStopsAtARefusal(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	k := "postgres-0/postgres"
	p.onList = func(p *fakePortal, n int) {
		switch n {
		case 2, 3, 4:
			p.broken[k] = "k8s 503 ServiceUnavailable: the kubelet did not answer"
		case 5:
			delete(p.broken, k)
			p.logs[k] = append(p.logs[k], line(9, "after the hiccup"))
		case 7:
			p.token = "someone-else" // the session lost its role
		}
	}
	code, out, errs := run(t, "logs", "postgres", "--url", srv.URL, "--follow")
	if code != exitcode.Forbidden {
		t.Fatalf("a refusal ends the follow with 5, got %d\n%s\n%s", code, out, errs)
	}
	if n := strings.Count(errs, "the kubelet did not answer"); n != 1 {
		t.Errorf("a repeated failure is said once, said %d times:\n%s", n, errs)
	}
	if !strings.Contains(errs, "reading postgres-0/postgres again") || !strings.Contains(out, "after the hiccup") {
		t.Errorf("the follow must recover and say so:\n%s\n%s", out, errs)
	}
	if strings.Count(out, "database system is ready") != 1 {
		t.Errorf("the line from before the failure is printed once:\n%s", out)
	}
	// A workload followed through a rollout reads more pods than it began
	// with, so even one pod's lines say whose they are.
	if !strings.Contains(out, "[postgres-0/postgres] "+line(9, "after the hiccup")) {
		t.Errorf("a followed workload's lines carry their pod:\n%s", out)
	}
}

func TestLogsUsage(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	usageCases(t, p, map[string][]string{
		"no name":                {"logs", "--url", srv.URL},
		"two names":              {"logs", "a", "b", "--url", srv.URL},
		"not a name":             {"logs", "Chino_API", "--url", srv.URL},
		"no url":                 {"logs", "postgres"},
		"tail zero":              {"logs", "postgres", "--url", srv.URL, "--tail", "0"},
		"tail beyond the portal": {"logs", "postgres", "--url", srv.URL, "--tail", "5001"},
		"since zero":             {"logs", "postgres", "--url", srv.URL, "--since", "0s"},
		"a bad container":        {"logs", "postgres", "--url", srv.URL, "--container", "Bad Name"},
	})
}

// A line can arrive after the newest one printed with the very same stamp:
// the cluster stamps every line of one write alike, and a read may see only
// the first lines of it. It is printed, once — and the lines read again are
// not.
func TestFollowPrintsALateLineWithTheNewestStamp(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	k := "postgres-0/postgres"
	p.logs[k] = []string{line(1, "first of one write")}
	p.onList = func(p *fakePortal, n int) {
		switch {
		case n == 2:
			p.logs[k] = append(p.logs[k], line(1, "second of the same write"))
		case n == 4:
			go interrupt()
		}
	}
	code, out, errs := run(t, "logs", "postgres-0", "--url", srv.URL, "--follow")
	if code != 130 {
		t.Fatalf("want 130, got %d\n%s\n%s", code, out, errs)
	}
	if want := line(1, "first of one write") + "\n" + line(1, "second of the same write") + "\n"; out != want {
		t.Fatalf("each line once:\n got:\n%s\nwant:\n%s", out, want)
	}
}

// Each read after the first asks for the window since the read before it,
// widened by the margin — measured on this machine's clock, here a minute a
// round — not for everything since the follow began, and never for less
// than the margin.
func TestFollowReadsTheWindowSinceTheLastRead(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	clock := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	p.onList = func(p *fakePortal, n int) {
		clock = clock.Add(time.Minute)
		if n == 6 {
			go interrupt()
		}
	}
	if code, out, errs := run(t, "logs", "postgres", "--url", srv.URL, "--follow"); code != 130 {
		t.Fatalf("want 130, got %d\n%s\n%s", code, out, errs)
	}
	if len(p.logQ) < 4 {
		t.Fatalf("too few reads: %v", p.logQ)
	}
	margin := int(followMargin / time.Second)
	// The first round reads in the minute the first read was made: the margin
	// alone. Every one after it, the minute since the read before.
	if since, _ := strconv.Atoi(p.logQ[1].Get("since")); since != margin {
		t.Errorf("the first follow read asked for %ds, want the margin, %ds", since, margin)
	}
	for _, q := range p.logQ[2:] {
		since, _ := strconv.Atoi(q.Get("since"))
		if want := 60 + margin; since != want {
			t.Errorf("a read asked for %ds, want the minute since the last read and the margin, %ds: %v", since, want, q)
		}
	}
	if margin < 5 {
		t.Errorf("a margin of %ds leaves a line written while a read was on its way unread", margin)
	}
}

// More lines than the portal returns in one read means some were not read:
// the follow says so instead of pretending to be complete.
func TestFollowSaysWhenAReadOverflows(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	k := "postgres-0/postgres"
	p.onList = func(p *fakePortal, n int) {
		switch {
		case n == 2:
			for i := 0; i < maxTail+10; i++ {
				p.logs[k] = append(p.logs[k], line(10+i, "busy"))
			}
		case n == 4:
			go interrupt()
		}
	}
	code, _, errs := run(t, "logs", "postgres", "--url", srv.URL, "--follow")
	if code != 130 || !strings.Contains(errs, "wrote more than 5000 lines between two reads — some of them were not read") {
		t.Fatalf("want 130 and the note, got %d %q", code, errs)
	}
}

// A line the portal sent without a readable stamp stays after the line before
// it when logs are merged — not first, as if it were the oldest.
func TestAnUnstampedLineKeepsItsPlace(t *testing.T) {
	p, srv := newPortal(t)
	exampleNamespace(p)
	p.logs["portal-api-554bd55786-krtkx/app"] = []string{line(1, "app starts"), "  at a continuation the stamp missed", line(3, "app serves")}
	p.logs["portal-api-554bd55786-krtkx/proxy"] = []string{line(2, "proxy starts")}
	_, out, _ := run(t, "logs", "portal-api", "--url", srv.URL)
	want := strings.Join([]string{
		"[portal-api-554bd55786-krtkx/app] " + line(1, "app starts"),
		"[portal-api-554bd55786-krtkx/app]   at a continuation the stamp missed",
		"[portal-api-554bd55786-krtkx/proxy] " + line(2, "proxy starts"),
		"[portal-api-554bd55786-krtkx/app] " + line(3, "app serves"),
	}, "\n") + "\n"
	if out != want {
		t.Fatalf("an unstamped line keeps its place:\n got:\n%s\nwant:\n%s", out, want)
	}
}
