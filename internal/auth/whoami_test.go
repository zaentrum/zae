package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zaentrum/zae/internal/capability"
	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// instanceWithMe is an instance that answers GET /api/portal/me as answer
// says, for the bearer it was given; status overrides the 200.
type instanceWithMe struct {
	srv    *httptest.Server
	answer string
	status int
	bearer string // what the last /me carried
}

func newInstanceWithMe(t *testing.T, auth *capability.Auth, answer string) *instanceWithMe {
	t.Helper()
	in := &instanceWithMe{answer: answer}
	in.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case capability.Path:
			_ = json.NewEncoder(w).Encode(map[string]any{"capabilityVersion": 1, "services": []any{}, "auth": auth})
		case mePath:
			in.bearer = r.Header.Get("Authorization")
			if in.status != 0 {
				http.Error(w, "unauthorized", in.status)
				return
			}
			fmt.Fprint(w, in.answer)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(in.srv.Close)
	return in
}

// whoami asks the instance — the admin role is its setting, and whether a
// token counts for it is its decision — and says what it answered.
func TestWhoamiAsksTheInstance(t *testing.T) {
	setup(t)
	p := newIDP(t, granted("zaentrum-admin"))
	in := newInstanceWithMe(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"},
		`{"username":"ada","roles":["offline_access","platform-operators"],"isAdmin":true,"adminRole":"platform-operators","client":"zae"}`)
	if code, _, errs := run(t, func() int { return Login([]string{"--url", in.srv.URL, "--no-browser"}) }); code != exitcode.OK {
		t.Fatalf("setup login failed: %d %s", code, errs)
	}
	code, out, errs := run(t, func() int { return Whoami([]string{"--url", in.srv.URL}) })
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	noSecrets(t, "whoami", out, errs)
	for _, s := range []string{
		"bearer from the stored session",
		"subject     user-1",
		"username    ada",
		`admin role  yes — the instance grants this bearer its admin role ("platform-operators")`,
		"roles       offline_access, platform-operators",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("whoami lacks %q:\n%s", s, out)
		}
	}
	// The role name in the token is not what decides: this instance's admin
	// role is another one, and the token's "zaentrum-admin" is not mentioned.
	if strings.Contains(out, `"zaentrum-admin"`) {
		t.Errorf("whoami decoded a hard-coded role instead of asking:\n%s", out)
	}
	if !strings.HasPrefix(in.bearer, "Bearer ") || !strings.Contains(in.bearer, accessToken) {
		t.Fatalf("/me must carry the bearer whoami reports on")
	}

	// --json says the same, and who answered.
	code, out, _ = run(t, func() int { return Whoami([]string{"--url", in.srv.URL, "--json"}) })
	var doc whoamiDoc
	if code != exitcode.OK || json.Unmarshal([]byte(out), &doc) != nil {
		t.Fatalf("--json: %d %q", code, out)
	}
	if doc.From != "instance" || !doc.Admin || doc.AdminRole != "platform-operators" || doc.Username != "ada" || doc.Subject != "user-1" || doc.Bearer != "session" {
		t.Fatalf("--json: %+v", doc)
	}
	noSecrets(t, "whoami --json", out)
}

// The role in a token issued to another client: the instance does not take
// it, and whoami says why — the fix is signing in with the CLI, not a role.
func TestWhoamiExplainsARoleOnAnotherClientsToken(t *testing.T) {
	setup(t)
	in := newInstanceWithMe(t, nil,
		`{"username":"ada","roles":["zaentrum-admin"],"isAdmin":false,"adminRole":"zaentrum-admin","client":"chino-web"}`)
	t.Setenv(instance.TokenEnv, "an-opaque-browser-token")
	code, out, errs := run(t, func() int { return Whoami([]string{"--url", in.srv.URL}) })
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	for _, s := range []string{
		"bearer from " + instance.TokenEnv,
		"client      chino-web",
		"username    ada", // an opaque token, named by the instance
		`no — the token carries "zaentrum-admin", but was issued to the client "chino-web"`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("whoami lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "an-opaque-browser-token") {
		t.Fatal("whoami printed the token")
	}
}

// An older portal-api cannot say: the token is read as before, and whoami
// says that it was.
func TestWhoamiReadsTheTokenFromAnOlderPortal(t *testing.T) {
	setup(t)
	p := newIDP(t, granted(instance.AdminRole))
	for name, answer := range map[string]string{
		"no /me at all":       "",
		"/me without isAdmin": `{"username":"ada","roles":["zaentrum-admin"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var srv *httptest.Server
			if answer == "" {
				srv = fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})
			} else {
				srv = newInstanceWithMe(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"}, answer).srv
			}
			if code, _, errs := run(t, func() int { return Login([]string{"--url", srv.URL, "--no-browser"}) }); code != exitcode.OK {
				t.Fatalf("setup login failed: %d %s", code, errs)
			}
			code, out, errs := run(t, func() int { return Whoami([]string{"--url", srv.URL}) })
			if code != exitcode.OK {
				t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
			}
			for _, s := range []string{"answered    by the token itself", "username    ada", `admin role  yes — the token carries "zaentrum-admin"`} {
				if !strings.Contains(out, s) {
					t.Errorf("whoami lacks %q:\n%s", s, out)
				}
			}
		})
	}
}

// A bearer the instance refuses is exit 5; an instance that cannot be asked
// is 4 — after what the token claims, so there is still something to go on.
func TestWhoamiRefusedAndUnreachable(t *testing.T) {
	setup(t)
	p := newIDP(t, granted(instance.AdminRole))
	in := newInstanceWithMe(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"}, "")
	if code, _, errs := run(t, func() int { return Login([]string{"--url", in.srv.URL, "--no-browser"}) }); code != exitcode.OK {
		t.Fatalf("setup login failed: %d %s", code, errs)
	}
	in.status = http.StatusUnauthorized
	code, out, errs := run(t, func() int { return Whoami([]string{"--url", in.srv.URL}) })
	if code != exitcode.Forbidden || !strings.Contains(errs, "refuses this bearer") {
		t.Fatalf("a refused bearer: want 5, got %d\n%s\n%s", code, out, errs)
	}
	noSecrets(t, "refused", out, errs)

	in.srv.Close()
	code, out, errs = run(t, func() int { return Whoami([]string{"--url", in.srv.URL}) })
	if code != exitcode.Undetermined || !strings.Contains(errs, "cannot ask") || !strings.Contains(out, "username    ada") {
		t.Fatalf("unreachable: want 4 with the token's claims, got %d\n%s\n%s", code, out, errs)
	}
	code, out, _ = run(t, func() int { return Whoami([]string{"--url", in.srv.URL, "--json"}) })
	var doc whoamiDoc
	if code != exitcode.Undetermined || json.Unmarshal([]byte(out), &doc) != nil || doc.From != "token" || doc.AdminRole != instance.AdminRole {
		t.Fatalf("unreachable --json: %d %q", code, out)
	}
}

// login asks the same question, with the token it was just given — not with
// whatever ZAE_TOKEN happens to hold.
func TestLoginAsksTheInstanceForTheAdminRole(t *testing.T) {
	setup(t)
	p := newIDP(t, granted("zaentrum-admin"))
	in := newInstanceWithMe(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"},
		`{"username":"ada","roles":["zaentrum-admin"],"isAdmin":false,"adminRole":"platform-operators","client":"zae"}`)
	t.Setenv(instance.TokenEnv, "somebody-elses-token")
	code, out, errs := run(t, func() int { return Login([]string{"--url", in.srv.URL, "--no-browser"}) })
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, `admin role no — the instance does not grant this bearer its admin role ("platform-operators")`) {
		t.Errorf("login must report the instance's answer:\n%s", out)
	}
	if !strings.Contains(in.bearer, accessToken) || strings.Contains(in.bearer, "somebody-elses-token") {
		t.Fatalf("login asked with another bearer than the one it just got")
	}
	noSecrets(t, "login", out, errs)
}
