package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/instance"
)

// mePath is the portal's answer to "who is this bearer": the one any signed-in
// caller may ask.
const mePath = "/api/portal/me"

// me is GET /api/portal/me. Which role makes an admin, and on whose tokens,
// is the instance's decision — its admin role is configurable, and a portal
// may take the role only on tokens issued to its own clients — so whoami
// asks rather than reading a role name out of the token.
//
// IsAdmin is a pointer: a portal-api that answers without it is older than
// the answer, and the token is read instead. AdminRole and Client are newer
// still, and may be empty.
//
// Subject and ExpiresAt are newest: the token's sub as the instance verified
// it — the one name an opaque token has — and when the instance stops taking
// the bearer. ExpiresAt is kept raw, because its three shapes mean three
// things: absent (a portal-api older than it), null (no token stands behind
// the caller: the instance's authentication is switched off), or a time.
type me struct {
	Username  string          `json:"username"`
	Roles     []string        `json:"roles"`
	IsAdmin   *bool           `json:"isAdmin"`
	AdminRole string          `json:"adminRole"`
	Client    string          `json:"client"`
	Subject   string          `json:"subject"`
	ExpiresAt json.RawMessage `json:"expiresAt"`
}

// expiry is when the instance stops taking the bearer, as it answered. said
// is false when it did not say — an older portal-api, or a value zae cannot
// read — and the token's own expiry is all there is. A zero time that was said
// is a caller no token stands behind.
func (m *me) expiry() (at time.Time, said bool) {
	switch raw := strings.TrimSpace(string(m.ExpiresAt)); raw {
	case "":
		return time.Time{}, false
	case "null":
		return time.Time{}, true
	}
	var s string
	if json.Unmarshal(m.ExpiresAt, &s) != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// noToken reports whether the instance said that no token stands behind this
// caller, so no expiry applies.
func (m *me) noToken() bool {
	at, said := m.expiry()
	return said && at.IsZero()
}

// meOutcome is how asking went.
type meOutcome int

const (
	meAnswered    meOutcome = iota
	meOlder                 // the portal-api predates the answer: read the token
	meRefused               // the instance does not accept this bearer (401/403)
	meUnreachable           // no answer zae can read
	meNoSession             // the stored session could not be renewed
)

// askMe asks the instance who a bearer is: token, or — when token is empty —
// the one every command sends (ZAE_TOKEN, else the stored session, renewed).
// said explains an outcome other than meAnswered, for a message.
func askMe(ctx context.Context, base, token string) (m *me, how meOutcome, said string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+mePath, nil)
	if err != nil {
		return nil, meUnreachable, err.Error()
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if err := instance.Authorize(ctx, req, base); err != nil {
		return nil, meNoSession, err.Error()
	}
	client := &http.Client{Timeout: 15 * time.Second,
		// A redirect is a sign-in page or a wrong address, never the answer.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, meUnreachable, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch s := resp.StatusCode; {
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return nil, meRefused, fmt.Sprintf("%s answered %d", base, s)
	case s == http.StatusNotFound || s == http.StatusMethodNotAllowed:
		return nil, meOlder, ""
	case s >= 200 && s < 300:
		var answer me
		if json.Unmarshal(body, &answer) != nil || answer.IsAdmin == nil {
			return nil, meOlder, ""
		}
		return &answer, meAnswered, ""
	default:
		return nil, meUnreachable, fmt.Sprintf("%s answered %d", base, s)
	}
}

// adminLine is the instance's answer about the admin role, worded for what it
// means: yes, or no — and when the token carries the role all the same, why
// that does not count here.
func (m *me) adminLine() string {
	role := ""
	if m.AdminRole != "" {
		role = fmt.Sprintf(" (%q)", m.AdminRole)
	}
	switch {
	case *m.IsAdmin:
		return "yes — the instance grants this bearer its admin role" + role
	case m.AdminRole != "" && slices.Contains(m.Roles, m.AdminRole) && m.Client != "":
		return fmt.Sprintf("no — the token carries %q, but was issued to the client %q, and the instance takes the role only on tokens of its own clients; admin commands will be refused (exit 5) — sign in with zae login",
			m.AdminRole, m.Client)
	default:
		return "no — the instance does not grant this bearer its admin role" + role + ", so admin commands will be refused (exit 5)"
	}
}
