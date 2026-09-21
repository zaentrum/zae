package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An issuer under a path prefix is the normal case, not an edge: a realm is
// served at /auth/realms/<name>, and its endpoints may be anywhere that
// document says. Building them by concatenation works on one deployment and
// lies on the next, so the test pins the endpoints somewhere concatenation
// would never find.
func TestDiscoverReadsEndpointsInsteadOfBuildingThem(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/realms/zaentrum/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer":%q,
			"device_authorization_endpoint":%q,
			"token_endpoint":%q}`,
			srv.URL+"/auth/realms/zaentrum",
			srv.URL+"/somewhere/else/device/auth",
			srv.URL+"/another/place/token")
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	ep, err := (&Client{}).Discover(context.Background(), srv.URL+"/auth/realms/zaentrum/")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if ep.Device != srv.URL+"/somewhere/else/device/auth" || ep.Token != srv.URL+"/another/place/token" {
		t.Fatalf("endpoints were not taken from the document: %+v", ep)
	}
	if ep.Issuer != srv.URL+"/auth/realms/zaentrum" {
		t.Fatalf("issuer: %q", ep.Issuer)
	}
}

// The one failure an operator can fix gets its own sentinel, so the caller
// can say "enable the device grant" instead of "unreachable".
func TestDiscoverWithoutDeviceEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer":"https://idp.example.org","token_endpoint":"https://idp.example.org/token"}`)
	}))
	defer srv.Close()
	_, err := (&Client{}).Discover(context.Background(), srv.URL)
	if !errors.Is(err, ErrNoDeviceGrant) {
		t.Fatalf("want ErrNoDeviceGrant, got %v", err)
	}
}

// A login page, a proxy, an HTML error: not an OAuth answer, so "cannot
// tell" — never "the provider refused you".
func TestNonOAuthAnswersAreUnreachableNotDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>gateway timeout</html>", http.StatusGatewayTimeout)
	}))
	defer srv.Close()
	c := &Client{}
	if _, err := c.Discover(context.Background(), srv.URL); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("discover: want unreachable, got %v", err)
	}
	ep := &Endpoints{Issuer: srv.URL, Device: srv.URL, Token: srv.URL}
	if _, err := c.Authorize(context.Background(), ep, "zae", "openid"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("authorize: want unreachable, got %v", err)
	}
}

// Ctrl-C must end a wait where it stands — not at the next five-second tick,
// and not after the code's ten minutes.
func TestPollStopsWhenTheContextEnds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"authorization_pending"}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Client{}
	_, err := c.Poll(ctx, &Endpoints{Token: srv.URL}, "zae", &Device{Interval: 5, ExpiresIn: 600, deviceCode: "d", verifier: "v"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want the context's error, got %v", err)
	}
}

// A provider that does not rotate refresh tokens omits it; the one we hold
// stays valid, so it must survive the exchange.
func TestRefreshKeepsANonRotatedRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"new","expires_in":300}`)
	}))
	defer srv.Close()
	tok, err := (&Client{}).Refresh(context.Background(), &Endpoints{Token: srv.URL}, "zae", "keep-me")
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "keep-me" {
		t.Fatalf("refresh token lost: %+v", tok)
	}
	if tok.Expiry.IsZero() || !tok.Expiry.After(time.Now()) {
		t.Fatalf("expiry must be absolute and in the future: %+v", tok)
	}
}

// An identity provider that requires PKCE answers a challenge-less request
// with this shape. zae always sends one, so recognising it lets the CLI point
// at itself (upgrade, or a proxy eating the form) instead of the realm.
func TestIsMissingPKCE(t *testing.T) {
	err := &Error{Code: "invalid_request", Description: "Missing parameter: code_challenge_method", Status: 400}
	if !IsMissingPKCE(err) {
		t.Fatal("the missing-PKCE answer was not recognised")
	}
	if IsMissingPKCE(&Error{Code: "invalid_client", Description: "Invalid client credentials"}) {
		t.Fatal("an unrelated refusal was read as missing PKCE")
	}
	if IsMissingPKCE(ErrUnreachable) {
		t.Fatal("a transport error is not an OAuth refusal")
	}
}

// The codes that mean something specific must be errors.Is-able, so callers
// map them onto exit codes without matching strings.
func TestOAuthErrorsUnwrapToSentinels(t *testing.T) {
	if !errors.Is(&Error{Code: "access_denied"}, ErrDenied) {
		t.Fatal("access_denied must be ErrDenied")
	}
	if !errors.Is(&Error{Code: "expired_token"}, ErrCodeExpired) {
		t.Fatal("expired_token must be ErrCodeExpired")
	}
	if errors.Is(&Error{Code: "authorization_pending"}, ErrDenied) {
		t.Fatal("pending is not a refusal")
	}
	// The description is where a provider says what to fix: keep it.
	e := &Error{Code: "invalid_client", Description: "Invalid client or Invalid client credentials"}
	if !strings.Contains(e.Error(), "Invalid client credentials") {
		t.Fatalf("the provider's description was dropped: %q", e)
	}
}

// zae reads an access token's claims to say who you are; it never pretends
// to have verified them, and an opaque token is an answer, not an error.
func TestParseClaims(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"sub": "user-1", "preferred_username": "ada", "exp": time.Now().Add(time.Hour).Unix(),
		"realm_access": map[string]any{"roles": []string{"zaentrum-admin", "user"}},
	})
	tok := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	c, ok := ParseClaims(tok)
	if !ok || c.Subject != "user-1" || c.Username != "ada" {
		t.Fatalf("claims: %+v (%v)", c, ok)
	}
	if !c.HasRole("zaentrum-admin") || c.HasRole("nope") || c.HasRole("") {
		t.Fatalf("roles: %+v", c.Roles)
	}
	if c.Expiry.IsZero() {
		t.Fatal("exp was not read")
	}
	for _, bad := range []string{"", "opaque-token", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".c"} {
		if _, ok := ParseClaims(bad); ok {
			t.Fatalf("ParseClaims(%q) must say it cannot read it", bad)
		}
	}
}

// A verifier is fresh per login and inside RFC 7636's length window; the
// challenge is its unpadded base64url SHA-256.
func TestVerifierAndChallenge(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		v, err := newVerifier()
		if err != nil {
			t.Fatal(err)
		}
		if len(v) < 43 || len(v) > 128 {
			t.Fatalf("verifier length %d outside 43–128: %q", len(v), v)
		}
		if strings.ContainsAny(v, "=+/") {
			t.Fatalf("verifier is not base64url without padding: %q", v)
		}
		if seen[v] {
			t.Fatalf("verifier repeated: %q", v)
		}
		seen[v] = true
		if ch := challenge(v); len(ch) != 43 || strings.ContainsAny(ch, "=+/") {
			t.Fatalf("challenge is not unpadded base64url of a SHA-256: %q", ch)
		}
	}
	// RFC 7636 appendix B's worked example, so the encoding itself is pinned.
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := challenge(verifier); got != want {
		t.Fatalf("S256(%q) = %q, want %q", verifier, got, want)
	}
}
