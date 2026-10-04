package debug

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// The portal's debug API: every route a GET, every one admin-only.
const (
	podsPath     = "/api/portal/debug/pods"
	logsPath     = "/api/portal/debug/logs"
	topologyPath = "/api/portal/debug/kafka/topology"
	eventsPath   = "/api/portal/debug/kafka/events"
	bundlePath   = "/api/portal/debug/support-bundle"
)

// adminNeed is what a refused call needs, for the forbidden hint.
const adminNeed = "the debug console needs the platform's admin role"

// outsideCluster opens the portal's own answer for an instance that runs
// where it has no pods to read: the one 503 that is definitive.
const outsideCluster = "log viewer is unavailable"

// routerNotFound is the router's own 404: no such route, which is a
// portal-api older than it — not the API answering about what was asked.
const routerNotFound = "404 page not found"

// apiError is a call that did not succeed, already mapped onto the exit-code
// contract, with the message to print.
type apiError struct {
	code   int
	msg    string
	status int // the HTTP status, 0 when there was no answer
	// said is the portal's own words.
	said string
	// transient: no answer, or the portal failing for a moment — worth asking
	// again while following, never a verdict.
	transient bool
	// gone: a log read the portal answered 404 — the pod went after zae
	// listed it. noContainer: one it answered 400 because the pod does not
	// run that container (it was replaced under the same name). Both are
	// definitive about that one read, and neither ends a follow, whose next
	// listing says what runs instead.
	gone, noContainer bool
}

func (e *apiError) Error() string { return e.msg }

func asAPIError(err error) (*apiError, bool) {
	var ae *apiError
	ok := errors.As(err, &ae)
	return ae, ok
}

// client reads one instance's debug API with the caller's bearer.
type client struct {
	base string
	http *http.Client
}

// newClient bounds each call by timeout: a log read is quick, while the
// portal takes up to a minute to assemble a support bundle.
func newClient(base string, timeout time.Duration) *client {
	return &client{base: base, http: &http.Client{
		Timeout: timeout,
		// A redirect from an admin API is a sign-in page or a wrong address,
		// never the answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// get makes one GET and returns at most limit bytes of a 2xx body. The error,
// when there is one, is an *apiError.
func (c *client) get(ctx context.Context, what, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, &apiError{code: exitcode.Usage, msg: fmt.Sprintf("usage: %s: %v", what, err)}
	}
	req.Header.Set("Accept", "application/json, text/plain")
	if err := instance.Authorize(ctx, req, c.base); err != nil {
		// Credentials exist for this instance and could not be made usable.
		return nil, &apiError{code: exitcode.Forbidden, msg: fmt.Sprintf("forbidden: %s: %v", what, err)}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &apiError{code: exitcode.Undetermined, transient: true,
			msg: fmt.Sprintf("undetermined: %s: cannot reach %s (%v) — not concluding anything", what, c.base, err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &apiError{code: exitcode.Undetermined, transient: true,
			msg: fmt.Sprintf("undetermined: %s: the answer from %s broke off (%v)", what, c.base, err)}
	}

	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		if int64(len(raw)) > limit {
			return nil, &apiError{code: exitcode.Failed, status: s,
				msg: fmt.Sprintf("failed: %s: %s answered more than %d MiB", what, c.base, limit>>20)}
		}
		return raw, nil
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return nil, &apiError{said: excerpt(raw), code: exitcode.Forbidden, status: s,
			msg: fmt.Sprintf("forbidden: %s: %s answered %d — %s", what, c.base, s, instance.ForbiddenHint(c.base, adminNeed))}
	case s == http.StatusNotFound:
		route, _, _ := strings.Cut(path, "?")
		if strings.TrimSpace(string(raw)) == routerNotFound {
			// The router's own 404: a portal-api that predates the route.
			return nil, &apiError{said: excerpt(raw), code: exitcode.NotOffered, status: s,
				msg: fmt.Sprintf("not offered: %s does not serve %s — its portal-api predates it", c.base, route)}
		}
		return nil, &apiError{said: excerpt(raw), code: exitcode.NotOffered, status: s,
			msg: fmt.Sprintf("not offered: %s: %s answered 404: %s", what, c.base, excerpt(raw))}
	case s == http.StatusServiceUnavailable:
		if strings.HasPrefix(strings.TrimSpace(string(raw)), outsideCluster) {
			return nil, &apiError{said: excerpt(raw), code: exitcode.NotOffered, status: s,
				msg: fmt.Sprintf("not offered: %s has no pods to read: %s", c.base, excerpt(raw))}
		}
		return nil, &apiError{said: excerpt(raw), code: exitcode.Undetermined, status: s, transient: true,
			msg: fmt.Sprintf("undetermined: %s: %s answered 503: %s", what, c.base, excerpt(raw))}
	case s >= 300 && s < 400:
		return nil, &apiError{said: excerpt(raw), code: exitcode.Failed, status: s,
			msg: fmt.Sprintf("failed: %s: %s answered %d, redirecting to %s — use the instance's final address as --url; a sign-in page means the bearer in %s is missing", what, c.base, s, resp.Header.Get("Location"), instance.TokenEnv)}
	default:
		return nil, &apiError{said: excerpt(raw), code: exitcode.Failed, status: s, transient: s >= 500,
			msg: fmt.Sprintf("failed: %s: HTTP %d from %s: %s", what, s, c.base, excerpt(raw))}
	}
}

// jsonMessage is the message of a JSON {"error"|"message"} body, "" for any
// other body.
func jsonMessage(b []byte) string {
	var j struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &j) != nil {
		return ""
	}
	if j.Error != "" {
		return j.Error
	}
	return j.Message
}

// excerpt is the instance's own message, trimmed for one line. portal-api
// answers errors as plain text; a JSON {"error"|"message"} body is unwrapped.
func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if j := jsonMessage(b); j != "" {
		s = j
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return printable(s)
}
