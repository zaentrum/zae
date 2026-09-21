// Package oidc is exactly as much of OAuth 2.0 as a terminal needs: the
// Device Authorization Grant (RFC 8628) with PKCE (RFC 7636), a refresh, and
// a look at an access token's own claims so `zae whoami` can say who you are.
//
// Three rules shape it:
//
//   - Endpoints are READ, never built. An issuer may live under a path prefix
//     (…/auth/realms/zaentrum), and its device endpoint is wherever it says —
//     so zae fetches /.well-known/openid-configuration and uses what is in it.
//     String concatenation would work on one deployment and lie on the next.
//   - PKCE always. A public client with no secret proves possession with a
//     fresh verifier per login; an identity provider that requires it refuses
//     a plain device request, and one that does not is unharmed by receiving
//     the challenge anyway.
//   - Nothing here prints. Device codes, access tokens and refresh tokens are
//     credentials; the package that stores them decides where they go, and
//     the ones this package holds are unexported so they cannot be formatted
//     into a message by accident.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// wellKnown is appended to the issuer, as OpenID Connect Discovery prescribes:
// an issuer with a path keeps it (…/realms/zaentrum/.well-known/…).
const wellKnown = "/.well-known/openid-configuration"

// DeviceGrant is the grant type a device-code token request carries.
const DeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// Sentinels. Callers map these onto zae's exit-code contract, so they are
// errors.Is-able rather than strings to match.
var (
	// ErrNoDeviceGrant: the issuer answered, and advertises no device
	// authorization endpoint. An operator must turn it on; no retry helps.
	ErrNoDeviceGrant = errors.New("the issuer advertises no device authorization endpoint")
	// ErrUnreachable: no usable answer from the issuer (connect, TLS, timeout,
	// an HTML error page). zae cannot tell whether login would work.
	ErrUnreachable = errors.New("identity provider unreachable")
	// ErrDenied: a person said no, or the provider refused this client.
	ErrDenied = errors.New("authorization denied")
	// ErrCodeExpired: the user code was not used in time.
	ErrCodeExpired = errors.New("the code expired before it was used")
)

// Error is what an OAuth endpoint answered (RFC 6749 §5.2): the
// machine-readable code, plus the description — which is where a provider
// says what an operator actually has to fix.
type Error struct {
	Code        string
	Description string
	Status      int
}

func (e *Error) Error() string {
	switch {
	case e.Code != "" && e.Description != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	case e.Code != "":
		return e.Code
	case e.Description != "":
		return e.Description
	default:
		return fmt.Sprintf("HTTP %d", e.Status)
	}
}

// Unwrap maps the codes that mean something specific onto the sentinels.
func (e *Error) Unwrap() error {
	switch e.Code {
	case "access_denied":
		return ErrDenied
	case "expired_token":
		return ErrCodeExpired
	}
	return nil
}

// Endpoints is the part of an issuer's metadata zae uses.
type Endpoints struct {
	Issuer string
	Device string
	Token  string
}

// Client talks to one issuer. The zero value works; Sleep is a seam for tests
// so a poll that waits five seconds in production waits none in a test.
type Client struct {
	HTTP *http.Client
	// Sleep paces polling. It must return ctx.Err() when the session ends, so
	// Ctrl-C stops a poll where it stands instead of at the next interval.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Discover reads the issuer's metadata. The device endpoint being absent is
// its own error: it is the one failure an operator can fix, and saying
// "unreachable" for it would send them looking at the network instead.
func (c *Client) Discover(ctx context.Context, issuer string) (*Endpoints, error) {
	issuer = strings.TrimRight(issuer, "/")
	if issuer == "" {
		return nil, fmt.Errorf("%w: no issuer to ask", ErrUnreachable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+wellKnown, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s%s answered %d", ErrUnreachable, issuer, wellKnown, resp.StatusCode)
	}
	var meta struct {
		Issuer string `json:"issuer"`
		Device string `json:"device_authorization_endpoint"`
		Token  string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return nil, fmt.Errorf("%w: %s%s did not answer OpenID metadata (%v)", ErrUnreachable, issuer, wellKnown, err)
	}
	if meta.Token == "" {
		return nil, fmt.Errorf("%w: %s%s advertises no token_endpoint", ErrUnreachable, issuer, wellKnown)
	}
	if meta.Device == "" {
		return nil, fmt.Errorf("%w: %s", ErrNoDeviceGrant, issuer)
	}
	if meta.Issuer == "" {
		meta.Issuer = issuer
	}
	return &Endpoints{Issuer: meta.Issuer, Device: meta.Device, Token: meta.Token}, nil
}

// Device is one authorization in flight. The device code and the PKCE
// verifier are unexported on purpose: they are credentials for the length of
// this login, and nothing outside this package needs to hold them.
type Device struct {
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
	Interval                int

	deviceCode string
	verifier   string
}

// Open is where to send the person: the complete URI when the provider offers
// one (it carries the code, so nothing has to be typed), else the plain one.
func (d *Device) Open() string {
	if d.VerificationURIComplete != "" {
		return d.VerificationURIComplete
	}
	return d.VerificationURI
}

// Authorize starts a device authorization. scope may be empty.
func (c *Client) Authorize(ctx context.Context, ep *Endpoints, clientID, scope string) (*Device, error) {
	verifier, err := newVerifier()
	if err != nil {
		return nil, fmt.Errorf("cannot generate a PKCE verifier: %w", err)
	}
	form := url.Values{
		"client_id": {clientID},
		// PKCE on the device request: an identity provider whose client
		// requires S256 refuses a request without these two, and one that does
		// not require it ignores them.
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	if scope != "" {
		form.Set("scope", scope)
	}
	var out struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := c.post(ctx, ep.Device, form, &out); err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" {
		return nil, fmt.Errorf("%w: %s answered without a device code to poll for", ErrUnreachable, ep.Device)
	}
	d := &Device{
		UserCode:                out.UserCode,
		VerificationURI:         out.VerificationURI,
		VerificationURIComplete: out.VerificationURIComplete,
		ExpiresIn:               out.ExpiresIn,
		Interval:                out.Interval,
		deviceCode:              out.DeviceCode,
		verifier:                verifier,
	}
	if d.Interval <= 0 {
		d.Interval = 5 // RFC 8628 §3.2: absent means 5 seconds
	}
	if d.ExpiresIn <= 0 {
		d.ExpiresIn = 600
	}
	return d, nil
}

// Token is what the provider issued. Expiry is absolute, computed when the
// answer arrived, so it survives being written to a file and read back.
type Token struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Scope        string
	Expiry       time.Time
}

// tokenResponse is the wire shape of a successful token answer.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
}

func (t tokenResponse) token() *Token {
	out := &Token{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		TokenType:    t.TokenType,
		Scope:        t.Scope,
	}
	if t.ExpiresIn > 0 {
		out.Expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	return out
}

// Poll asks the token endpoint until the person has approved, refused, or let
// the code expire — honouring the interval the provider set, the slow_down it
// may ask for, and the context, so Ctrl-C ends the wait at once.
func (c *Client) Poll(ctx context.Context, ep *Endpoints, clientID string, d *Device) (*Token, error) {
	interval := time.Duration(d.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(d.ExpiresIn) * time.Second)
	form := url.Values{
		"grant_type":  {DeviceGrant},
		"client_id":   {clientID},
		"device_code": {d.deviceCode},
		// The verifier the challenge was derived from: a provider enforcing
		// PKCE rejects the exchange without it.
		"code_verifier": {d.verifier},
	}
	for {
		if err := c.sleep(ctx, interval); err != nil {
			return nil, err
		}
		var out tokenResponse
		err := c.post(ctx, ep.Token, form, &out)
		switch {
		case err == nil:
			if out.AccessToken == "" {
				return nil, fmt.Errorf("%w: %s answered without an access token", ErrUnreachable, ep.Token)
			}
			return out.token(), nil
		case isCode(err, "authorization_pending"):
			// Nobody has approved yet. Normal, and by far the common answer.
		case isCode(err, "slow_down"):
			// RFC 8628 §3.5: add five seconds and keep the new interval.
			interval += 5 * time.Second
		default:
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w after %ds", ErrCodeExpired, d.ExpiresIn)
		}
	}
}

// Refresh exchanges a refresh token for a new access token.
func (c *Client) Refresh(ctx context.Context, ep *Endpoints, clientID, refreshToken string) (*Token, error) {
	var out tokenResponse
	err := c.post(ctx, ep.Token, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("%w: %s answered without an access token", ErrUnreachable, ep.Token)
	}
	t := out.token()
	if t.RefreshToken == "" {
		// A provider that does not rotate refresh tokens omits it; the one we
		// have stays valid, so the caller keeps writing it back.
		t.RefreshToken = refreshToken
	}
	return t, nil
}

// post sends a form and decodes the answer, turning an OAuth error body into
// an *Error and anything unreadable into ErrUnreachable.
func (c *Client) post(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%w: %s answered %d with something that is not JSON", ErrUnreachable, endpoint, resp.StatusCode)
		}
		return nil
	}
	var oe struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &oe) == nil && (oe.Error != "" || oe.Description != "") {
		return &Error{Code: oe.Error, Description: oe.Description, Status: resp.StatusCode}
	}
	// Not an OAuth error body at all: a proxy, a login page, a gateway. That is
	// "cannot tell", not "the provider refused you".
	return fmt.Errorf("%w: %s answered %d", ErrUnreachable, endpoint, resp.StatusCode)
}

// isCode reports whether err is an OAuth error with this code.
func isCode(err error, code string) bool {
	var oe *Error
	return errors.As(err, &oe) && oe.Code == code
}

// IsMissingPKCE reports whether err is the provider complaining that the
// request carried no PKCE challenge — the shape an identity provider answers
// when its client requires S256 and the CLI is too old to send one. Naming it
// saves an operator from debugging their realm instead of their zae.
func IsMissingPKCE(err error) bool {
	var oe *Error
	if !errors.As(err, &oe) {
		return false
	}
	d := strings.ToLower(oe.Description)
	return strings.Contains(d, "code_challenge")
}

// newVerifier is a fresh PKCE code verifier: 32 random bytes as 43 base64url
// characters, inside RFC 7636's 43–128. Fresh per login, never reused.
func newVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// challenge is S256: base64url(SHA-256(verifier)), unpadded.
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Claims is the little zae reads out of an access token to tell a person who
// they are signed in as.
//
// zae does NOT verify the signature, and must not be read as if it did: the
// instance validates every token on every call against the issuer's keys.
// This is the token describing itself, for display — which is why `whoami`
// says what the token claims and the API remains the thing that decides.
type Claims struct {
	Subject  string
	Username string
	Roles    []string
	Expiry   time.Time
}

// HasRole reports whether the token carries the named realm role.
func (c *Claims) HasRole(role string) bool {
	if c == nil || role == "" {
		return false
	}
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// ParseClaims decodes a JWT's payload. ok is false for anything that is not a
// readable JWT — an opaque token is a legitimate thing for a provider to
// issue, so that is an answer ("cannot say"), not an error.
func ParseClaims(token string) (*Claims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var raw struct {
		Sub               string `json:"sub"`
		PreferredUsername string `json:"preferred_username"`
		Exp               int64  `json:"exp"`
		RealmAccess       struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, false
	}
	c := &Claims{Subject: raw.Sub, Username: raw.PreferredUsername, Roles: raw.RealmAccess.Roles}
	if raw.Exp > 0 {
		c.Expiry = time.Unix(raw.Exp, 0)
	}
	return c, true
}
