package platform

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
)

// at is the clock the status lines are read against.
var at = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func stamp(d time.Duration) string { return at.Add(-d).Format(time.RFC3339) }

// withClock pins the clock for one test.
func withClock(t *testing.T) {
	t.Helper()
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
}

// passed is a run that went through: what the operator records after an
// update, with the checks the doctor reported.
func passed() *Verification {
	return &Verification{Enabled: ptr(true), Result: VerifyPassed, Trigger: "update", Version: "1.5.0",
		Fingerprint: "3f9a1c0b2d4e", StartedAt: stamp(4 * time.Minute), FinishedAt: stamp(3 * time.Minute),
		Job: "zaentrum-verify-7f3c", Passed: 14,
		Checks: []VerificationCheck{{Name: "tls", Status: "ok", Detail: "certificate valid"}}}
}

// The "verified" line, for every state a run can be in — and for a platform
// that never ran one, does not run them, or cannot say.
func TestStatusShowsTheLastVerification(t *testing.T) {
	withClock(t)
	cases := map[string]struct {
		v    *Verification
		raw  string
		want []string
	}{
		"never run": {raw: `{"enabled":true,"result":null}`, want: []string{"verified     never\n"}},
		"passed":    {v: passed(), want: []string{"verified     passed 14/14 · 3 min ago · after the update to 1.5.0 (image set 3f9a1c0b2d4e)\n"}},
		"passed, with warnings and skips": {v: func() *Verification {
			v := passed()
			v.Passed, v.Warned, v.Skipped = 11, 1, 2
			return v
		}(), want: []string{"verified     passed 11/14, 1 warning, 2 skipped · 3 min ago"}},
		"failed": {v: func() *Verification {
			v := passed()
			v.Result, v.Passed, v.Failed = VerifyFailed, 12, 2
			v.Checks = []VerificationCheck{
				{Name: "tls", Status: "ok", Detail: "certificate valid"},
				{Name: "chino-api: items", Status: "fail", Detail: "/api/v1/items answered HTTP 502 · fix: the catalog is down"},
				{Name: "app /chino/", Status: "fail", Detail: "the page loads /chino/assets/index.js, which answers HTTP 404"},
			}
			return v
		}(), want: []string{
			"verified     FAILED 2 of 14 checks · 3 min ago · after the update to 1.5.0 (image set 3f9a1c0b2d4e)\n",
			"\n               ✗ chino-api: items — /api/v1/items answered HTTP 502 · fix: the catalog is down\n",
			"\n               ✗ app /chino/ — the page loads /chino/assets/index.js, which answers HTTP 404\n",
		}},
		"running": {v: &Verification{Enabled: ptr(true), Result: VerifyRunning, Trigger: "update", Version: "1.5.0",
			StartedAt: stamp(2 * time.Minute), Job: "zaentrum-verify-7f3c"},
			want: []string{"verified     running (job zaentrum-verify-7f3c) · started 2 min ago · after the update to 1.5.0\n"}},
		"an error": {v: &Verification{Enabled: ptr(true), Result: VerifyError, Trigger: "update", Version: "1.5.0",
			StartedAt: stamp(2 * time.Minute), FinishedAt: stamp(time.Minute), Message: "the verification job could not pull ghcr.io/zaentrum/zae"},
			want: []string{"verified     error — the verification job could not pull ghcr.io/zaentrum/zae · 1 min ago · after the update to 1.5.0\n"}},
		"skipped": {v: &Verification{Enabled: ptr(true), Result: VerifySkipped, Trigger: "request", Version: "1.5.0",
			FinishedAt: stamp(10 * time.Second), Message: "no test account: the realm is external"},
			want: []string{"verified     skipped — no test account: the realm is external · just now · asked for, at 1.5.0\n"}},
		"switched off": {raw: `{"enabled":false,"result":null}`,
			want: []string{"verified     off — the platform does not verify itself (spec.verification.enabled is false)\n"}},
		"a request waits, never run": {raw: `{"enabled":true,"result":null,"pendingRequest":"r1"}`,
			want: []string{"verified     never — one is asked for and waits to run\n"}},
		"a request waits after a run": {v: func() *Verification { v := passed(); v.PendingRequest = "r2"; return v }(),
			want: []string{"after the update to 1.5.0 (image set 3f9a1c0b2d4e) · another run is asked for and waits\n"}},
		"hours ago":   {v: func() *Verification { v := passed(); v.FinishedAt = stamp(5 * time.Hour); return v }(), want: []string{"· 5 h ago ·"}},
		"days ago":    {v: func() *Verification { v := passed(); v.FinishedAt = stamp(72 * time.Hour); return v }(), want: []string{"· 3 days ago ·"}},
		"unreadable":  {raw: `{"enabled":true,"result":null,"note":"status.verification is not readable"}`, want: []string{"verified     unreadable — status.verification is not readable\n"}},
		"not there":   {want: []string{"verified     not reported — this portal-api predates the platform's verification of itself\n"}},
		"a new state": {raw: `{"enabled":true,"result":"Queued"}`, want: []string{"verified     Queued\n"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p, srv := newPortal(t)
			p.op.Verification, p.verificationRaw = c.v, c.raw
			code, out, errs := run(t, "", false, "status", "--url", srv.URL)
			if code != exitcode.OK {
				t.Fatalf("a status is a status: %d\n%s\n%s", code, out, errs)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("status lacks %q:\n%s", w, out)
				}
			}
			// The line sits with the platform's own state, before the workloads.
			if i, j := strings.Index(out, "  verified "), strings.Index(out, "NAME"); i < 0 || i > j {
				t.Errorf("the verified line belongs to the platform's section:\n%s", out)
			}
		})
	}
}

// --json is the portal's own document, the verification in it untouched.
func TestStatusJSONPassesTheVerificationThrough(t *testing.T) {
	p, srv := newPortal(t)
	raw := `{"enabled":true,"result":"Failed","trigger":"update","failed":1,"checks":[{"name":"tls","status":"fail","detail":"expired"}],"condition":{"status":"False","reason":"ChecksFailed"}}`
	p.verificationRaw = raw
	code, out, _ := run(t, "", false, "status", "--url", srv.URL, "--json")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d", code)
	}
	var doc struct {
		Operator struct {
			Verification json.RawMessage `json:"verification"`
		} `json:"operator"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	var got, want any
	_ = json.Unmarshal(doc.Operator.Verification, &got)
	_ = json.Unmarshal([]byte(raw), &want)
	if g, w := mustJSON(got), mustJSON(want); g != w {
		t.Fatalf("the sub-document changed on the way:\n got %s\nwant %s", g, w)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
