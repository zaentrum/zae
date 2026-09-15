package addon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// The portal's admin API for addons installed from a chart, and the list of
// every installed addon (chart addons and addons installed from an address).
const (
	chartsPath = "/api/portal/addon-charts"
	addonsPath = "/api/portal/addons"
)

// Phases of a ZaentrumAddon, as the operator reports them.
const (
	PhasePending    = "Pending"
	PhasePlanned    = "Planned"
	PhasePlanFailed = "PlanFailed"
	PhaseInstalling = "Installing"
	PhaseReady      = "Ready"
	PhaseDegraded   = "Degraded"
	PhaseFailed     = "Failed"
)

// adminNeed is what a refused call needs, for the forbidden hint.
const adminNeed = "managing addons needs the platform's admin role"

// Addon is GET /api/portal/addon-charts/{name}: one chart addon, its operator
// status and the inputs it was given — secret inputs by path only, never by
// value.
type Addon struct {
	Name             string         `json:"name"`
	Suspended        bool           `json:"suspended"`
	Phase            string         `json:"phase"`
	Message          string         `json:"message"`
	Plan             *Plan          `json:"plan"`
	Components       []Component    `json:"components"`
	LastAppliedChart *Chart         `json:"lastAppliedChart"`
	Values           map[string]any `json:"values"`
	SecretKeys       []string       `json:"secretKeys"`

	// Not part of the v1 document. A portal that sends them gives zae the one
	// exact way to tell a status written for the current spec from an older
	// one, so they are used when present.
	Generation         int64 `json:"generation"`
	ObservedGeneration int64 `json:"observedGeneration"`
}

// installed: the operator has applied a chart for this addon at least once.
func (a *Addon) installed() bool {
	return a.LastAppliedChart != nil && (a.LastAppliedChart.Ref != "" || a.LastAppliedChart.Version != "")
}

// current: the status was written for the current spec, as far as the
// document can say.
func (a *Addon) current() bool {
	return a.Generation == 0 || a.ObservedGeneration >= a.Generation
}

// Chart names a chart: what the resource asks for, or what actually runs.
type Chart struct {
	Ref     string `json:"ref"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

// Plan is what the operator would apply for the current spec.
type Plan struct {
	Chart        PlanChart  `json:"chart"`
	ValuesSchema schemaDoc  `json:"valuesSchema"`
	ValuesErrors []string   `json:"valuesErrors"`
	Violations   []string   `json:"violations"`
	Objects      []Object   `json:"objects"`
	Workloads    []Workload `json:"workloads"`
	Changes      Changes    `json:"changes"`
}

type PlanChart struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	AppVersion  string            `json:"appVersion"`
	Description string            `json:"description"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations"`
}

type Object struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Workload is a rendered pod-running object. Ports are kept raw: a number, a
// "8080/TCP" string or a container port object all render.
type Workload struct {
	Kind   string            `json:"kind"`
	Name   string            `json:"name"`
	Images []string          `json:"images"`
	Ports  []json.RawMessage `json:"ports"`
}

// Changes compares the plan with what was last applied.
type Changes struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Images  []string `json:"images"`
}

func (c Changes) empty() bool { return len(c.Added)+len(c.Removed)+len(c.Images) == 0 }

// Component is one applied workload with its live readiness.
type Component struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Ready   int    `json:"ready"`
	Desired int    `json:"desired"`
	Reason  string `json:"reason"`
}

// schemaDoc holds values.schema.json. The contract sends it as a string; an
// object is accepted too, so a portal that inlines it still renders.
type schemaDoc string

func (s *schemaDoc) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = schemaDoc(str)
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*s = ""
		return nil
	}
	*s = schemaDoc(b)
	return nil
}

// Listed is one row of GET /api/portal/addons: every installed addon, with
// chart information merged in for chart addons.
type Listed struct {
	Key        string          `json:"key"`
	Name       string          `json:"name"`
	ProxyURL   string          `json:"proxyUrl"`
	Version    string          `json:"version"`
	Chart      *Chart          `json:"chart"`
	Phase      string          `json:"phase"`
	Suspended  bool            `json:"suspended"`
	Components []listComponent `json:"components"`
}

// listComponent tolerates both shapes the list carries: pointers that are null
// for a workload that is not deployed, and plain numbers.
type listComponent struct {
	Name    string `json:"name"`
	Ready   *int   `json:"ready"`
	Desired *int   `json:"desired"`
}

// apiError is a call that did not succeed, already mapped onto the exit-code
// contract, with the message to print.
type apiError struct {
	code   int
	msg    string
	status int // the HTTP status, 0 when there was no answer
	// transient: no answer, or the portal failing (5xx) — worth asking again
	// while waiting, never a verdict about the addon.
	transient bool
	// notFound: the instance answered 404; noAPI: because it has no such
	// route at all, not because the addon does not exist.
	notFound, noAPI bool
}

func (e *apiError) Error() string { return e.msg }

// client talks to one instance's portal with the caller's bearer.
type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	return &client{base: base, http: &http.Client{
		Timeout: 30 * time.Second,
		// A redirect from an admin API is a sign-in page or a wrong address,
		// never the answer. Following it would turn a DELETE into a GET of a
		// login page that reads as success.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// do makes one call. in is sent as JSON when not nil; a 2xx body is decoded
// into out when out is not nil. missing is the message for a 404 on a route
// that exists; a 404 from a portal that has no such route says so instead.
func (c *client) do(what, method, path string, in, out any, missing string) *apiError {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return &apiError{code: exitcode.Usage, msg: fmt.Sprintf("usage: %s: cannot encode the request: %v", what, err)}
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return &apiError{code: exitcode.Usage, msg: fmt.Sprintf("usage: %s: %v", what, err)}
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	instance.Authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return &apiError{code: exitcode.Undetermined, transient: true,
			msg: fmt.Sprintf("undetermined: %s: cannot reach %s (%v) — not concluding anything about the addon", what, c.base, err)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				return &apiError{code: exitcode.Undetermined, status: s,
					msg: fmt.Sprintf("undetermined: %s: %s answered %d with something that is not the addon API's JSON (%v) — does the address reach portal-api?", what, c.base, s, err)}
			}
		}
		return nil
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return &apiError{code: exitcode.Forbidden, status: s,
			msg: fmt.Sprintf("forbidden: %s: %s answered %d — %s", what, c.base, s, instance.ForbiddenHint(adminNeed))}
	case s == http.StatusNotFound:
		// A router's own 404 means the route is absent: a portal-api that
		// predates it. Anything else is the API saying "no such addon".
		if strings.TrimSpace(string(raw)) == "404 page not found" {
			route, _, _ := strings.Cut(path, "?")
			if strings.HasPrefix(route, chartsPath) {
				route = chartsPath
			}
			return &apiError{code: exitcode.NotOffered, status: s, notFound: true, noAPI: true,
				msg: fmt.Sprintf("not offered: %s does not serve %s — its portal-api predates it", c.base, route)}
		}
		if missing == "" {
			missing = fmt.Sprintf("%s: %s answered 404: %s", what, c.base, excerpt(raw))
		}
		return &apiError{code: exitcode.NotOffered, status: s, notFound: true, msg: "not offered: " + missing}
	case s >= 300 && s < 400:
		return &apiError{code: exitcode.Failed, status: s,
			msg: fmt.Sprintf("failed: %s: %s answered %d, redirecting to %s — use the instance's final address as --url; a sign-in page means the bearer in %s is missing", what, c.base, s, resp.Header.Get("Location"), instance.TokenEnv)}
	default:
		return &apiError{code: exitcode.Failed, status: s, transient: s >= 500,
			msg: fmt.Sprintf("failed: %s: HTTP %d from %s: %s", what, s, c.base, excerpt(raw))}
	}
}

// get reads one chart addon.
func (c *client) get(name string) (*Addon, *apiError) {
	var a Addon
	if err := c.do("status of "+name, http.MethodGet, chartsPath+"/"+url.PathEscape(name), nil, &a,
		fmt.Sprintf("%s has no addon %q installed from a chart", c.base, name)); err != nil {
		return nil, err
	}
	if a.Name == "" {
		a.Name = name
	}
	return &a, nil
}

// excerpt is the instance's own message, trimmed for one line. portal-api
// answers errors as plain text; a JSON {"error"|"message"} body is unwrapped.
func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	var j struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &j) == nil {
		if j.Error != "" {
			s = j.Error
		} else if j.Message != "" {
			s = j.Message
		}
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}
