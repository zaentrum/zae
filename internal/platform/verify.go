package platform

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// verify asks the operator to verify the platform now, and with --wait
// follows the run that answers the request.
//
// The operator verifies the platform by itself after every rollout; this is
// the same run, on demand: `zae doctor --sign-in` in the platform's own
// namespace, as a test account the operator created, recorded on its
// resource. Nothing here signs in or holds a password — the request is a
// token on the resource, and the run is the cluster's.
func verify(args []string) int {
	fs := flagSet("verify")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	yes := fs.Bool("yes", false, "ask without asking first")
	wait := fs.Bool("wait", false, "follow the run until it has a result, and print its checks")
	timeout := fs.Duration("timeout", defaultTimeout, "how long --wait lasts")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return usageErr("zae platform verify --url https://… takes no arguments")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	if *timeout <= 0 {
		return usageErr("--timeout must be positive")
	}
	if code := requireTTY(*yes, "asking the platform to verify itself"); code != exitcode.OK {
		return code
	}

	ctx := context.Background()
	c := newClient(base)
	cons, code := requireOperator(ctx, c, base, "zae platform verify")
	if cons == nil {
		return code
	}
	v := cons.Operator.Verification
	switch {
	case v == nil:
		errf("not offered: %s cannot be asked to verify itself — its portal-api predates the platform's verification of itself", base)
		return exitcode.NotOffered
	case v.off():
		errf("not offered: %s does not verify itself — spec.verification.enabled is false on its Zaentrum resource", base)
		return exitcode.NotOffered
	}
	fmt.Fprintf(stdout, "%s — the platform\n", base)
	label(stdout, "verified", verificationLine(v, now()))
	if !*yes && !confirm(fmt.Sprintf("ask the operator to verify %s now?", base)) {
		fmt.Fprintln(stdout, "nothing asked")
		return exitcode.Failed
	}

	var asked struct {
		Request string `json:"request"`
	}
	if err := c.do(ctx, "ask for a verification", http.MethodPost, verifyPath, nil, &asked); err != nil {
		return verifyRefused(base, err)
	}
	token := strings.TrimSpace(asked.Request)
	joined := ""
	if token != "" && token == v.PendingRequest {
		// A request was already waiting: the portal answers with that one, so
		// two people asking at once start one run, and both follow it.
		joined = ", which was already waiting"
	}
	if !*wait {
		fmt.Fprintf(stdout, "asked the operator to verify %s (request %s%s) — follow it with zae platform status --url %s\n",
			base, dash(token), joined, base)
		return exitcode.OK
	}
	if token == "" {
		errf("failed: %s took the request and answered no token for it, so zae cannot tell its run from another — follow it with zae platform status --url %s", base, base)
		return exitcode.Failed
	}
	fmt.Fprintf(stdout, "asked (request %s%s)\n", token, joined)
	return followVerification(ctx, c, base, token, *timeout)
}

// verifyRefused maps what a refused request answered. A portal that predates
// the request, a platform that does not verify itself and an instance without
// an operator are each definitive — exit 3; a conflict is worth asking again.
func verifyRefused(base string, err error) int {
	ae, ok := asAPIError(err)
	if !ok {
		return fail(err)
	}
	switch ae.status {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		errf("not offered: %s cannot be asked to verify itself — its portal-api predates the request (POST %s)", base, verifyPath)
		return exitcode.NotOffered
	case http.StatusBadRequest:
		errf("not offered: %s has no operator to verify it: %s", base, ae.said)
		return exitcode.NotOffered
	case http.StatusConflict:
		if strings.Contains(ae.said, "verification.enabled") || strings.Contains(strings.ToLower(ae.said), "disabled") {
			errf("not offered: %s does not verify itself: %s", base, ae.said)
			return exitcode.NotOffered
		}
		errf("failed: the request collided with another change to the platform: %s — ask again", ae.said)
		return exitcode.Failed
	}
	return fail(err)
}

// followVerification waits for the run that answers token, then prints its
// checks. A run that answers an EARLIER request may finish first, and does
// not count: the wait is for this request's result, not the next one.
func followVerification(ctx context.Context, c *client, base, token string, timeout time.Duration) int {
	fmt.Fprintf(stdout, "waiting for the operator to verify %s (timeout %s)\n", base, timeout)
	var got *Verification
	done, state, err := c.poll(ctx, timeout, false, func(cons *Console) (bool, string) {
		v := cons.Operator.Verification
		switch {
		case !cons.offered():
			return false, cons.noConsole("the instance")
		case v == nil:
			return false, "the portal does not report the verification"
		case v.Request == token && v.final():
			got = v
			return true, v.Result
		case v.Request == token:
			return false, verificationLine(v, now())
		case v.PendingRequest == token && v.Result == VerifyRunning:
			return false, "asked for — a run for an earlier request is still going, and this one waits for it"
		case v.PendingRequest == token:
			return false, "asked for — the operator has not started the run yet"
		}
		return false, "waiting for the operator to take up the request"
	})
	switch {
	case err != nil:
		return fail(err)
	case !done:
		errf("failed: still waiting for the verification after %s — %s", timeout, state)
		return exitcode.Failed
	}

	fmt.Fprintln(stdout)
	marks := map[string]string{"ok": "✓", "warn": "!", "fail": "✗", "skip": "-"}
	for _, ch := range got.Checks {
		m := marks[ch.Status]
		if m == "" {
			m = "?"
		}
		fmt.Fprintf(stdout, "  %s %-28s %s\n", m, ch.Name, ch.Detail)
	}
	if len(got.Checks) > 0 {
		fmt.Fprintln(stdout)
	}
	line := verificationLine(got, now())
	switch got.Result {
	case VerifyPassed:
		fmt.Fprintf(stdout, "verified: %s\n", line)
		return exitcode.OK
	case VerifySkipped:
		errf("not offered: %s did not verify itself: %s", base, orNoReason(got.Message))
		return exitcode.NotOffered
	case VerifyError:
		errf("failed: the verification could not run: %s", orNoReason(got.Message))
		return exitcode.Failed
	}
	errf("failed: the platform's verification %s", strings.Replace(line, "FAILED", "failed", 1))
	return exitcode.Failed
}
