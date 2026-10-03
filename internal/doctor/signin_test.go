package doctor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/creds"
	"github.com/zaentrum/zae/internal/instance"
)

// The whole path, as a person takes it: the login page, the form, the
// redirect back, the code exchanged — and then the platform used with the
// token, reading only. Every step is its own line, and none of them prints a
// credential.
func TestSignInWithAPasswordTheWayAPersonDoes(t *testing.T) {
	w := newWorld(t)
	signedIn(t)
	code, out, errs := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 0 {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	noSecrets(t, "a passing run", out, errs)
	for _, name := range []string{
		"sign-in", "chino-api: config", "sign-in: issuer", "sign-in: authorization", "sign-in: login form",
		"sign-in: callback", "sign-in: token exchange", "sign-in: id token", "sign-in: access token",
		"portal: launchpad", "app /portal/", "app /chino/", "app /katalog/",
		"chino-api: items", "chino-api: item detail", "chino-api: poster", "chino-api: people search",
		"chino-api: person", "chino-api: portrait", "chino-api: playback", "katalog-manager: graphql",
	} {
		if m := mark(out, name); m != "✓" {
			t.Errorf("%s: want ✓, got %q (%s)\n%s", name, m, line(out, name), out)
		}
	}
	// The launchpad decides which apps are checked: a disabled tile, a link
	// to another site and a page inside the portal add nothing.
	if strings.Contains(out, "app /tv/") || strings.Contains(out, "docs.example.org") || strings.Count(out, "app /portal/") != 1 {
		t.Errorf("the apps checked are the launchpad's open, local mounts, once each:\n%s", out)
	}
	// What a browser sends: the hidden input of the form, and nothing it
	// would not — the unchecked "remember me" stays unchecked.
	if len(w.forms) != 1 {
		t.Fatalf("the form must be sent once, got %d", len(w.forms))
	}
	f := w.forms[0]
	if _, ok := f["credentialId"]; !ok {
		t.Errorf("the form's hidden input was not sent: %v", f)
	}
	if _, ok := f["rememberMe"]; ok {
		t.Errorf("an unchecked checkbox was sent: %v", f)
	}
	// Read-only: GETs, the sign-in's two POSTs and one GraphQL query.
	if writes := w.writes(); len(writes) != 0 {
		t.Errorf("the run wrote to the instance: %v", writes)
	}
	// The playlist was asked for as a client that plays everything, so the
	// package answers and nothing is transcoded.
	if w.masterCaps != allCaps {
		t.Errorf("the master playlist was asked for with caps %q", w.masterCaps)
	}
	if !strings.Contains(line(out, "sign-in: callback"), w.srv.URL+callbackPath) {
		t.Errorf("the web client's redirect URI is the site root's %s:\n%s", callbackPath, out)
	}
}

// The ways a sign-in stops, each named for its likely cause.
func TestSignInFailuresNameTheirCause(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(w *world)
		step    string
		detail  []string
		fix     []string
		noAfter string // a step that must not have run
	}{
		{"wrong password", func(w *world) { w.password = "something else" },
			"sign-in: login form", []string{"wrong password", "“Invalid username or password.”"},
			[]string{UserEnv, PasswordEnv}, "sign-in: callback"},
		{"UPDATE_PASSWORD pending", func(w *world) { w.requiredFor = "UPDATE_PASSWORD" },
			"sign-in: login form", []string{"required action pending: UPDATE_PASSWORD", "must set a new password"},
			[]string{"Required user actions"}, "sign-in: callback"},
		{"VERIFY_PROFILE pending", func(w *world) { w.requiredFor = "VERIFY_PROFILE" },
			"sign-in: login form", []string{"required action pending: VERIFY_PROFILE", "profile"},
			[]string{"clear it"}, "sign-in: callback"},
		{"a second factor", func(w *world) { w.otp = true },
			"sign-in: login form", []string{"another step"}, []string{"no second factor"}, "sign-in: callback"},
		{"redirect URI not allowed", func(w *world) {
			w.allowRedirect = func(u string) bool { return !strings.HasSuffix(u, callbackPath) }
		}, "sign-in: authorization", []string{"redirect URI not allowed", callbackPath},
			[]string{"valid redirect URIs", webClient}, "sign-in: login form"},
		{"an unknown web client", func(w *world) { delete(w.clients, webClient) },
			"sign-in: authorization", []string{"does not know the client", webClient}, []string{"oidcClientId.web"}, "sign-in: login form"},
		{"another state in the redirect", func(w *world) { w.stateOverride = "someone-elses" },
			"sign-in: callback", []string{"different state"}, []string{"another sign-in"}, "sign-in: token exchange"},
		{"no ID token", func(w *world) { w.noIDToken = true },
			"sign-in: id token", []string{"no ID token"}, []string{"openid"}, "sign-in: access token"},
		{"an ID token from another issuer", func(w *world) { w.idIssuer = "https://sso.internal.example/realms/zaentrum" },
			"sign-in: id token", []string{"issuer mismatch", "sso.internal.example"}, []string{"hostname"}, "sign-in: access token"},
		{"an ID token for another sign-in", func(w *world) { w.idNonce = "replayed-nonce" },
			"sign-in: id token", []string{"another nonce"}, []string{"replays"}, "sign-in: access token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			c.setup(w)
			signedIn(t)
			code, out, errs := doctor(t, "--url", w.srv.URL, "--sign-in")
			if code != 1 {
				t.Fatalf("want 1, got %d\n%s\n%s", code, out, errs)
			}
			noSecrets(t, c.name, out, errs)
			if m := mark(out, c.step); m != "✗" {
				t.Fatalf("%s: want ✗, got %q\n%s", c.step, m, out)
			}
			for _, d := range c.detail {
				if !strings.Contains(line(out, c.step), d) {
					t.Errorf("the line lacks %q: %s", d, line(out, c.step))
				}
			}
			for _, f := range c.fix {
				if !strings.Contains(fixOf(out, c.step), f) {
					t.Errorf("the fix lacks %q: %q", f, fixOf(out, c.step))
				}
			}
			if line(out, c.noAfter) != "" {
				t.Errorf("%s ran after a failed step:\n%s", c.noAfter, out)
			}
			if m := mark(out, "signed-in checks"); m != "-" || !strings.Contains(line(out, "signed-in checks"), "did not complete") {
				t.Errorf("what did not run is said once: %q\n%s", line(out, "signed-in checks"), out)
			}
			if n := w.called("GET " + itemsPath); n != 0 {
				t.Errorf("no API is used without a token, got %d item reads", n)
			}
			if writes := w.writes(); len(writes) != 0 {
				t.Errorf("a failing run wrote to the instance: %v", writes)
			}
		})
	}
}

// A wrong password is tried ONCE: an account the brute-force protection locks
// would fail every verification after it.
func TestAWrongPasswordIsTriedOnce(t *testing.T) {
	w := newWorld(t)
	w.password = "something else"
	signedIn(t)
	doctor(t, "--url", w.srv.URL, "--sign-in")
	if n := w.called("POST " + realmPath + "/login-actions/authenticate"); n != 1 {
		t.Fatalf("the form was sent %d times", n)
	}
}

// The password goes to the identity provider and nowhere else: a login form
// that posts to another origin is refused before anything is sent.
func TestThePasswordIsSentOnlyToTheIdentityProvider(t *testing.T) {
	var got []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
	}))
	defer elsewhere.Close()
	w := newWorld(t)
	w.formAction = elsewhere.URL + realmPath + "/login-actions/authenticate?session_code=x"
	signedIn(t)
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 1 || mark(out, "sign-in: login form") != "✗" {
		t.Fatalf("want the form step to fail, got %d\n%s", code, out)
	}
	if len(got) != 0 {
		t.Fatalf("the form was sent to another origin: %v", got)
	}
	if !strings.Contains(line(out, "sign-in: login form"), "the password was not sent") ||
		!strings.Contains(fixOf(out, "sign-in: login form"), "issuer trap") {
		t.Errorf("the refusal must say why:\n%s", out)
	}
}

// The issuer trap seen from the tokens: the identity provider calls itself by
// another name than the one the instance validates against. Its metadata, its
// redirect and its ID token agree with each other — and every API will refuse
// the access token, so none is asked, and that is said once.
func TestTokensFromAnotherIssuerThanTheInstanceValidates(t *testing.T) {
	w := newWorld(t)
	w.metaIssuer = "https://sso.internal.example/realms/zaentrum"
	signedIn(t)
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 1 {
		t.Fatalf("want 1, got %d\n%s", code, out)
	}
	for _, name := range []string{"sign-in: callback", "sign-in: token exchange", "sign-in: id token"} {
		if mark(out, name) != "✓" {
			t.Errorf("%s agrees with the provider's own metadata: %s", name, line(out, name))
		}
	}
	l := line(out, "sign-in: access token")
	if mark(out, "sign-in: access token") != "✗" || !strings.Contains(l, "issuer mismatch") ||
		!strings.Contains(l, "sso.internal.example") || !strings.Contains(l, w.issuer()) {
		t.Fatalf("the access token's issuer and the instance's must both be named: %s\n%s", l, out)
	}
	if !strings.Contains(line(out, "signed-in checks"), "another issuer") || w.called("GET "+itemsPath) != 0 {
		t.Errorf("no API is asked with a token every API refuses:\n%s", out)
	}
}

// RFC 9207: the redirect names the issuer it came from. One that is not the
// issuer asked is an answer from someone else.
func TestARedirectFromAnotherIssuer(t *testing.T) {
	w := newWorld(t)
	w.callbackIss = "https://idp.elsewhere.example/realms/zaentrum"
	signedIn(t)
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 1 || mark(out, "sign-in: callback") != "✗" || !strings.Contains(line(out, "sign-in: callback"), "issuer mismatch") {
		t.Fatalf("want the callback to fail on the issuer, got %d\n%s", code, out)
	}
	if w.called("POST "+realmPath+"/protocol/openid-connect/token") != 0 {
		t.Errorf("a code from another issuer is not exchanged")
	}
}

// A token without chino-api's audience: named once, the chino-api checks say
// they did not run, and the rest of the platform is still checked.
func TestAnAccessTokenWithoutTheAPIsAudience(t *testing.T) {
	w := newWorld(t)
	w.accessAud = []string{"account"}
	signedIn(t)
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 1 {
		t.Fatalf("the web client's tokens are refused by chino-api — a failure: %d\n%s", code, out)
	}
	if mark(out, "sign-in: access token") != "✗" || !strings.Contains(line(out, "sign-in: access token"), `no "chino" audience`) ||
		!strings.Contains(fixOf(out, "sign-in: access token"), "audience mapper") {
		t.Fatalf("the audience must be named, with its fix:\n%s", out)
	}
	if mark(out, "chino-api") != "-" || line(out, "chino-api: items") != "" {
		t.Errorf("the chino-api checks are skipped once, not failed one by one:\n%s", out)
	}
	for _, name := range []string{"portal: launchpad", "app /chino/", "katalog-manager: graphql"} {
		if mark(out, name) != "✓" {
			t.Errorf("%s must still run: %s", name, line(out, name))
		}
	}
}

// The credentials come in pairs.
func TestHalfACredentialIsAFailure(t *testing.T) {
	w := newWorld(t)
	t.Setenv(UserEnv, testUser)
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 1 || mark(out, "sign-in") != "✗" || !strings.Contains(line(out, "sign-in"), PasswordEnv+" is not") {
		t.Fatalf("want a failure naming the missing half, got %d\n%s", code, out)
	}
	noSecrets(t, "half a credential", out)
	if w.called("GET ") != 0 {
		t.Errorf("nothing is asked with half a credential: %v", w.requests)
	}
}

// No credentials at all: the signed-in checks are skipped, in one line that
// says how to run them — and the run still passes.
func TestNoCredentialsSkipsTheSignedInChecks(t *testing.T) {
	w := newWorld(t)
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 0 {
		t.Fatalf("want 0, got %d\n%s", code, out)
	}
	l := line(out, "sign-in")
	if mark(out, "sign-in") != "-" || !strings.Contains(l, UserEnv) || !strings.Contains(l, "zae login --url") {
		t.Fatalf("the skip must say how to run them: %q\n%s", l, out)
	}
	if strings.Count(out, "\n  - ") != 1 {
		t.Errorf("one line, not one per check:\n%s", out)
	}
}

// Without the environment, the session `zae login` stored is used — its
// bearer, no login page. That session belongs to another client, whose
// tokens chino-api may not accept: then that is a skip with the way out, not
// a failure of the platform.
func TestTheStoredSessionIsUsedWithoutAPassword(t *testing.T) {
	w := newWorld(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tok := jwt(map[string]any{"iss": w.issuer(), "aud": []string{"chino", "account"}, "sub": "u1",
		"exp": time.Now().Add(time.Hour).Unix()})
	w.acceptsToken = func(got string) bool { return got == tok }
	if err := creds.Put(w.srv.URL, creds.Entry{Issuer: w.issuer(), ClientID: "zae", AccessToken: tok,
		RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 0 {
		t.Fatalf("want 0, got %d\n%s", code, out)
	}
	if !strings.Contains(line(out, "sign-in"), "the session zae login stored") {
		t.Errorf("the line must say whose session: %s", line(out, "sign-in"))
	}
	if w.called("GET "+realmPath+"/protocol") != 0 || line(out, "sign-in: login form") != "" {
		t.Errorf("a stored session does not walk the login page:\n%s", out)
	}
	if mark(out, "chino-api: items") != "✓" || mark(out, "katalog-manager: graphql") != "✓" {
		t.Errorf("the deep checks run with the stored bearer:\n%s", out)
	}
	if strings.Contains(out, tok) {
		t.Fatal("the bearer was printed")
	}

	// A session whose token is for the CLI's own client only.
	w2 := newWorld(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cli := jwt(map[string]any{"iss": w2.issuer(), "aud": "account", "sub": "u1", "exp": time.Now().Add(time.Hour).Unix()})
	w2.acceptsToken = func(got string) bool { return got == cli }
	_ = creds.Put(w2.srv.URL, creds.Entry{Issuer: w2.issuer(), ClientID: "zae", AccessToken: cli, Expiry: time.Now().Add(time.Hour)})
	code, out, _ = doctor(t, "--url", w2.srv.URL, "--sign-in")
	if code != 0 {
		t.Fatalf("a borrowed token's audience is not the platform's failure: %d\n%s", code, out)
	}
	if mark(out, "sign-in: access token") != "-" || !strings.Contains(fixOf(out, "sign-in: access token"), UserEnv) {
		t.Errorf("the skip must say how to run the chino-api checks:\n%s", out)
	}
	if mark(out, "chino-api") != "-" || mark(out, "portal: launchpad") != "✓" {
		t.Errorf("chino-api is skipped, the rest runs:\n%s", out)
	}
}

// ZAE_TOKEN is the bearer every command sends first, and the doctor follows
// the same order.
func TestZAETokenIsTheBorrowedBearer(t *testing.T) {
	w := newWorld(t)
	tok := jwt(map[string]any{"iss": w.issuer(), "aud": "chino", "exp": time.Now().Add(time.Hour).Unix()})
	w.acceptsToken = func(got string) bool { return got == tok }
	t.Setenv(instance.TokenEnv, tok)
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 0 || !strings.Contains(line(out, "sign-in"), instance.TokenEnv) {
		t.Fatalf("want 0 naming %s, got %d\n%s", instance.TokenEnv, code, out)
	}
}

// The password wins over a stored session: the environment is what the
// operator's Job sets, and it names the account it means.
func TestThePasswordWinsOverAStoredSession(t *testing.T) {
	w := newWorld(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_ = creds.Put(w.srv.URL, creds.Entry{Issuer: w.issuer(), ClientID: "zae", AccessToken: "stored", Expiry: time.Now().Add(time.Hour)})
	signedIn(t)
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 0 || mark(out, "sign-in: login form") != "✓" {
		t.Fatalf("the password sign-in must run, got %d\n%s", code, out)
	}
}

// An instance that says nothing about where to sign in stops there.
func TestAnInstanceThatAdvertisesNoWebClient(t *testing.T) {
	w := newWorld(t)
	w.config = func(rw http.ResponseWriter) {
		writeJSON(rw, map[string]any{"oidcIssuer": w.issuer(), "oidcEnabled": true, "oidcClientId": map[string]string{"tv": "chino-tv"}})
	}
	signedIn(t)
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 1 || mark(out, "chino-api: config") != "✗" || !strings.Contains(line(out, "chino-api: config"), "oidcClientId.web") {
		t.Fatalf("want the config to fail naming the field, got %d\n%s", code, out)
	}
	if !strings.Contains(line(out, "signed-in checks"), "does not say where to sign in") {
		t.Errorf("what did not run is said:\n%s", out)
	}
}
