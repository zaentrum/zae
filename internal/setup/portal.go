package setup

import (
	"bytes"
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

// The portal's setup API, and the operator console's write the pipeline is
// switched with.
const (
	setupPath    = "/api/portal/setup"
	completePath = setupPath + "/complete"
	metadataPath = setupPath + "/metadata"
	scanPath     = setupPath + "/library/scan"
	operatorPath = "/api/portal/operator"
)

// adminNeed is what a refused call needs, for the forbidden hint.
const adminNeed = "the setup checklist needs the platform's admin role"

// What the portal says, word for word, when something it reads through is
// not there — the openings zae tells its answers apart by.
const (
	// routerNotFound is the router's own 404: a portal-api older than the
	// route, not the API answering about what was asked.
	routerNotFound = "404 page not found"
	// noCatalog: portal-api is pointed at no catalog manager, so neither the
	// key nor a scan has anywhere to go.
	noCatalog = "portal-api is pointed at no catalog manager"
	// catalogSilent and catalogRefused open a 502: the portal took the write,
	// and the catalog manager did not answer it, or refused the admin.
	catalogSilent  = "the catalog manager did not answer"
	catalogRefused = "the catalog manager refused this admin"
	// noManagement: the portal runs where it manages no workloads at all.
	noManagement = "instance management is unavailable"
)

// Step states, as the portal reads them.
const (
	stateDone     = "done"     // nothing left to do
	stateTodo     = "todo"     // an admin has something to do
	stateWorking  = "working"  // under way: a scan running, workers starting
	stateOptional = "optional" // off, and fine off
	stateUnknown  = "unknown"  // the portal cannot tell; its note says why
	stateInfo     = "info"     // nothing to check from here
)

// Doc is GET /api/portal/setup: every step of the checklist, read live, and
// the record that setup was marked done.
type Doc struct {
	// Completed is null while setup is open: the launchpad shows the
	// checklist to admins until it is marked done.
	Completed  *Completion `json:"completed"`
	Metadata   Metadata    `json:"metadata"`
	Library    Library     `json:"library"`
	Processing Processing  `json:"processing"`
	Devices    Devices     `json:"devices"`
	People     Step        `json:"people"`
}

// Completion is who marked setup done, and when.
type Completion struct {
	At time.Time `json:"at"`
	By string    `json:"by"`
}

// Step is what every step says: its state, and why when it is unknown.
type Step struct {
	State string `json:"state"`
	Note  string `json:"note"`
}

// Metadata is the TMDB key in effect — where it comes from, never the key:
// setting (set through the portal or the catalog's console), environment
// (the catalog manager's own), or none.
type Metadata struct {
	Step
	Key       string     `json:"key"`
	UpdatedAt *time.Time `json:"updatedAt"`
}

// Library is what the catalog holds and where files go: Path as the catalog
// reads it, Volume the claim that holds it and Folder the folder inside —
// Volume is "" when the portal cannot tell. Titles is a floor when
// TitlesMore is set.
type Library struct {
	Step
	Titles     int      `json:"titles"`
	TitlesMore bool     `json:"titlesMore"`
	Path       string   `json:"path"`
	Volume     string   `json:"volume"`
	Folder     string   `json:"folder"`
	Appliance  bool     `json:"appliance"`
	Scan       *ScanJob `json:"scan"`
}

// ScanJob is the catalog's latest scan.
type ScanJob struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"` // running | done | failed
	StartedAt     *time.Time `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt"`
	Error         string     `json:"error"`
	FilesSeen     int        `json:"filesSeen"`
	ItemsInserted int        `json:"itemsInserted"`
	ItemsUpdated  int        `json:"itemsUpdated"`
}

// Processing is the media pipeline: what the operator's resource asks for —
// null without one to read — whether a node offers the GPU its transcoder
// needs, and its workers as they run. Switchable: there is a resource to
// switch it on.
type Processing struct {
	Step
	Pipeline   *bool    `json:"pipeline"`
	GPU        *bool    `json:"gpu"`
	GPUNodes   *int     `json:"gpuNodes"`
	GPUNote    string   `json:"gpuNote"`
	Workers    []Worker `json:"workers"`
	Switchable bool     `json:"switchable"`
}

// Worker is one of the pipeline's workloads. Phase is the operator console's,
// or "absent" while the operator has not rolled it out yet.
type Worker struct {
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	Reason  string `json:"reason"`
	Ready   int    `json:"ready"`
	Desired int    `json:"desired"`
}

// Devices is how the platform is reached: phones and TVs sign in over https
// only, on a name they reach.
type Devices struct {
	Step
	Origin      string `json:"origin"`
	HTTPS       bool   `json:"https"`
	Issuer      string `json:"issuer"`
	IssuerHTTPS bool   `json:"issuerHttps"`
	LocalOnly   bool   `json:"localOnly"`
	Source      string `json:"source"`
}

// apiError is a call that did not succeed, already mapped onto the exit-code
// contract, with the message to print.
type apiError struct {
	code   int
	msg    string
	status int // the HTTP status, 0 when there was no answer
	// said is the portal's own words, for a caller that words its own message.
	said string
}

func (e *apiError) Error() string { return e.msg }

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
		// The checklist reads the catalog, the operator's resource and the
		// cluster side by side, each under its own timeout in the portal.
		Timeout: 60 * time.Second,
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
		return &apiError{code: exitcode.Undetermined,
			msg: fmt.Sprintf("undetermined: %s: cannot reach %s (%v) — not concluding anything about the platform", what, c.base, err)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	said := excerpt(raw)

	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		if out != nil && len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return &apiError{code: exitcode.Undetermined, status: s, said: said,
					msg: fmt.Sprintf("undetermined: %s: %s answered %d with something that is not the setup API's JSON (%v) — does the address reach portal-api?", what, c.base, s, err)}
			}
		}
		return nil
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return &apiError{code: exitcode.Forbidden, status: s, said: said,
			msg: fmt.Sprintf("forbidden: %s: %s answered %d — %s", what, c.base, s, instance.ForbiddenHint(c.base, adminNeed))}
	case s == http.StatusNotFound && strings.TrimSpace(string(raw)) == routerNotFound:
		route, _, _ := strings.Cut(path, "?")
		return &apiError{code: exitcode.NotOffered, status: s, said: said,
			msg: fmt.Sprintf("not offered: %s does not serve %s — its portal-api predates the setup checklist", c.base, route)}
	case s == http.StatusNotFound:
		return &apiError{code: exitcode.NotOffered, status: s, said: said,
			msg: fmt.Sprintf("not offered: %s: %s answered 404: %s", what, c.base, said)}
	case s == http.StatusBadGateway && strings.HasPrefix(said, catalogSilent):
		// The portal took the write and could not hand it on, or not hear
		// back: whether it happened is what nobody can say.
		return &apiError{code: exitcode.Undetermined, status: s, said: said,
			msg: fmt.Sprintf("undetermined: %s: %s — zae cannot tell whether it happened; zae setup --url %s shows the checklist as it is", what, said, c.base)}
	case s == http.StatusBadGateway && strings.HasPrefix(said, catalogRefused):
		return &apiError{code: exitcode.Forbidden, status: s, said: said,
			msg: fmt.Sprintf("forbidden: %s: %s — the account needs the catalog manager's admin role too", what, said)}
	case s == http.StatusBadGateway:
		return &apiError{code: exitcode.Failed, status: s, said: said,
			msg: fmt.Sprintf("failed: %s: %s", what, said)}
	case s == http.StatusServiceUnavailable && strings.HasPrefix(said, noCatalog):
		return &apiError{code: exitcode.NotOffered, status: s, said: said,
			msg: fmt.Sprintf("not offered: %s has no catalog manager to %s: %s", c.base, what, said)}
	case s == http.StatusServiceUnavailable && strings.HasPrefix(said, noManagement):
		return &apiError{code: exitcode.NotOffered, status: s, said: said,
			msg: fmt.Sprintf("not offered: %s does not manage its own workloads: %s", c.base, said)}
	case s == http.StatusServiceUnavailable:
		return &apiError{code: exitcode.Undetermined, status: s, said: said,
			msg: fmt.Sprintf("undetermined: %s: %s answered 503: %s", what, c.base, said)}
	case s >= 300 && s < 400:
		return &apiError{code: exitcode.Failed, status: s, said: said,
			msg: fmt.Sprintf("failed: %s: %s answered %d, redirecting to %s — use the instance's final address as --url; a sign-in page means the bearer in %s is missing", what, c.base, s, resp.Header.Get("Location"), instance.TokenEnv)}
	default:
		return &apiError{code: exitcode.Failed, status: s, said: said,
			msg: fmt.Sprintf("failed: %s: HTTP %d from %s: %s", what, s, c.base, said)}
	}
}

// read reads the checklist, and keeps the bytes: --json prints what the portal
// said, not what zae made of it.
func read(ctx context.Context, c *client) (*Doc, json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.do(ctx, "read the setup checklist", http.MethodGet, setupPath, nil, &raw); err != nil {
		return nil, nil, err
	}
	var d Doc
	if err := json.Unmarshal(raw, &d); err != nil || d.Metadata.State+d.Library.State+d.Processing.State+d.Devices.State == "" {
		return nil, raw, &apiError{code: exitcode.Undetermined,
			msg: fmt.Sprintf("undetermined: read the setup checklist: %s answered with JSON that is not the setup checklist — does the address reach portal-api?", c.base)}
	}
	return &d, raw, nil
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
	return printable(s)
}

// printable keeps text from the platform — a step's note, the catalog
// manager's words, a path — on one line and out of the terminal's control: an
// escape sequence in it would be executed by the terminal rather than shown.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}
