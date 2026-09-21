package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/capability"
	"github.com/zaentrum/zae/internal/creds"
	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
	"github.com/zaentrum/zae/internal/oidc"
)

// Secrets the tests assert never appear in output. Distinctive on purpose:
// a substring search for them is the whole point.
const (
	accessToken  = "ACCESS-TOKEN-must-never-be-printed"
	refreshToken = "REFRESH-TOKEN-must-never-be-printed"
	deviceCode   = "DEVICE-CODE-must-never-be-printed"
	userCode     = "WDJB-MJHT" // meant to be read aloud; this one IS printed
)

// realmPrefix proves zae reads endpoints instead of building them: every
// endpoint lives under a path the well-known document alone reveals.
const realmPrefix = "/auth/realms/zaentrum"

// idp is a fake identity provider that records what the CLI sent it and
// answers /token from a script the test writes.
type idp struct {
	mu sync.Mutex

	// replies are handed out one per poll; the last one repeats.
	replies []string
	// deviceReply overrides the device authorization answer.
	deviceReply  string
	deviceStatus int
	// noDeviceEndpoint drops device_authorization_endpoint from the metadata.
	noDeviceEndpoint bool

	polls      int
	deviceForm url.Values
	tokenForm  url.Values

	srv *httptest.Server
}

func newIDP(t *testing.T, replies ...string) *idp {
	t.Helper()
	p := &idp{replies: replies}
	mux := http.NewServeMux()
	mux.HandleFunc(realmPrefix+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		device := fmt.Sprintf(`"device_authorization_endpoint":%q,`, p.srv.URL+realmPrefix+"/device")
		if p.noDeviceEndpoint {
			device = ""
		}
		fmt.Fprintf(w, `{"issuer":%q,%s"token_endpoint":%q}`,
			p.srv.URL+realmPrefix, device, p.srv.URL+realmPrefix+"/token")
	})
	mux.HandleFunc(realmPrefix+"/device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		p.deviceForm = r.PostForm
		reply, status := p.deviceReply, p.deviceStatus
		p.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
		if reply != "" {
			fmt.Fprint(w, reply)
			return
		}
		fmt.Fprintf(w, `{"device_code":%q,"user_code":%q,
			"verification_uri":%q,"verification_uri_complete":%q,
			"expires_in":600,"interval":5}`,
			deviceCode, userCode,
			p.srv.URL+realmPrefix+"/device/verify",
			p.srv.URL+realmPrefix+"/device/verify?user_code="+userCode)
	})
	mux.HandleFunc(realmPrefix+"/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		p.tokenForm = r.PostForm
		i := p.polls
		p.polls++
		if i >= len(p.replies) {
			i = len(p.replies) - 1
		}
		reply := p.replies[i]
		p.mu.Unlock()
		if strings.Contains(reply, `"error"`) {
			w.WriteHeader(http.StatusBadRequest)
		}
		fmt.Fprint(w, reply)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) issuer() string { return p.srv.URL + realmPrefix }

// granted is a successful token answer carrying a readable JWT.
func granted(roles ...string) string {
	payload, _ := json.Marshal(map[string]any{
		"sub": "user-1", "preferred_username": "ada",
		"exp":          time.Now().Add(5 * time.Minute).Unix(),
		"realm_access": map[string]any{"roles": roles},
	})
	jwt := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	return fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":300,"token_type":"Bearer"}`,
		jwt+"#"+accessToken, refreshToken)
}

const (
	pending = `{"error":"authorization_pending","error_description":"The authorization request is still pending"}`
	slowest = `{"error":"slow_down","error_description":"Slow down"}`
	denied  = `{"error":"access_denied","error_description":"The end user denied the authorization request"}`
	expired = `{"error":"expired_token","error_description":"Device code is expired"}`
)

// fakeInstance serves the discovery document, with or without `auth`.
func fakeInstance(t *testing.T, auth *capability.Auth) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != capability.Path {
			http.NotFound(w, r)
			return
		}
		doc := map[string]any{"capabilityVersion": 1, "services": []any{}}
		if auth != nil {
			doc["auth"] = auth
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// harness isolates the credentials file, silences the browser, and paces
// polling instantly while recording what the real intervals would have been.
type harness struct {
	opened []string
	slept  []time.Duration
	mu     sync.Mutex
}

func setup(t *testing.T) *harness {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(instance.TokenEnv, "")
	h := &harness{}

	realOpen, realClient := openBrowser, newClient
	openBrowser = func(u string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.opened = append(h.opened, u)
	}
	newClient = func() *oidc.Client {
		return &oidc.Client{Sleep: func(ctx context.Context, d time.Duration) error {
			h.mu.Lock()
			h.slept = append(h.slept, d)
			h.mu.Unlock()
			return ctx.Err()
		}}
	}
	t.Cleanup(func() { openBrowser, newClient = realOpen, realClient })
	return h
}

// run captures what a command printed, both streams.
func run(t *testing.T, fn func() int) (code int, out, errs string) {
	t.Helper()
	var ob, eb bytes.Buffer
	stdout, stderr = &ob, &eb
	defer func() { stdout, stderr = os.Stdout, os.Stderr }()
	code = fn()
	return code, ob.String(), eb.String()
}

// noSecrets is the rule the whole feature rests on: a credential must never
// reach a terminal, a scrollback buffer, a CI log or a pasted bug report.
func noSecrets(t *testing.T, where string, streams ...string) {
	t.Helper()
	for _, s := range streams {
		for _, secret := range []string{accessToken, refreshToken, deviceCode} {
			if strings.Contains(s, secret) {
				t.Fatalf("%s: a credential reached the terminal: %q appears in %q", where, secret, s)
			}
		}
	}
}

func TestLoginHappyPath(t *testing.T) {
	h := setup(t)
	p := newIDP(t, granted(instance.AdminRole))
	srv := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})

	code, out, errs := run(t, func() int { return Login([]string{"--url", srv.URL}) })
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d: %s%s", code, out, errs)
	}
	noSecrets(t, "login", out, errs)

	// What a person needs is on the screen: where to go, and the code to check.
	if !strings.Contains(out, userCode) || !strings.Contains(out, p.srv.URL+realmPrefix+"/device/verify") {
		t.Fatalf("the verification URL and user code must be printed: %q", out)
	}
	if !strings.Contains(out, p.issuer()) {
		t.Fatalf("the identity provider must be named before a browser opens: %q", out)
	}
	// verification_uri_complete is preferred: it carries the code already.
	if len(h.opened) != 1 || !strings.Contains(h.opened[0], "user_code="+userCode) {
		t.Fatalf("want the complete verification URI opened, got %v", h.opened)
	}

	// PKCE, on the DEVICE request: an identity provider whose client requires
	// S256 refuses anything else.
	form := p.deviceForm
	if form.Get("client_id") != "zae" || form.Get("scope") != "openid" {
		t.Fatalf("device request: %v", form)
	}
	if form.Get("code_challenge_method") != "S256" {
		t.Fatalf("device request carried no S256 challenge method: %v", form)
	}
	verifier := p.tokenForm.Get("code_verifier")
	if len(verifier) < 43 {
		t.Fatalf("PKCE verifier must be at least 43 characters, got %d (%q)", len(verifier), verifier)
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); form.Get("code_challenge") != want {
		t.Fatalf("challenge is not S256(verifier): %q vs %q", form.Get("code_challenge"), want)
	}
	if strings.ContainsAny(form.Get("code_challenge"), "=+/") {
		t.Fatalf("challenge must be unpadded base64url: %q", form.Get("code_challenge"))
	}
	if got := p.tokenForm.Get("grant_type"); got != oidc.DeviceGrant {
		t.Fatalf("token request grant_type = %q", got)
	}
	if p.tokenForm.Get("device_code") != deviceCode || p.tokenForm.Get("client_id") != "zae" {
		t.Fatalf("token request: %v", p.tokenForm)
	}

	// Stored, per instance, with everything a renewal needs.
	e, ok, err := creds.Get(srv.URL)
	if err != nil || !ok {
		t.Fatalf("nothing was stored: %v", err)
	}
	if !strings.HasSuffix(e.AccessToken, accessToken) || e.RefreshToken != refreshToken {
		t.Fatalf("stored entry lost its tokens: %+v", e)
	}
	if e.Issuer != p.issuer() || e.ClientID != "zae" || e.Username != "ada" || e.Subject != "user-1" {
		t.Fatalf("stored entry: %+v", e)
	}
	if e.Expiry.IsZero() || !e.Expiry.After(time.Now()) {
		t.Fatalf("stored entry has no usable expiry: %+v", e)
	}
	if !strings.Contains(out, "admin role") || !strings.Contains(out, "yes") {
		t.Fatalf("login must say whether the admin role is there: %q", out)
	}

	// And a fresh PKCE verifier per login — never the same one twice.
	first := verifier
	_, _, _ = run(t, func() int { return Login([]string{"--url", srv.URL}) })
	if p.tokenForm.Get("code_verifier") == first {
		t.Fatal("the PKCE verifier was reused across logins")
	}
}

// The common case: nobody has approved yet, then the provider asks for a
// slower poll, then it works. slow_down must actually slow zae down.
func TestLoginPendingThenSlowDownThenSuccess(t *testing.T) {
	h := setup(t)
	p := newIDP(t, pending, slowest, granted(instance.AdminRole))
	srv := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})

	code, out, errs := run(t, func() int { return Login([]string{"--url", srv.URL}) })
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d: %s%s", code, out, errs)
	}
	noSecrets(t, "slow_down", out, errs)
	if p.polls != 3 {
		t.Fatalf("want 3 polls, got %d", p.polls)
	}
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}
	if len(h.slept) != len(want) {
		t.Fatalf("want %d waits, got %v", len(want), h.slept)
	}
	for i := range want {
		if h.slept[i] != want[i] {
			// RFC 8628 §3.5: slow_down adds five seconds, and the new interval
			// is kept for every poll after it.
			t.Fatalf("wait %d = %s, want %s (all: %v)", i, h.slept[i], want[i], h.slept)
		}
	}
}

func TestLoginDeniedIsForbidden(t *testing.T) {
	setup(t)
	p := newIDP(t, pending, denied)
	srv := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})

	code, out, errs := run(t, func() int { return Login([]string{"--url", srv.URL}) })
	if code != exitcode.Forbidden {
		t.Fatalf("a refused sign-in is exit 5, got %d: %s", code, errs)
	}
	noSecrets(t, "denied", out, errs)
	if _, ok, _ := creds.Get(srv.URL); ok {
		t.Fatal("a refused login must store nothing")
	}
}

func TestLoginExpiredCodeIsFailed(t *testing.T) {
	setup(t)
	p := newIDP(t, expired)
	srv := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})

	code, out, errs := run(t, func() int { return Login([]string{"--url", srv.URL}) })
	if code != exitcode.Failed || !strings.Contains(errs, "expired") {
		t.Fatalf("want 1 saying the code expired, got %d: %s", code, errs)
	}
	noSecrets(t, "expired", out, errs)
	if _, ok, _ := creds.Get(srv.URL); ok {
		t.Fatal("an expired login must store nothing")
	}
}

// The one failure an operator can act on gets its own exit code and names
// what to enable — rather than reading as a network problem.
func TestLoginWithoutDeviceEndpointSaysWhatToEnable(t *testing.T) {
	setup(t)
	p := newIDP(t, granted())
	p.noDeviceEndpoint = true
	srv := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})

	code, _, errs := run(t, func() int { return Login([]string{"--url", srv.URL}) })
	if code != exitcode.NotOffered {
		t.Fatalf("want 3, got %d: %s", code, errs)
	}
	for _, want := range []string{"device_authorization_endpoint", "Device Authorization Grant"} {
		if !strings.Contains(errs, want) {
			t.Fatalf("the message must name %q: %s", want, errs)
		}
	}
}

// An instance that does not advertise `auth` is an older instance, not a
// broken one: say so, and take the flags instead.
func TestLoginFallsBackToFlagsOnAnOlderInstance(t *testing.T) {
	setup(t)
	p := newIDP(t, granted(instance.AdminRole))
	srv := fakeInstance(t, nil)

	code, _, errs := run(t, func() int { return Login([]string{"--url", srv.URL}) })
	if code != exitcode.NotOffered {
		t.Fatalf("want 3, got %d: %s", code, errs)
	}
	for _, want := range []string{"--issuer", "--client-id", "does not advertise"} {
		if !strings.Contains(errs, want) {
			t.Fatalf("the message must name %q: %s", want, errs)
		}
	}

	code, out, errs := run(t, func() int {
		return Login([]string{"--url", srv.URL, "--issuer", p.issuer(), "--client-id", "zae-cli", "--no-browser"})
	})
	if code != exitcode.OK {
		t.Fatalf("with both flags it must work: %d %s%s", code, out, errs)
	}
	if p.deviceForm.Get("client_id") != "zae-cli" {
		t.Fatalf("--client-id ignored: %v", p.deviceForm)
	}
	// An instance that predates discovery entirely: same answer, same words.
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	code, _, errs = run(t, func() int { return Login([]string{"--url", old.URL}) })
	if code != exitcode.NotOffered || !strings.Contains(errs, "--issuer") {
		t.Fatalf("want 3 naming the flags, got %d: %s", code, errs)
	}
}

// An instance that cannot be reached is undetermined — never "cannot be
// signed in to".
func TestLoginUnreachableInstanceIsUndetermined(t *testing.T) {
	setup(t)
	code, _, errs := run(t, func() int { return Login([]string{"--url", "http://127.0.0.1:1"}) })
	if code != exitcode.Undetermined || !strings.Contains(errs, "not concluding") {
		t.Fatalf("want 4, got %d: %s", code, errs)
	}
}

func TestLoginNoBrowserOpensNothing(t *testing.T) {
	h := setup(t)
	p := newIDP(t, granted())
	srv := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})

	code, out, _ := run(t, func() int { return Login([]string{"--url", srv.URL, "--no-browser"}) })
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d", code)
	}
	if len(h.opened) != 0 {
		t.Fatalf("--no-browser still opened %v", h.opened)
	}
	if !strings.Contains(out, userCode) {
		t.Fatalf("the code must still be printed: %q", out)
	}
}

// A token without the admin role is a successful login that will still be
// refused by admin commands. Say it at login, not three commands later.
func TestLoginReportsAMissingAdminRole(t *testing.T) {
	setup(t)
	p := newIDP(t, granted("user"))
	srv := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})

	code, out, _ := run(t, func() int { return Login([]string{"--url", srv.URL, "--no-browser"}) })
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d", code)
	}
	if !strings.Contains(out, "no — the token does not carry") || !strings.Contains(out, instance.AdminRole) {
		t.Fatalf("a missing admin role must be stated at login: %q", out)
	}
}

func TestLogout(t *testing.T) {
	setup(t)
	p := newIDP(t, granted())
	a := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})
	b := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})
	for _, srv := range []*httptest.Server{a, b} {
		if code, _, errs := run(t, func() int { return Login([]string{"--url", srv.URL, "--no-browser"}) }); code != 0 {
			t.Fatalf("setup login failed: %d %s", code, errs)
		}
	}

	code, out, _ := run(t, func() int { return Logout([]string{"--url", a.URL}) })
	if code != exitcode.OK || !strings.Contains(out, "removed") {
		t.Fatalf("logout: %d %q", code, out)
	}
	if _, ok, _ := creds.Get(a.URL); ok {
		t.Fatal("the entry survived logout")
	}
	if _, ok, _ := creds.Get(b.URL); !ok {
		t.Fatal("logout removed another instance's session")
	}

	// Removing what is not there is the end state asked for, not an error.
	if code, _, _ := run(t, func() int { return Logout([]string{"--url", a.URL}) }); code != exitcode.OK {
		t.Fatalf("a second logout must be 0, got %d", code)
	}

	code, out, _ = run(t, func() int { return Logout([]string{"--all"}) })
	if code != exitcode.OK || !strings.Contains(out, "1 stored session") {
		t.Fatalf("logout --all: %d %q", code, out)
	}
	path, _ := creds.Path()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("logout --all must leave no file: %v", err)
	}
	if code, _, _ := run(t, func() int { return Logout([]string{"--url", a.URL, "--all"}) }); code != exitcode.Usage {
		t.Fatal("--url with --all is a usage error")
	}
}

func TestWhoami(t *testing.T) {
	setup(t)
	p := newIDP(t, granted(instance.AdminRole))
	srv := fakeInstance(t, &capability.Auth{Issuer: p.issuer(), ClientID: "zae"})

	// Before signing in: exit 5, and the message says how to fix it.
	code, _, errs := run(t, func() int { return Whoami([]string{"--url", srv.URL}) })
	if code != exitcode.Forbidden || !strings.Contains(errs, "zae login --url") {
		t.Fatalf("want 5 naming login, got %d: %s", code, errs)
	}

	if code, _, errs := run(t, func() int { return Login([]string{"--url", srv.URL, "--no-browser"}) }); code != 0 {
		t.Fatalf("setup login failed: %d %s", code, errs)
	}
	code, out, errs := run(t, func() int { return Whoami([]string{"--url", srv.URL}) })
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d: %s", code, errs)
	}
	noSecrets(t, "whoami", out, errs)
	for _, want := range []string{"user-1", "ada", "admin role", "yes", p.issuer()} {
		if !strings.Contains(out, want) {
			t.Fatalf("whoami must print %q: %q", want, out)
		}
	}

	// A bearer from the environment is what would be sent, so it is what
	// whoami reports on — and it says which one won.
	t.Setenv(instance.TokenEnv, "opaque-service-account-token")
	code, out, errs = run(t, func() int { return Whoami([]string{"--url", srv.URL}) })
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d: %s", code, errs)
	}
	if !strings.Contains(out, instance.TokenEnv) || !strings.Contains(out, "wins") {
		t.Fatalf("whoami must say which bearer would be sent: %q", out)
	}
	if !strings.Contains(out, "not a JWT") {
		t.Fatalf("an opaque token must be reported as unreadable, not guessed at: %q", out)
	}
	if strings.Contains(out, "opaque-service-account-token") {
		t.Fatalf("whoami printed the token: %q", out)
	}
}

// --url is the one thing zae cannot guess: a session belongs to an instance.
func TestCommandsRequireAnInstance(t *testing.T) {
	setup(t)
	for name, fn := range map[string]func([]string) int{"login": Login, "logout": Logout, "whoami": Whoami} {
		if code, _, _ := run(t, func() int { return fn(nil) }); code != exitcode.Usage {
			t.Fatalf("%s without --url must be a usage error, got %d", name, code)
		}
	}
}
