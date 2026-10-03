package oidc

// The authorization code flow (RFC 6749 §4.1) with PKCE, a state and a nonce:
// the flow a browser runs when a person signs in to a web client.
//
// zae signs PEOPLE in with the device grant; nothing here is for that. It is
// for `zae doctor --sign-in`, which signs in the way a person does — with the
// web client's own client id, through the login page, back to the web
// client's redirect URI — because the point of verifying a platform is the
// path its users take, not a path only a CLI takes.
//
// The same rules hold as for the device grant: endpoints are read from the
// issuer's metadata, PKCE is always sent, and the values that make a request
// one person's — the state, the nonce, the verifier — are unexported, so they
// cannot be formatted into a message by accident.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrStateMismatch: the redirect answers a different request than the one
	// sent — another login's answer, or something in between rewrote it.
	ErrStateMismatch = errors.New("the redirect carries a different state than the request sent")
	// ErrNoCode: the redirect carries neither a code nor an OAuth error.
	ErrNoCode = errors.New("the redirect carries no authorization code")
)

// AuthCode is one authorization request in flight.
type AuthCode struct {
	ClientID    string
	RedirectURI string
	Scope       string

	state, nonce, verifier string
}

// NewAuthCode starts a request: a fresh state, nonce and PKCE verifier, each
// from crypto/rand and never reused.
func NewAuthCode(clientID, redirectURI, scope string) (*AuthCode, error) {
	var vals [3]string
	for i := range vals {
		v, err := newVerifier()
		if err != nil {
			return nil, fmt.Errorf("cannot generate a random value for the request: %w", err)
		}
		vals[i] = v
	}
	return &AuthCode{ClientID: clientID, RedirectURI: redirectURI, Scope: scope,
		state: vals[0], nonce: vals[1], verifier: vals[2]}, nil
}

// URL is the authorization request: the endpoint the metadata names, with
// what a browser carries there. A query the endpoint already has is kept.
func (a *AuthCode) URL(ep *Endpoints) (string, error) {
	if ep == nil || strings.TrimSpace(ep.Authorization) == "" {
		return "", errors.New("the issuer advertises no authorization_endpoint")
	}
	u, err := url.Parse(ep.Authorization)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("the authorization_endpoint %q is not a URL", ep.Authorization)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", a.ClientID)
	q.Set("redirect_uri", a.RedirectURI)
	if a.Scope != "" {
		q.Set("scope", a.Scope)
	}
	q.Set("state", a.state)
	q.Set("nonce", a.nonce)
	q.Set("code_challenge", challenge(a.verifier))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// IsCallback reports whether raw is the redirect back to this request's
// redirect URI — the point where a browser would hand over to the web client,
// and where the doctor stops. Scheme, host and path have to match; the query
// is the answer.
func (a *AuthCode) IsCallback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	want, err := url.Parse(a.RedirectURI)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, want.Scheme) && strings.EqualFold(u.Host, want.Host) &&
		strings.TrimRight(u.Path, "/") == strings.TrimRight(want.Path, "/")
}

// Callback reads the redirect back. It answers the OAuth error the provider
// sent there as an *Error, a state that is not this request's as
// ErrStateMismatch, or the code — and the issuer the redirect names (RFC 9207),
// "" when it names none, for the caller to compare with the one it expects.
func (a *AuthCode) Callback(raw string) (code, issuer string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("the redirect %q is not a URL", raw)
	}
	q := u.Query()
	if q.Get("error") != "" {
		return "", q.Get("iss"), &Error{Code: q.Get("error"), Description: q.Get("error_description")}
	}
	if q.Get("state") != a.state {
		return "", q.Get("iss"), ErrStateMismatch
	}
	if q.Get("code") == "" {
		return "", q.Get("iss"), ErrNoCode
	}
	return q.Get("code"), q.Get("iss"), nil
}

// Exchange trades the code for tokens (RFC 6749 §4.1.3), sending the verifier
// the challenge was made from.
func (c *Client) Exchange(ctx context.Context, ep *Endpoints, a *AuthCode, code string) (*Token, error) {
	var out tokenResponse
	err := c.post(ctx, ep.Token, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {a.RedirectURI},
		"client_id":     {a.ClientID},
		"code_verifier": {a.verifier},
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("%w: %s answered without an access token", ErrUnreachable, ep.Token)
	}
	return out.token(), nil
}

// ClaimError is an ID token that does not answer this request: which claim,
// and what it said instead.
type ClaimError struct {
	// Claim is the JWT claim: iss, aud, azp, nonce or exp.
	Claim string
	Got   string
	Want  string
}

func (e *ClaimError) Error() string {
	switch e.Claim {
	case "nonce":
		// The values are one sign-in's, and say nothing to a reader.
		return "the ID token carries another nonce than the request sent"
	case "exp":
		if e.Got == "" {
			return "the ID token carries no expiry"
		}
		return fmt.Sprintf("the ID token expired at %s", e.Got)
	case "aud":
		return fmt.Sprintf("the ID token is for %s, not for the client %q", e.Got, e.Want)
	case "azp":
		return fmt.Sprintf("the ID token was issued to %s, not to the client %q", e.Got, e.Want)
	default:
		return fmt.Sprintf("the ID token's %s is %q, not %q", e.Claim, e.Got, e.Want)
	}
}

// IDClaims is what an ID token says about the sign-in it answers.
type IDClaims struct {
	Issuer   string
	Audience []string
	Subject  string
	Expiry   time.Time
}

// CheckIDToken compares an ID token with what this request expects (OpenID
// Connect Core §3.1.3.7): the issuer the metadata named, this client among the
// audiences — and as the authorized party when there are several — this
// request's nonce, and an expiry still ahead of now.
//
// The signature is not verified, and does not need to be for what the doctor
// concludes: every API it calls next verifies the ACCESS token against the
// issuer's keys, and that verification is the one that decides whether the
// platform works.
func (a *AuthCode) CheckIDToken(raw, issuer string, now time.Time) (*IDClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("the ID token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, errors.New("the ID token's payload is not base64url")
	}
	var c struct {
		Iss   string   `json:"iss"`
		Aud   audience `json:"aud"`
		Azp   string   `json:"azp"`
		Nonce string   `json:"nonce"`
		Sub   string   `json:"sub"`
		Exp   int64    `json:"exp"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errors.New("the ID token's payload is not JSON")
	}
	out := &IDClaims{Issuer: c.Iss, Audience: c.Aud, Subject: c.Sub}
	if c.Exp > 0 {
		out.Expiry = time.Unix(c.Exp, 0)
	}
	switch {
	case c.Iss != strings.TrimRight(issuer, "/") && c.Iss != issuer:
		return out, &ClaimError{Claim: "iss", Got: c.Iss, Want: issuer}
	case !c.Aud.has(a.ClientID):
		return out, &ClaimError{Claim: "aud", Got: quoteAll(c.Aud), Want: a.ClientID}
	case len(c.Aud) > 1 && c.Azp != a.ClientID:
		return out, &ClaimError{Claim: "azp", Got: fmt.Sprintf("%q", c.Azp), Want: a.ClientID}
	case c.Nonce != a.nonce:
		return out, &ClaimError{Claim: "nonce"}
	case c.Exp == 0:
		return out, &ClaimError{Claim: "exp"}
	case !now.Before(out.Expiry):
		return out, &ClaimError{Claim: "exp", Got: out.Expiry.UTC().Format(time.RFC3339)}
	}
	return out, nil
}

// quoteAll names audiences for a message: "a", "b" — or (none).
func quoteAll(ss []string) string {
	if len(ss) == 0 {
		return "(no audience)"
	}
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}
