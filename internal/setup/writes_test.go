package setup

import (
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
)

// scan starts one, says so, and follows nothing: the checklist shows it. It
// does not ask — no terminal and no --yes are needed — as the console's
// button does not.
func TestScanStartsAScan(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	code, out, errs := run(t, "scan", "--url", srv.URL)
	if code != exitcode.OK || p.called("POST "+scanPath) != 1 {
		t.Fatalf("want 0 and one scan, got %d %v\n%s\n%s", code, p.calls, out, errs)
	}
	for _, s := range []string{
		"started a scan on " + srv.URL + " — follow it with zae setup --url " + srv.URL,
		"  library      scanning — no titles yet",
		"               a scan is running, started just now — new titles appear as it finds them",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("scan lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "[y/N]") || p.bodies["POST "+scanPath][0] != "" {
		t.Errorf("no question, and no body:\n%s %q", out, p.bodies["POST "+scanPath])
	}
}

// A scan under way is waited out, as the console's button waits it out; one
// that has run for an hour has most likely stopped, and another is started.
func TestScanDoesNotStartASecondOne(t *testing.T) {
	d := setUp()
	started := clock.Add(-2 * time.Minute)
	d.Library.Scan = &ScanJob{Status: "running", StartedAt: &started}
	p, srv := newPortal(t, d)
	code, out, errs := run(t, "scan", "--url", srv.URL)
	if code != exitcode.OK || p.called("POST "+scanPath) != 0 ||
		!strings.Contains(out, "a scan is running, started 2 min ago — new titles appear as it finds them — nothing more started") {
		t.Fatalf("a scan under way: want 0 and no second one, got %d %v\n%s\n%s", code, p.calls, out, errs)
	}
	started = clock.Add(-time.Hour)
	p.doc.Library.Scan.StartedAt = &started
	if code, _, _ := run(t, "scan", "--url", srv.URL); code != exitcode.OK || p.called("POST "+scanPath) != 1 {
		t.Fatalf("a scan that stopped: want another, got %d %v", code, p.calls)
	}
}

// The catalog manager scans, through the portal: without one there is nothing
// to scan with (3, without trying); one that refuses the admin is 5, one that
// does not answer 4.
func TestScanWhereTheCatalogDoesNotTakeIt(t *testing.T) {
	none := freshBox()
	none.Library = Library{Step: Step{State: stateUnknown, Note: catalogAnswers["none"].body}, Path: "/var/lib/katalog/media"}
	p, srv := newPortal(t, none)
	if code, _, errs := run(t, "scan", "--url", srv.URL); code != exitcode.NotOffered || p.called("POST "+scanPath) != 0 ||
		!strings.Contains(errs, "has no catalog manager to scan with") {
		t.Errorf("no catalog manager: want 3 without trying, got %d %q", code, errs)
	}
	for catalog, want := range map[string]int{"none": exitcode.NotOffered, "refused": exitcode.Forbidden, "silent": exitcode.Undetermined, "failed": exitcode.Failed} {
		p, srv := newPortal(t, freshBox())
		p.catalog = catalog
		if code, _, errs := run(t, "scan", "--url", srv.URL); code != want || !strings.Contains(errs, "start a scan") {
			t.Errorf("%s: want %d, got %d %q", catalog, want, code, errs)
		}
	}
}

// Every step done: done marks it so, with nothing to ask. Without a terminal
// it needs --yes all the same — whether it would ask depends on the
// checklist, and a script should not have to.
func TestDoneMarksSetupDone(t *testing.T) {
	p, srv := newPortal(t, setUp())
	code, _, errs := run(t, "done", "--url", srv.URL)
	if code != exitcode.Usage || !strings.Contains(errs, "stdin is not a terminal") || len(p.calls) != 0 {
		t.Fatalf("no terminal, no --yes: want 2 before any call, got %d %q %v", code, errs, p.calls)
	}
	code, out, errs := runWith(t, "", true, "done", "--url", srv.URL)
	if code != exitcode.OK || p.called("POST "+completePath) != 1 || strings.Contains(out, "[y/N]") {
		t.Fatalf("every step done: want 0, one write and no question, got %d %v\n%s\n%s", code, p.calls, out, errs)
	}
	if !strings.Contains(out, "marked setup done on "+srv.URL+", by admin, just now (2026-10-04 08:00 UTC) — the launchpad no longer shows the checklist; zae setup reopen --url "+srv.URL+" shows it again") {
		t.Errorf("done says who and when, and how to undo it:\n%s", out)
	}

	// Done already: the record stays the first one, and nothing is written.
	code, out, _ = run(t, "done", "--url", srv.URL, "--yes")
	if code != exitcode.OK || p.called("POST "+completePath) != 1 || !strings.Contains(out, "is marked done already, by admin, just now") {
		t.Fatalf("done already: want 0 and no second write, got %d %v\n%s", code, p.calls, out)
	}
}

// Steps still open: done says which, and asks — a no writes nothing.
func TestDoneAsksWhileStepsAreOpen(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	code, out, errs := runWith(t, "n\n", true, "done", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(out, "nothing changed") || p.called("POST "+completePath) != 0 {
		t.Fatalf("declined: want 1 and no write, got %d %v\n%s\n%s", code, p.calls, out, errs)
	}
	for _, s := range []string{
		srv.URL + " — first-run setup · 1 of 4 steps done",
		"  metadata     to do — no TMDB key",
		"  library      to do — no titles yet",
		"  devices      to do — zaentrum.localhost answers on this machine only",
		"marking setup done takes the checklist off the launchpad; zae setup reopen shows it again",
		"mark setup done on " + srv.URL + " with 3 steps open? [y/N]",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("done lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "  processing ") {
		t.Errorf("a pipeline that is off is fine off, and not open:\n%s", out)
	}
	if code, _, _ := runWith(t, "y\n", true, "done", "--url", srv.URL); code != exitcode.OK || p.called("POST "+completePath) != 1 {
		t.Fatalf("confirmed: want 0 and one write, got %d %v", code, p.calls)
	}
}

// reopen takes the record away, so the checklist shows again — and says when
// it was marked; open already, it writes nothing.
func TestReopenShowsTheChecklistAgain(t *testing.T) {
	d := setUp()
	d.Completed = &Completion{At: clock.Add(-3 * time.Hour), By: "ada"}
	p, srv := newPortal(t, d)
	code, out, errs := run(t, "reopen", "--url", srv.URL)
	if code != exitcode.OK || p.called("DELETE "+completePath) != 1 {
		t.Fatalf("want 0 and one write, got %d %v\n%s\n%s", code, p.calls, out, errs)
	}
	if !strings.Contains(out, "reopened setup on "+srv.URL+" — it was marked done by ada, 3 h ago (2026-10-04 05:00 UTC); the launchpad shows the checklist to admins again") {
		t.Errorf("reopen says what it undid:\n%s", out)
	}
	code, out, _ = run(t, "reopen", "--url", srv.URL)
	if code != exitcode.OK || p.called("DELETE "+completePath) != 1 || !strings.Contains(out, "is open already") {
		t.Fatalf("open already: want 0 and no write, got %d %v\n%s", code, p.calls, out)
	}
}

// Against a portal-api without the checklist, each of them is not offered —
// and none of them writes anything first.
func TestWritesOnAPortalWithoutTheChecklist(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	p.old = true
	for _, args := range [][]string{
		{"scan", "--url", srv.URL},
		{"done", "--url", srv.URL, "--yes"},
		{"reopen", "--url", srv.URL},
	} {
		if code, _, errs := run(t, args...); code != exitcode.NotOffered || !strings.Contains(errs, "predates the setup checklist") {
			t.Errorf("%v: want 3, got %d %q", args, code, errs)
		}
	}
	if w := p.writes(); len(w) != 0 {
		t.Fatalf("nothing is written to a portal without the checklist: %v", w)
	}
}

func TestWritesUsage(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	usageCases(t, p, map[string][]string{
		"scan with an argument":   {"scan", "now", "--url", srv.URL},
		"scan without a url":      {"scan"},
		"scan with --yes":         {"scan", "--url", srv.URL, "--yes"},
		"done with an argument":   {"done", "anyway", "--url", srv.URL, "--yes"},
		"reopen with an argument": {"reopen", "again", "--url", srv.URL},
	})
}
