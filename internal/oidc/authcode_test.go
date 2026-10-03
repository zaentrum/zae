package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A JWT whose payload is the claims given; nothing here reads a signature.
func jwtOf(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

// The metadata the code flow needs is read without asking for the device
// grant: an issuer that has none is still one a browser signs in to.
func TestMetadataDoesNotRequireTheDeviceGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"issuer":"https://idp.example.org/realms/z",
			"authorization_endpoint":"https://idp.example.org/somewhere/auth",
			"token_endpoint":"https://idp.example.org/somewhere/token"}`)
	}))
	defer srv.Close()
	ep, err := (&Client{}).Metadata(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if ep.Authorization != "https://idp.example.org/somewhere/auth" || ep.Token != "https://idp.example.org/somewhere/token" {
		t.Fatalf("endpoints were not read from the document: %+v", ep)
	}
	if _, err := (&Client{}).Discover(context.Background(), srv.URL); !errors.Is(err, ErrNoDeviceGrant) {
		t.Fatalf("the device grant still needs its endpoint: %v", err)
	}
}

// The request carries what a browser's does: PKCE S256 of a fresh verifier,
// a state and a nonce of its own — and keeps a query the endpoint already has.
func TestAuthCodeURL(t *testing.T) {
	a, err := NewAuthCode("web", "https://media.example.org/auth/callback", "openid profile")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.URL(&Endpoints{Authorization: "https://idp.example.org/auth?kc_idp_hint=none"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	for k, want := range map[string]string{
		"response_type": "code", "client_id": "web", "redirect_uri": "https://media.example.org/auth/callback",
		"scope": "openid profile", "code_challenge_method": "S256", "kc_idp_hint": "none",
		"state": a.state, "nonce": a.nonce, "code_challenge": challenge(a.verifier),
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if strings.Contains(raw, a.verifier) {
		t.Fatal("the verifier itself must never leave in the request")
	}
	b, _ := NewAuthCode("web", "https://media.example.org/auth/callback", "openid")
	if a.state == b.state || a.nonce == b.nonce || a.verifier == b.verifier || a.state == a.nonce {
		t.Fatal("state, nonce and verifier must be fresh and independent")
	}
	if _, err := a.URL(&Endpoints{}); err == nil {
		t.Fatal("no authorization endpoint is an error, not an empty URL")
	}
}

func TestAuthCodeCallback(t *testing.T) {
	a, _ := NewAuthCode("web", "https://media.example.org/auth/callback", "openid")
	cb := func(q string) string { return "https://media.example.org/auth/callback?" + q }

	code, iss, err := a.Callback(cb("state=" + a.state + "&code=C&iss=https%3A%2F%2Fidp.example.org"))
	if err != nil || code != "C" || iss != "https://idp.example.org" {
		t.Fatalf("a good answer: %q %q %v", code, iss, err)
	}
	if _, _, err := a.Callback(cb("state=someone-else&code=C")); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("another request's state must be refused: %v", err)
	}
	if _, _, err := a.Callback(cb("state=" + a.state)); !errors.Is(err, ErrNoCode) {
		t.Fatalf("no code: %v", err)
	}
	var oe *Error
	_, _, err = a.Callback(cb("error=unauthorized_client&error_description=Client+is+not+allowed&state=" + a.state))
	if !errors.As(err, &oe) || oe.Code != "unauthorized_client" || oe.Description != "Client is not allowed" {
		t.Fatalf("the provider's error must come back as it was sent: %v", err)
	}

	for raw, want := range map[string]bool{
		cb("code=x"): true,
		"https://media.example.org/auth/callback/": true,
		"https://MEDIA.example.org/auth/callback":  true,
		"https://media.example.org/auth/other":     false,
		"https://evil.example.org/auth/callback":   false,
		"http://media.example.org/auth/callback":   false,
	} {
		if got := a.IsCallback(raw); got != want {
			t.Errorf("IsCallback(%q) = %v, want %v", raw, got, want)
		}
	}
}

// The exchange sends the code, the redirect URI and the verifier the
// challenge was made from, and hands back the ID token with the others.
func TestExchange(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		if form.Get("code") == "used" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"Code not valid"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"A","id_token":"I","refresh_token":"R","expires_in":300}`)
	}))
	defer srv.Close()
	a, _ := NewAuthCode("web", "https://media.example.org/auth/callback", "openid")
	tok, err := (&Client{}).Exchange(context.Background(), &Endpoints{Token: srv.URL}, a, "C")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "A" || tok.IDToken != "I" || tok.RefreshToken != "R" {
		t.Fatalf("tokens: %+v", tok)
	}
	for k, want := range map[string]string{"grant_type": "authorization_code", "code": "C",
		"redirect_uri": a.RedirectURI, "client_id": "web", "code_verifier": a.verifier} {
		if form.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, form.Get(k), want)
		}
	}
	var oe *Error
	if _, err := (&Client{}).Exchange(context.Background(), &Endpoints{Token: srv.URL}, a, "used"); !errors.As(err, &oe) || oe.Code != "invalid_grant" {
		t.Fatalf("a refused exchange is the provider's error: %v", err)
	}
}

// OpenID Connect Core §3.1.3.7, the parts that say whether this token answers
// this request: issuer, audience (and the authorized party when there are
// several), nonce, expiry.
func TestCheckIDToken(t *testing.T) {
	a, _ := NewAuthCode("web", "https://media.example.org/auth/callback", "openid")
	const iss = "https://idp.example.org/realms/z"
	now := time.Now()
	good := func() map[string]any {
		return map[string]any{"iss": iss, "aud": "web", "nonce": a.nonce, "sub": "s", "exp": now.Add(time.Minute).Unix()}
	}
	with := func(k string, v any) string {
		c := good()
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return jwtOf(c)
	}
	if c, err := a.CheckIDToken(jwtOf(good()), iss, now); err != nil || c.Subject != "s" {
		t.Fatalf("a token answering this request: %v", err)
	}
	if _, err := a.CheckIDToken(with("aud", []string{"web", "other"}), iss, now); err == nil {
		t.Fatal("several audiences and no authorized party must be refused")
	}
	c2 := good()
	c2["aud"], c2["azp"] = []string{"other", "web"}, "web"
	if _, err := a.CheckIDToken(jwtOf(c2), iss, now); err != nil {
		t.Fatalf("several audiences with this client as the authorized party: %v", err)
	}
	for name, tc := range map[string]struct {
		tok   string
		claim string
	}{
		"another issuer": {with("iss", "https://sso.internal.example/realms/z"), "iss"},
		"another client": {with("aud", "chino-tv"), "aud"},
		"no audience":    {with("aud", nil), "aud"},
		"another nonce":  {with("nonce", "replayed"), "nonce"},
		"no nonce":       {with("nonce", nil), "nonce"},
		"expired":        {with("exp", now.Add(-time.Second).Unix()), "exp"},
		"no expiry":      {with("exp", nil), "exp"},
		"azp another":    {jwtOf(map[string]any{"iss": iss, "aud": []string{"web", "x"}, "azp": "x", "nonce": a.nonce, "exp": now.Add(time.Minute).Unix()}), "azp"},
		// OpenID Connect compares issuers exactly: a slash more is another one.
		"a trailing slash": {with("iss", iss+"/"), "iss"},
	} {
		_, err := a.CheckIDToken(tc.tok, iss, now)
		var ce *ClaimError
		if !errors.As(err, &ce) || ce.Claim != tc.claim {
			t.Errorf("%s: want a %s claim error, got %v", name, tc.claim, err)
			continue
		}
		if tc.claim == "nonce" && (strings.Contains(err.Error(), a.nonce) || strings.Contains(err.Error(), "replayed")) {
			t.Errorf("%s: a nonce is one sign-in's and is not printed: %v", name, err)
		}
		if tc.claim == "iss" && !strings.Contains(err.Error(), iss) {
			t.Errorf("%s: the message names the issuer expected: %v", name, err)
		}
		if name == "no expiry" && !strings.Contains(err.Error(), "carries no expiry") {
			t.Errorf("%s: a token without an expiry is not one that expired in year 1: %v", name, err)
		}
	}
	for _, bad := range []string{"", "opaque", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("[")) + ".c"} {
		if _, err := a.CheckIDToken(bad, iss, now); err == nil {
			t.Errorf("%q is not an ID token", bad)
		}
	}
}

// An access token names its issuer and audience, as one string or a list;
// an exotic `aud` reads as none and does not make the token unreadable.
func TestParseClaimsReadsIssuerAndAudience(t *testing.T) {
	for aud, want := range map[string][]string{`"chino"`: {"chino"}, `["account","chino"]`: {"account", "chino"}, `7`: nil, `""`: nil} {
		raw := "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"s","iss":"https://idp.example.org","aud":`+aud+`}`)) + ".s"
		c, ok := ParseClaims(raw)
		if !ok || c.Issuer != "https://idp.example.org" || fmt.Sprint(c.Audience) != fmt.Sprint(want) {
			t.Errorf("aud %s: %+v %v", aud, c, ok)
		}
		if want != nil && (!c.HasAudience(want[len(want)-1]) || c.HasAudience("other")) {
			t.Errorf("aud %s: HasAudience", aud)
		}
	}
}
