// Package instance is what every zae command that talks to an instance
// shares: how the instance is named on the command line, how the caller's
// credentials travel, and what to say when the instance refuses them. One
// place, so that a change to how zae authenticates happens once and not per
// command.
package instance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/creds"
	"github.com/zaentrum/zae/internal/oidc"
)

// TokenEnv carries a bearer minted elsewhere — a service account's
// client-credentials token, a CI secret, a browser session copied out by
// hand. It wins over a stored session on purpose: a script that sets it is
// stating which identity it means, and a developer's own login must not
// quietly override it.
const TokenEnv = "ZAE_TOKEN"

// AdminRole is the platform's admin realm role as the portal defaults it
// (PORTAL_ADMIN_ROLE). Only used to TELL a person whether their token carries
// it — the instance decides, always.
const AdminRole = "zaentrum-admin"

// refreshSkew renews a token that is about to expire rather than sending one
// that dies in flight.
const refreshSkew = 60 * time.Second

// ErrSessionExpired: there is a stored session for the instance and it could
// not be renewed. Distinct from having no credentials at all, because the
// answer differs: sign in again, rather than sign in.
var ErrSessionExpired = errors.New("session expired")

// Token is the bearer to send to base, or "" when zae has none — in which
// case the call is made unauthenticated and the instance's own answer (401)
// decides, which is how a CLI learns that an endpoint is public.
//
// The order is ZAE_TOKEN, then the stored session for this instance, renewed
// with its refresh token when it has expired. A renewal is written back, so
// one expired token means one extra round trip and not one per command.
func Token(ctx context.Context, base string) (string, error) {
	if tok := strings.TrimSpace(os.Getenv(TokenEnv)); tok != "" {
		return tok, nil
	}
	e, ok, err := creds.Get(base)
	if err != nil || !ok {
		return "", err
	}
	if e.Valid(refreshSkew) {
		return e.AccessToken, nil
	}
	if e.RefreshToken == "" {
		return "", expired(base, errors.New("the stored session has no refresh token"))
	}
	tok, err := renew(ctx, base, e)
	if err != nil {
		return "", err
	}
	return tok, nil
}

// renew exchanges the refresh token and writes the result back.
func renew(ctx context.Context, base string, e creds.Entry) (string, error) {
	c := &oidc.Client{HTTP: &http.Client{Timeout: 30 * time.Second}}
	ep, err := c.Discover(ctx, e.Issuer)
	if err != nil {
		return "", expired(base, err)
	}
	tok, err := c.Refresh(ctx, ep, e.ClientID, e.RefreshToken)
	if err != nil {
		return "", expired(base, err)
	}
	e.AccessToken = tok.AccessToken
	e.RefreshToken = tok.RefreshToken
	e.Expiry = tok.Expiry
	e.SavedAt = time.Now()
	if claims, ok := oidc.ParseClaims(tok.AccessToken); ok {
		e.Subject, e.Username = claims.Subject, claims.Username
	}
	if err := creds.Put(base, e); err != nil {
		// The token is good; only the write failed. Use it for this command and
		// say so, rather than failing a call that would have worked.
		fmt.Fprintf(os.Stderr, "zae: warning: renewed the session for %s but could not store it: %v\n", base, err)
	}
	return tok.AccessToken, nil
}

// expired wraps the cause in a message that says what to do. The cause is
// included because "sign in again" is right but "why" is what a person needs
// when signing in again does not help either.
func expired(base string, cause error) error {
	return fmt.Errorf("%w — run: zae login --url %s (%v)", ErrSessionExpired, base, cause)
}

// Authorize attaches the caller's bearer for base. An error means zae HAS
// credentials for this instance and could not make them usable; callers map
// that onto the forbidden exit code, since it is an authentication problem
// and not a failure of the command.
func Authorize(ctx context.Context, req *http.Request, base string) error {
	tok, err := Token(ctx, base)
	if err != nil {
		return err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return nil
}

// ForbiddenHint says what to do about a 401 or 403 — which depends entirely
// on what zae sent: nothing, an environment bearer, or a stored session. need
// is what the call requires (e.g. a role) when the caller knows it.
func ForbiddenHint(base, need string) string {
	with := func(s string) string {
		if need != "" {
			return s + " — " + need
		}
		return s
	}
	if os.Getenv(TokenEnv) != "" {
		return with("the token in " + TokenEnv + " was refused")
	}
	if _, ok, _ := creds.Get(base); ok {
		// Signed in, and still refused: the session is real but the account
		// lacks the role. Another login with the same account changes nothing,
		// so say what would.
		return with(fmt.Sprintf("the stored session for %s was refused; it is signed in, so this is a missing role, not a missing login", base))
	}
	return with(fmt.Sprintf("run: zae login --url %s, or supply a bearer via %s", base, TokenEnv))
}

// Base validates --url and returns the instance's origin without a trailing
// slash. The error is phrased for a usage message.
func Base(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("--url is required (the instance's public address, e.g. https://media.example.org)")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("--url %q is not an http(s) address like https://media.example.org", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}
