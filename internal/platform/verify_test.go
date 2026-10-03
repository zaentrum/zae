package platform

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
)

// verify is a write, and asks first — never on a stdin nobody can answer.
func TestVerifyAsksFirst(t *testing.T) {
	withClock(t)
	p, srv := newPortal(t)
	p.op.Verification = passed()

	code, _, errs := run(t, "y\n", false, "verify", "--url", srv.URL)
	if code != exitcode.Usage || !strings.Contains(errs, "stdin is not a terminal") || len(p.calls) != 0 {
		t.Fatalf("no terminal, no --yes: want 2 and nothing sent, got %d %q %v", code, errs, p.calls)
	}

	code, out, _ := run(t, "n\n", true, "verify", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(out, "nothing asked") || p.verifies != 0 {
		t.Fatalf("declined: want 1 and nothing asked, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "verified     passed 14/14") || !strings.Contains(out, "ask the operator to verify "+srv.URL+" now? [y/N]") {
		t.Errorf("the last run is shown, then the question names the instance:\n%s", out)
	}

	code, out, errs = run(t, "", false, "verify", "--url", srv.URL, "--yes")
	if code != exitcode.OK || p.verifies != 1 {
		t.Fatalf("--yes: want 0 and one request, got %d %d\n%s\n%s", code, p.verifies, out, errs)
	}
	if !strings.Contains(out, "request req-1") || !strings.Contains(out, "zae platform status --url "+srv.URL) {
		t.Errorf("the request is named, and how to follow it:\n%s", out)
	}
	if b := p.bodies["POST "+verifyPath]; len(b) != 1 || b[0] != "" {
		t.Errorf("the request carries no body: %q", b)
	}

	// Asked again while that one waits: the same request, and zae says so.
	code, out, _ = run(t, "", false, "verify", "--url", srv.URL, "--yes")
	if code != exitcode.OK || !strings.Contains(out, "request req-1, which was already waiting") {
		t.Errorf("a waiting request is joined, not doubled:\n%s", out)
	}
}

// --wait follows THIS request's run: picked up, running, done — and prints
// what it checked.
func TestVerifyWaitsForTheRunItAskedFor(t *testing.T) {
	withClock(t)
	p, srv := newPortal(t)
	p.op.Verification = passed()
	asked, mine := 0, ""
	p.onVerify = func(p *fakePortal, token string) { asked, mine = p.reads, token }
	p.onGet = func(p *fakePortal, n int) {
		if asked == 0 {
			return
		}
		switch {
		case n >= asked+4:
			done := passed()
			done.Trigger, done.Request, done.FinishedAt = "request", mine, stamp(0)
			done.Checks = []VerificationCheck{{Name: "tls", Status: "ok", Detail: "certificate valid"},
				{Name: "chino-api: playback", Status: "skip", Detail: "no packaged title to play"}}
			done.Passed, done.Skipped = 13, 1
			p.op.Verification = done
		case n >= asked+2:
			// The operator took the request up: it is this run's now.
			p.op.Verification = &Verification{Enabled: ptr(true), Result: VerifyRunning, Trigger: "request",
				Request: mine, StartedAt: stamp(0), Job: "zaentrum-verify-9a1b"}
		}
	}
	code, out, errs := run(t, "", false, "verify", "--url", srv.URL, "--yes", "--wait", "--timeout", "10s")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{
		"waiting for the operator to verify " + srv.URL,
		"asked for — the operator has not started the run yet",
		"running (job zaentrum-verify-9a1b)",
		"  ✓ tls                          certificate valid",
		"  - chino-api: playback          no packaged title to play",
		"verified: passed 13/14, 1 skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the wait lacks %q:\n%s", want, out)
		}
	}
}

// A run that answers an earlier request can finish while this one waits. It
// is not this request's result, and the wait goes on to the one that is.
func TestVerifyDoesNotTakeAnotherRequestsResult(t *testing.T) {
	withClock(t)
	p, srv := newPortal(t)
	p.op.Verification = &Verification{Enabled: ptr(true), Result: VerifyRunning, Request: "older", Trigger: "request", StartedAt: stamp(time.Minute)}
	asked, mine := 0, ""
	p.onVerify = func(p *fakePortal, token string) { asked, mine = p.reads, token }
	p.onGet = func(p *fakePortal, n int) {
		switch {
		case asked == 0 || n == asked+1:
			// The older run is still going, and this request waits behind it.
		case n == asked+2:
			// The older run ends, and passes; this request still waits.
			older := passed()
			older.Request, older.Trigger, older.PendingRequest = "older", "request", mine
			p.op.Verification = older
		case n >= asked+3:
			ours := passed()
			ours.Request, ours.Trigger, ours.Result, ours.Passed, ours.Failed = mine, "request", VerifyFailed, 13, 1
			ours.Checks = []VerificationCheck{{Name: "chino-api: items", Status: "fail", Detail: "HTTP 502"}}
			p.op.Verification = ours
		}
	}
	code, out, errs := run(t, "", false, "verify", "--url", srv.URL, "--yes", "--wait", "--timeout", "10s")
	if code != exitcode.Failed {
		t.Fatalf("this request's run failed — want 1, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "a run for an earlier request is still going") {
		t.Errorf("the wait says why it waits:\n%s", out)
	}
	if !strings.Contains(out, "  ✗ chino-api: items             HTTP 502") || !strings.Contains(errs, "verification failed 1 of 14 checks") {
		t.Errorf("the failing run's checks and verdict:\n%s\n%s", out, errs)
	}
}

// How a run can end, and what each means to a script.
func TestVerifyExitCodes(t *testing.T) {
	withClock(t)
	for name, c := range map[string]struct {
		result, message string
		code            int
		says            string
	}{
		"passed":            {VerifyPassed, "", exitcode.OK, ""},
		"failed":            {VerifyFailed, "", exitcode.Failed, "verification failed"},
		"could not run":     {VerifyError, "the job was evicted", exitcode.Failed, "could not run: the job was evicted"},
		"skipped, disabled": {VerifySkipped, "verification is disabled", exitcode.NotOffered, "did not verify itself: verification is disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			p, srv := newPortal(t)
			p.op.Verification = &Verification{Enabled: ptr(true)}
			p.onVerify = func(p *fakePortal, token string) {
				v := passed()
				v.Result, v.Request, v.Message, v.Trigger = c.result, token, c.message, "request"
				if c.result == VerifyFailed {
					v.Passed, v.Failed = 13, 1
				}
				p.op.Verification = v
			}
			code, out, errs := run(t, "", false, "verify", "--url", srv.URL, "--yes", "--wait", "--timeout", "5s")
			if code != c.code || !strings.Contains(errs, c.says) {
				t.Fatalf("want %d saying %q, got %d\n%s\n%s", c.code, c.says, code, out, errs)
			}
		})
	}
}

// The ways a request is refused before any run, each with its exit code.
func TestVerifyRefusals(t *testing.T) {
	withClock(t)
	for name, c := range map[string]struct {
		status int
		body   string
		code   int
		says   string
	}{
		"an older portal":         {http.StatusNotFound, "404 page not found", exitcode.NotOffered, "predates the request"},
		"no such method":          {http.StatusMethodNotAllowed, "", exitcode.NotOffered, "predates the request"},
		"switched off":            {http.StatusConflict, "verification is disabled: set spec.verification.enabled", exitcode.NotOffered, "does not verify itself"},
		"a conflict":              {http.StatusConflict, "the resource changed underneath — try again", exitcode.Failed, "ask again"},
		"no operator":             {http.StatusBadRequest, "no operator resource", exitcode.NotOffered, "no operator"},
		"not an administrator":    {http.StatusForbidden, "forbidden", exitcode.Forbidden, "forbidden"},
		"the portal cannot patch": {http.StatusServiceUnavailable, "cannot patch the Zaentrum resource", exitcode.Undetermined, "cannot patch"},
	} {
		t.Run(name, func(t *testing.T) {
			p, srv := newPortal(t)
			p.op.Verification = passed()
			p.verifyStatus, p.verifyBody = c.status, c.body
			code, out, errs := run(t, "", false, "verify", "--url", srv.URL, "--yes", "--wait")
			if code != c.code || !strings.Contains(errs, c.says) {
				t.Fatalf("want %d saying %q, got %d\n%s\n%s", c.code, c.says, code, out, errs)
			}
		})
	}

	// What the console already says is answered without asking: a platform
	// that does not verify itself, and a portal that cannot say.
	for name, c := range map[string]struct {
		v    *Verification
		raw  string
		says string
	}{
		"switched off": {raw: `{"enabled":false,"result":null}`, says: "spec.verification.enabled is false"},
		"not reported": {says: "predates the platform's verification of itself"},
	} {
		t.Run(name+", from the console", func(t *testing.T) {
			p, srv := newPortal(t)
			p.op.Verification, p.verificationRaw = c.v, c.raw
			code, _, errs := run(t, "", false, "verify", "--url", srv.URL, "--yes")
			if code != exitcode.NotOffered || !strings.Contains(errs, c.says) || p.verifies != 0 {
				t.Fatalf("want 3 without a request, got %d (%d sent) %q", code, p.verifies, errs)
			}
		})
	}
}

// A wait that runs out says what it was still waiting for.
func TestVerifyWaitTimesOut(t *testing.T) {
	withClock(t)
	p, srv := newPortal(t)
	p.op.Verification = passed()
	code, _, errs := run(t, "", false, "verify", "--url", srv.URL, "--yes", "--wait", "--timeout", "40ms")
	if code != exitcode.Failed || !strings.Contains(errs, "still waiting for the verification") ||
		!strings.Contains(errs, "has not started the run yet") {
		t.Fatalf("want 1 naming the state, got %d %q", code, errs)
	}
}

func TestVerifyUsage(t *testing.T) {
	p, srv := newPortal(t)
	for name, args := range map[string][]string{
		"a positional": {"verify", "now", "--url", srv.URL, "--yes"},
		"zero timeout": {"verify", "--url", srv.URL, "--yes", "--wait", "--timeout", "0"},
		"no url":       {"verify", "--yes"},
	} {
		if code, _, errs := run(t, "", false, args...); code != exitcode.Usage {
			t.Errorf("%s: want 2, got %d %q", name, code, errs)
		}
	}
	if len(p.calls) != 0 {
		t.Errorf("usage errors reach no instance: %v", p.calls)
	}
	if _, out, _ := run(t, "", false, "help"); !strings.Contains(out, "zae platform verify --url") {
		t.Errorf("the help offers verify:\n%s", out)
	}
}
