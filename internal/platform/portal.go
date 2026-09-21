package platform

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

// The portal's admin API for the operator console: one document with the
// platform's desired and observed state, and the writes that change it.
const (
	operatorPath = "/api/portal/operator"
	applyPath    = operatorPath + "/apply-update"
)

// instancePath addresses one workload's action. The name is escaped because
// it reaches a URL path; the portal validates it again.
func instancePath(name, action string) string {
	return operatorPath + "/instances/" + url.PathEscape(name) + "/" + action
}

// adminNeed is what a refused call needs, for the forbidden hint.
const adminNeed = "driving the platform needs the platform's admin role"

// Phases a workload reports. The portal derives them from the Deployment's
// counters AND from what its pods say: a workload whose pods are failing is
// never "ready", however the arithmetic comes out.
const (
	PhaseReady       = "ready"
	PhaseProgressing = "progressing"
	PhaseDegraded    = "degraded"
	PhaseStopped     = "stopped"
)

// Groups, in the order a status prints them: what the operator renders, what
// was installed next to it, and what neither claims — which is worth seeing
// last and precisely, because nothing owns it.
const (
	GroupPlatform = "platform"
	GroupAddon    = "addon"
	GroupOther    = "other"
)

// Console is GET /api/portal/operator: whether this instance can manage its
// own workloads at all, the operator's view of the platform, and every
// workload running beside it.
type Console struct {
	Available bool       `json:"available"`
	Operator  Operator   `json:"operator"`
	Instances []Workload `json:"instances"`
	// Error: the portal could read the operator but not the workloads. The
	// console is offered; this one list is missing.
	Error string `json:"error"`
}

// Operator is the platform as the operator's resource declares and reports it:
// Version, Channel and UpdateMode are what it was asked for, Phase,
// CurrentVersion and AvailableUpdate what it found.
type Operator struct {
	Present         bool        `json:"present"`
	Name            string      `json:"name"`
	Channel         string      `json:"channel"`
	Version         string      `json:"version"`
	UpdateMode      string      `json:"updateMode"`
	Hostname        string      `json:"hostname"`
	Phase           string      `json:"phase"`
	CurrentVersion  string      `json:"currentVersion"`
	AvailableUpdate string      `json:"availableUpdate"`
	Components      []Component `json:"components"`
	// Generation and ObservedGeneration are the spec generation of the
	// operator's resource and the one its status was written for. A wait uses
	// them to follow the write it made rather than whatever is Ready. Both are
	// 0 against a portal that predates them.
	Generation         int64 `json:"generation"`
	ObservedGeneration int64 `json:"observedGeneration"`
	// Note says why there is nothing to report, when Present is false.
	Note string `json:"note"`
}

// Component is one workload as the operator's own status lists it.
type Component struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	Image string `json:"image"`
}

// Workload is one running Deployment as the cluster has it.
type Workload struct {
	Name              string `json:"name"`
	Image             string `json:"image"`
	DesiredReplicas   int    `json:"desiredReplicas"`
	ReadyReplicas     int    `json:"readyReplicas"`
	UpdatedReplicas   int    `json:"updatedReplicas"`
	AvailableReplicas int    `json:"availableReplicas"`
	Restarts          int    `json:"restarts"`
	Phase             string `json:"phase"`
	// Protected: the platform refuses to scale or restart it from here. The
	// refusal, and its wording, belong to the portal — zae never decides it.
	Protected bool `json:"protected"`
	// OperatorManaged: the operator owns it, so a platform update rolls it.
	OperatorManaged bool   `json:"operatorManaged"`
	Group           string `json:"group"`
	// Reason is why it is not healthy, in the cluster's own words.
	Reason     string `json:"reason"`
	AlwaysPull bool   `json:"alwaysPull"`
	Addon      string `json:"addon,omitempty"`
	Component  string `json:"component,omitempty"`
	// Generation, ObservedGeneration and RestartedAt are what make a wait
	// exact. The replica counters cannot do it: for the first seconds of a
	// rollout they describe the pods from BEFORE the write — every field true,
	// the conclusion false — which is how a restart once reported ready eight
	// seconds after it was asked for. All three are 0/"" against a portal that
	// predates them, and zae then says its wait is a readiness gate only.
	Generation         int64  `json:"generation"`
	ObservedGeneration int64  `json:"observedGeneration"`
	RestartedAt        string `json:"restartedAt"`
}

// write answers POST …/instances/{name}/{scale,restart}: the generation the
// write produced, and the rollout stamp a restart wrote. An older portal
// answers 204 and nothing, which leaves both zero.
type write struct {
	Name        string `json:"name"`
	Generation  int64  `json:"generation"`
	RestartedAt string `json:"restartedAt"`
}

// updated answers PATCH /operator and POST /operator/apply-update: the version
// the platform now asks for and the generation that write made.
type updated struct {
	Version    string `json:"version"`
	Generation int64  `json:"generation"`
}

// offered reports whether this instance has an operator console to drive.
// Both halves must hold: the portal must be able to reach a cluster at all,
// and that cluster must hold the resource the platform is declared in.
func (c *Console) offered() bool { return c.Available && c.Operator.Present }

// noConsole says why there is nothing to drive, in the portal's own words.
func (c *Console) noConsole(base string) string {
	note := strings.TrimSpace(c.Operator.Note)
	if note == "" {
		note = "the portal gave no reason"
	}
	if !c.Available {
		return fmt.Sprintf("%s does not manage its own workloads: %s", base, note)
	}
	return fmt.Sprintf("%s is not managed by the operator: %s — `zae platform` drives the operator's resource, and this instance has none", base, note)
}

// find returns the workload by name, or nil.
func (c *Console) find(name string) *Workload {
	for i := range c.Instances {
		if c.Instances[i].Name == name {
			return &c.Instances[i]
		}
	}
	return nil
}

// managed counts the workloads a platform update rolls.
func (c *Console) managed() []Workload {
	var out []Workload
	for _, w := range c.Instances {
		if w.OperatorManaged {
			out = append(out, w)
		}
	}
	return out
}

// settled reports whether a workload has reached what was asked of it: the
// cluster has acted on the spec it now has, every replica it asks for is a new
// one, and every one of those is ready.
//
// A workload scaled to zero is settled at zero: stopped is a state an admin
// chose, not one to wait out. Ready and available are compared with >= rather
// than == because a rollout that surges runs old and new pods together for a
// moment, and a wait must not flap on that; updated is compared exactly,
// since it never exceeds what was asked for.
func (w *Workload) settled() bool {
	if !w.observed(0) {
		return false
	}
	if w.DesiredReplicas == 0 {
		return w.Phase == PhaseStopped || w.ReadyReplicas == 0
	}
	return w.Phase == PhaseReady &&
		w.UpdatedReplicas == w.DesiredReplicas &&
		w.ReadyReplicas >= w.DesiredReplicas &&
		w.AvailableReplicas >= w.DesiredReplicas
}

// observed reports whether the cluster has acted on generation gen, and on the
// spec this workload now has. A portal that reports no generations says 0, and
// the test passes — that instance's waits are readiness gates, which zae says
// out loud rather than pretending otherwise.
func (w *Workload) observed(gen int64) bool {
	if gen > w.ObservedGeneration {
		return false
	}
	return w.Generation == 0 || w.ObservedGeneration >= w.Generation
}

// reportsRollout: this instance tells a client what a write produced, so a
// wait can follow that rollout instead of whatever happens to be ready.
func (w *Workload) reportsRollout() bool { return w != nil && w.Generation > 0 }

// state is one workload on one line: how many of its replicas are up, its
// phase, and the cluster's reason when it has one.
func (w *Workload) state() string {
	s := fmt.Sprintf("%s %d/%d %s", w.Name, w.ReadyReplicas, w.DesiredReplicas, w.Phase)
	if w.Reason != "" {
		s += ": " + w.Reason
	}
	return s
}

// apiError is a call that did not succeed, already mapped onto the exit-code
// contract, with the message to print.
type apiError struct {
	code   int
	msg    string
	status int // the HTTP status, 0 when there was no answer
	// transient: no answer, or the portal failing for a moment — worth asking
	// again while waiting, never a verdict about the platform.
	transient bool
}

func (e *apiError) Error() string { return e.msg }

// asAPIError unwraps err into an *apiError, if it is one.
func asAPIError(err error) (*apiError, bool) {
	var ae *apiError
	ok := errors.As(err, &ae)
	return ae, ok
}

// client talks to one instance's portal with the caller's bearer.
type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	return &client{base: base, http: &http.Client{
		Timeout: 30 * time.Second,
		// A redirect from an admin API is a sign-in page or a wrong address,
		// never the answer. Following it would turn a POST into a GET of a
		// login page that reads as success.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// do makes one call. in is sent as JSON when not nil; a 2xx body is decoded
// into out when out is not nil. The error, when there is one, is an *apiError.
func (c *client) do(ctx context.Context, what, method, path string, in, out any) error {
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
		// an authentication failure, not a verdict about the platform.
		return &apiError{code: exitcode.Forbidden, msg: fmt.Sprintf("forbidden: %s: %v", what, err)}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &apiError{code: exitcode.Undetermined, transient: true,
			msg: fmt.Sprintf("undetermined: %s: cannot reach %s (%v) — not concluding anything about the platform", what, c.base, err)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		if out != nil && len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return &apiError{code: exitcode.Undetermined, status: s,
					msg: fmt.Sprintf("undetermined: %s: %s answered %d with something that is not the operator console's JSON (%v) — does the address reach portal-api?", what, c.base, s, err)}
			}
		}
		return nil
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return &apiError{code: exitcode.Forbidden, status: s,
			msg: fmt.Sprintf("forbidden: %s: %s answered %d — %s", what, c.base, s, instance.ForbiddenHint(c.base, adminNeed))}
	case s == http.StatusNotFound:
		// A router's own 404 means the route is absent: a portal-api that
		// predates the operator console. Anything else is the API's own answer.
		if strings.TrimSpace(string(raw)) == "404 page not found" {
			return &apiError{code: exitcode.NotOffered, status: s,
				msg: fmt.Sprintf("not offered: %s does not serve %s — its portal-api predates the operator console", c.base, operatorPath)}
		}
		return &apiError{code: exitcode.NotOffered, status: s,
			msg: fmt.Sprintf("not offered: %s: %s answered 404: %s", what, c.base, excerpt(raw))}
	case s == http.StatusConflict:
		// The platform refused because what zae asked for is no longer what it
		// read: the update on the shelf changed while the question was being
		// answered. Looking again is the whole fix, so say that.
		return &apiError{code: exitcode.Failed, status: s,
			msg: fmt.Sprintf("failed: %s: %s — look again with: zae platform status --url %s", what, excerpt(raw), c.base)}
	case s == http.StatusServiceUnavailable:
		// The portal says this in exactly one case: it is not running where it
		// could manage anything. That is definitive, and no wait changes it.
		// Any other 503 is somebody's gateway, which says nothing either way.
		if strings.HasPrefix(strings.TrimSpace(string(raw)), noManagement) {
			return &apiError{code: exitcode.NotOffered, status: s,
				msg: fmt.Sprintf("not offered: %s does not manage its own workloads: %s", c.base, excerpt(raw))}
		}
		return &apiError{code: exitcode.Undetermined, status: s, transient: true,
			msg: fmt.Sprintf("undetermined: %s: %s answered 503: %s", what, c.base, excerpt(raw))}
	case s >= 300 && s < 400:
		return &apiError{code: exitcode.Failed, status: s,
			msg: fmt.Sprintf("failed: %s: %s answered %d, redirecting to %s — use the instance's final address as --url; a sign-in page means the bearer in %s is missing", what, c.base, s, resp.Header.Get("Location"), instance.TokenEnv)}
	default:
		return &apiError{code: exitcode.Failed, status: s, transient: s >= 500,
			msg: fmt.Sprintf("failed: %s: HTTP %d from %s: %s", what, s, c.base, excerpt(raw))}
	}
}

// noManagement opens the portal's own note for an instance that cannot manage
// workloads at all.
const noManagement = "instance management is unavailable"

// console reads the whole console in one call, and keeps the bytes: --json
// prints what the portal said, not what zae made of it.
func (c *client) console(ctx context.Context) (*Console, json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.do(ctx, "read the platform", http.MethodGet, operatorPath, nil, &raw); err != nil {
		return nil, nil, err
	}
	var cons Console
	if err := json.Unmarshal(raw, &cons); err != nil {
		return nil, raw, &apiError{code: exitcode.Undetermined,
			msg: fmt.Sprintf("undetermined: read the platform: %s answered with JSON that is not the operator console's (%v)", c.base, err)}
	}
	return &cons, raw, nil
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
