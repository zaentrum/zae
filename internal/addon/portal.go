package addon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Phases of a ZaentrumAddon, as the operator reports them. An addon the
// operator has not reconciled yet has no phase at all.
const (
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
	Name string `json:"name"`
	// Chart is what the resource asks for; LastAppliedChart is what runs.
	Chart            *Chart          `json:"chart"`
	Suspended        bool            `json:"suspended"`
	Phase            string          `json:"phase"`
	Message          string          `json:"message"`
	Plan             *Plan           `json:"plan"`
	Components       []Component     `json:"components"`
	LastAppliedChart *Chart          `json:"lastAppliedChart"`
	Values           json.RawMessage `json:"values"`
	SecretKeys       []string        `json:"secretKeys"`
	// Generation is the resource's spec generation; ObservedGeneration the one
	// the operator's status was written for. Together they tell a plan for the
	// current spec from an older one.
	Generation         int64  `json:"generation"`
	ObservedGeneration int64  `json:"observedGeneration"`
	Registered         bool   `json:"registered"`
	RegistrationError  string `json:"registrationError"`
}

// installed: the operator has applied a chart for this addon at least once.
func (a *Addon) installed() bool {
	return a.LastAppliedChart != nil && (a.LastAppliedChart.Ref != "" || a.LastAppliedChart.Version != "")
}

// spec is the chart the resource asks for, or the running one when a portal
// does not say.
func (a *Addon) spec() *Chart {
	if a.Chart != nil && a.Chart.Ref != "" {
		return a.Chart
	}
	return a.LastAppliedChart
}

// valuesMap decodes the non-secret values; numbers stay exact.
func (a *Addon) valuesMap() map[string]any {
	var m map[string]any
	if len(bytes.TrimSpace(a.Values)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(a.Values))
	dec.UseNumber()
	_ = dec.Decode(&m)
	return m
}

// Chart names a chart: what the resource asks for, or what actually runs.
type Chart struct {
	Ref     string `json:"ref"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

// Plan is what the operator would apply for the spec it observed.
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

// schemaDoc holds values.schema.json. The API sends it as a string; an object
// is accepted too, so a portal that inlines it still renders.
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

// accepted answers every write: the generation the write produced — the one
// a plan must be written for — and the chart the resource now asks for.
type accepted struct {
	Name               string `json:"name"`
	Generation         int64  `json:"generation"`
	ObservedGeneration int64  `json:"observedGeneration"`
	Chart              *Chart `json:"chart"`
}

// removal answers DELETE /api/portal/addon-charts/{name}.
type removal struct {
	Name       string   `json:"name"`
	Resource   bool     `json:"resource"`
	KeptValues bool     `json:"keptValues"`
	Warnings   []string `json:"warnings"`
}

// Listed is one row of GET /api/portal/addons: every installed addon, with
// chart information merged in for chart addons.
type Listed struct {
	Key               string          `json:"key"`
	Name              string          `json:"name"`
	ProxyURL          string          `json:"proxyUrl"`
	Version           string          `json:"version"`
	Chart             *listChart      `json:"chart"`
	Phase             string          `json:"phase"`
	Suspended         bool            `json:"suspended"`
	Registered        bool            `json:"registered"`
	RegistrationError string          `json:"registrationError"`
	Components        []listComponent `json:"components"`
}

// listChart is a list row's chart: what the resource asks for, and what runs.
type listChart struct {
	Ref         string `json:"ref"`
	Version     string `json:"version"`
	Digest      string `json:"digest"`
	LastApplied *Chart `json:"lastApplied"`
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
	// transient: no answer, or the portal failing for a moment — worth asking
	// again while waiting, never a verdict about the addon.
	transient bool
	// notFound: the answer was 404. noAPI: the instance cannot manage chart
	// addons at all — a portal-api without the API, or a cluster without the
	// ZaentrumAddon resource — which no amount of waiting changes.
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
// into out when out is not nil. missing is the message for a 404 about the
// named addon. The error, when there is one, is an *apiError.
func (c *client) do(ctx context.Context, what, method, path string, in, out any, missing string) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return &apiError{code: exitcode.Usage, msg: fmt.Sprintf("usage: %s: cannot encode the request: %v", what, err)}
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return &apiError{code: exitcode.Usage, msg: fmt.Sprintf("usage: %s: %v", what, err)}
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := instance.Authorize(ctx, req, c.base); err != nil {
		// Credentials exist for this instance and could not be made usable —
		// an authentication failure, not a verdict about the addon.
		return &apiError{code: exitcode.Forbidden, msg: fmt.Sprintf("forbidden: %s: %v", what, err)}
	}

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
			msg: fmt.Sprintf("forbidden: %s: %s answered %d — %s", what, c.base, s, instance.ForbiddenHint(c.base, adminNeed))}
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
	case s == http.StatusServiceUnavailable:
		return c.unavailable(ctx, what, raw)
	case s >= 300 && s < 400:
		return &apiError{code: exitcode.Failed, status: s,
			msg: fmt.Sprintf("failed: %s: %s answered %d, redirecting to %s — use the instance's final address as --url; a sign-in page means the bearer in %s is missing", what, c.base, s, resp.Header.Get("Location"), instance.TokenEnv)}
	default:
		return &apiError{code: exitcode.Failed, status: s, transient: s >= 500,
			msg: fmt.Sprintf("failed: %s: HTTP %d from %s: %s", what, s, c.base, excerpt(raw))}
	}
}

// clusterSilent opens the note portal-api gives when the cluster did not
// answer it — the one unavailability that passes by itself.
const clusterSilent = "the cluster did not answer"

// unavailable classifies a 503. portal-api answers 503 both when the instance
// cannot manage chart addons at all (outside a cluster, no ZaentrumAddon
// resource, a Role without it) and when the cluster did not answer for a
// moment. GET /api/portal/addon-charts says which: available, and if not, why.
// "Cannot" is exit 3 and ends any wait; "not now" is asked again.
func (c *client) unavailable(ctx context.Context, what string, raw []byte) error {
	available, note, answered := c.probe(ctx)
	switch {
	case answered && !available && !strings.HasPrefix(note, clusterSilent):
		return &apiError{code: exitcode.NotOffered, status: http.StatusServiceUnavailable, noAPI: true,
			msg: fmt.Sprintf("not offered: %s cannot install addons from charts: %s", c.base, note)}
	case answered && available:
		// The API is there, and this call was refused by the cluster anyway —
		// portal-api lacks a permission the call needs. Waiting does not help.
		return &apiError{code: exitcode.Failed, status: http.StatusServiceUnavailable,
			msg: fmt.Sprintf("failed: %s: %s answered 503: %s", what, c.base, excerpt(raw))}
	default:
		return &apiError{code: exitcode.Undetermined, status: http.StatusServiceUnavailable, transient: true,
			msg: fmt.Sprintf("undetermined: %s: %s answered 503: %s", what, c.base, excerpt(raw))}
	}
}

// probe asks GET /api/portal/addon-charts whether chart addons can be managed
// here. answered is false for anything but a readable 200 — the probe never
// classifies its own failure, so a 503 in front of it cannot loop.
func (c *client) probe(ctx context.Context) (available bool, note string, answered bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+chartsPath, nil)
	if err != nil {
		return false, "", false
	}
	req.Header.Set("Accept", "application/json")
	// A probe classifies someone else's failure; it never classifies its own.
	// An unusable credential here just means the probe asks anonymously and
	// reports "could not tell", which is the answer that costs nothing.
	_ = instance.Authorize(ctx, req, c.base)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", false
	}
	defer resp.Body.Close()
	var body struct {
		Available bool   `json:"available"`
		Note      string `json:"note"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) != nil {
		return false, "", false
	}
	return body.Available, body.Note, true
}

// get reads one chart addon.
func (c *client) get(ctx context.Context, name string) (*Addon, error) {
	var a Addon
	if err := c.do(ctx, "status of "+name, http.MethodGet, namePath(name), nil, &a,
		fmt.Sprintf("%s has no addon %q installed from a chart", c.base, name)); err != nil {
		return nil, err
	}
	if a.Name == "" {
		a.Name = name
	}
	return &a, nil
}

func namePath(name string) string { return chartsPath + "/" + url.PathEscape(name) }

// asAPIError unwraps err into an *apiError, if it is one.
func asAPIError(err error) (*apiError, bool) {
	var ae *apiError
	ok := errors.As(err, &ae)
	return ae, ok
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
