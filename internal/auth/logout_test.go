package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zaentrum/zae/internal/capability"
	"github.com/zaentrum/zae/internal/creds"
	"github.com/zaentrum/zae/internal/exitcode"
)

// signIn logs in to a fresh fake instance of the identity provider p.
func signIn(t *testing.T, p *idp) *httptest.Server {
	t.Helper()
	srv := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})
	if code, _, errs := run(t, func() int { return Login([]string{"--url", srv.URL, "--no-browser"}) }); code != exitcode.OK {
		t.Fatalf("setup login failed: %d %s", code, errs)
	}
	return srv
}

// Signing out ends the session where it lives — the refresh token revoked at
// the endpoint the issuer names (RFC 7009) — and then forgets it here.
func TestLogoutRevokesTheSessionThenForgetsIt(t *testing.T) {
	setup(t)
	p := newIDP(t, granted())
	srv := signIn(t, p)

	code, out, errs := run(t, func() int { return Logout([]string{"--url", srv.URL}) })
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	noSecrets(t, "logout", out, errs)
	if len(p.revoked) != 1 {
		t.Fatalf("want one revocation, got %d", len(p.revoked))
	}
	form := p.revoked[0]
	if form.Get("token") != refreshToken || form.Get("token_type_hint") != "refresh_token" || form.Get("client_id") != "zae" {
		t.Fatalf("the revocation must name the refresh token, its kind and the client: %v", form)
	}
	for _, s := range []string{"ended the session for " + srv.URL + " at " + p.issuer() + ": its refresh token is revoked",
		"removed the stored session for " + srv.URL} {
		if !strings.Contains(out, s) {
			t.Errorf("logout lacks %q:\n%s", s, out)
		}
	}
	if strings.Index(out, "ended the session") > strings.Index(out, "removed the stored session") {
		t.Errorf("the session is ended first, then forgotten:\n%s", out)
	}
	if _, ok, _ := creds.Get(srv.URL); ok {
		t.Fatal("the entry survived logout")
	}
}

// Whatever the issuer answers, the session is forgotten here — nobody stays
// signed in because an identity provider is down — and the exit code says
// whether it ended there too.
func TestLogoutForgetsTheSessionWhenTheIssuerCannotEndIt(t *testing.T) {
	for name, c := range map[string]struct {
		prepare func(p *idp)
		code    int
		says    string
	}{
		"no revocation endpoint": {func(p *idp) { p.noRevocation = true }, exitcode.NotOffered, "offers no token revocation"},
		"a refusal": {func(p *idp) {
			p.revokeStatus, p.revokeReply = http.StatusBadRequest, `{"error":"unsupported_token_type","error_description":"Unsupported token type"}`
		}, exitcode.Failed, "refused to revoke"},
		"unreachable": {func(p *idp) { p.srv.Close() }, exitcode.Undetermined, "could not end the session"},
	} {
		t.Run(name, func(t *testing.T) {
			setup(t)
			p := newIDP(t, granted())
			srv := signIn(t, p)
			c.prepare(p)
			code, out, errs := run(t, func() int { return Logout([]string{"--url", srv.URL}) })
			if code != c.code || !strings.Contains(errs, c.says) || !strings.Contains(errs, "stays valid there until it expires") {
				t.Fatalf("want %d saying %q and how long it lives on, got %d\n%s\n%s", c.code, c.says, code, out, errs)
			}
			noSecrets(t, name, out, errs)
			if _, ok, _ := creds.Get(srv.URL); ok {
				t.Fatal("the session must be forgotten here all the same")
			}
			if !strings.Contains(out, "removed the stored session") {
				t.Errorf("and say so:\n%s", out)
			}
		})
	}
}

// An entry without a refresh token has its access token revoked instead.
func TestLogoutRevokesAnAccessTokenWhenThereIsNoRefreshToken(t *testing.T) {
	setup(t)
	p := newIDP(t)
	const base = "https://media.example.org"
	if err := creds.Put(base, creds.Entry{Issuer: p.issuer(), ClientID: "zae", AccessToken: accessToken}); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, func() int { return Logout([]string{"--url", base}) })
	if code != exitcode.OK || len(p.revoked) != 1 || p.revoked[0].Get("token_type_hint") != "access_token" || p.revoked[0].Get("token") != accessToken {
		t.Fatalf("want the access token revoked, got %d %v\n%s\n%s", code, p.revoked, out, errs)
	}
	noSecrets(t, "logout", out, errs)
}

func TestLogoutAllEndsEverySession(t *testing.T) {
	setup(t)
	p := newIDP(t, granted())
	signIn(t, p)
	signIn(t, p)
	code, out, errs := run(t, func() int { return Logout([]string{"--all"}) })
	if code != exitcode.OK || len(p.revoked) != 2 || !strings.Contains(out, "removed 2 stored session(s)") {
		t.Fatalf("want both revoked and the file gone, got %d (%d revoked)\n%s\n%s", code, len(p.revoked), out, errs)
	}
	path, _ := creds.Path()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("logout --all must leave no file: %v", err)
	}

	// One that cannot be ended: every session is still forgotten, and the exit
	// code is that one's.
	q := newIDP(t, granted())
	signIn(t, p)
	signIn(t, q)
	q.noRevocation = true
	code, _, errs = run(t, func() int { return Logout([]string{"--all"}) })
	if code != exitcode.NotOffered || !strings.Contains(errs, "offers no token revocation") {
		t.Fatalf("want 3 for the one that could not be ended, got %d %q", code, errs)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("every session is forgotten all the same: %v", err)
	}
}
