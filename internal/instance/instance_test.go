package instance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/creds"
)

func TestBase(t *testing.T) {
	for raw, want := range map[string]string{
		"https://media.example.org":    "https://media.example.org",
		"https://media.example.org/":   "https://media.example.org",
		"http://127.0.0.1:8080//":      "http://127.0.0.1:8080",
		"https://example.org/zaentrum": "https://example.org/zaentrum",
	} {
		if got, err := Base(raw); err != nil || got != want {
			t.Errorf("Base(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "media.example.org", "ftp://example.org", "https://"} {
		if _, err := Base(raw); err == nil {
			t.Errorf("Base(%q) must be refused", raw)
		}
	}
}

// isolate points the credentials file at a temporary directory, so a test
// never reads — or writes — the developer's own sessions.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(TokenEnv, "")
	return dir
}

// jwt builds an unsigned-looking JWT: zae only ever reads the payload, and
// the instance is what verifies it.
func jwt(sub, user string, roles []string, exp time.Time) string {
	payload, _ := json.Marshal(map[string]any{
		"sub": sub, "preferred_username": user, "exp": exp.Unix(),
		"realm_access": map[string]any{"roles": roles},
	})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".signature-not-checked-here"
}

// The hint must say what to DO, and what to do depends on what zae sent:
// nothing, an environment bearer, or a stored session that is signed in and
// still refused — which is a missing ROLE and not a missing login.
func TestForbiddenHint(t *testing.T) {
	isolate(t)
	const base = "https://media.example.org"

	h := ForbiddenHint(base, "needs admin")
	if !strings.Contains(h, "supply a bearer via "+TokenEnv) || !strings.Contains(h, "zae login --url "+base) {
		t.Fatalf("no credentials: %q", h)
	}

	t.Setenv(TokenEnv, "tok")
	if h := ForbiddenHint(base, "needs admin"); !strings.Contains(h, "was refused — needs admin") {
		t.Fatalf("refused env token: %q", h)
	}
	req, _ := http.NewRequest(http.MethodGet, base, nil)
	if err := Authorize(context.Background(), req, base); err != nil {
		t.Fatalf("authorize with %s: %v", TokenEnv, err)
	}
	if req.Header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("bearer not attached: %q", req.Header.Get("Authorization"))
	}

	t.Setenv(TokenEnv, "")
	if err := creds.Put(base, creds.Entry{AccessToken: "stored", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if h := ForbiddenHint(base, "needs admin"); !strings.Contains(h, "missing role, not a missing login") {
		t.Fatalf("refused stored session: %q", h)
	}
}

// The order is the contract: ZAE_TOKEN first, so a script that sets it keeps
// the identity it named even on a machine where someone is logged in.
func TestEnvTokenWinsOverStoredSession(t *testing.T) {
	isolate(t)
	const base = "https://media.example.org"
	if err := creds.Put(base, creds.Entry{AccessToken: "stored", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(TokenEnv, "from-the-environment")
	got, err := Token(context.Background(), base)
	if err != nil || got != "from-the-environment" {
		t.Fatalf("want the environment's bearer, got %q (%v)", got, err)
	}
}

// No credentials at all is not an error: the call goes out unauthenticated
// and the instance's own answer decides, which is how zae learns that an
// endpoint is public.
func TestNoCredentialsSendsNoBearer(t *testing.T) {
	isolate(t)
	req, _ := http.NewRequest(http.MethodGet, "https://media.example.org", nil)
	if err := Authorize(context.Background(), req, "https://media.example.org"); err != nil {
		t.Fatalf("no credentials must not be an error: %v", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatalf("a bearer appeared from nowhere: %q", req.Header.Get("Authorization"))
	}
}

// fakeIssuer serves metadata and a token endpoint whose answer the test picks.
func fakeIssuer(t *testing.T, token http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"issuer":%q,"device_authorization_endpoint":%q,"token_endpoint":%q}`,
			srv.URL, srv.URL+"/device", srv.URL+"/token")
	})
	mux.HandleFunc("/token", token)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// An expired access token is renewed with the refresh token, and the new one
// is written back — so one expiry costs one extra round trip, not one per
// command.
func TestExpiredSessionIsRefreshedAndWrittenBack(t *testing.T) {
	isolate(t)
	const base = "https://media.example.org"
	fresh := jwt("user-1", "ada", []string{AdminRole}, time.Now().Add(time.Hour))
	var sent url.Values
	iss := fakeIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sent = r.PostForm
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"refresh-2","expires_in":300,"token_type":"Bearer"}`, fresh)
	})
	if err := creds.Put(base, creds.Entry{
		Issuer: iss.URL, ClientID: "zae",
		AccessToken: "stale", RefreshToken: "refresh-1",
		Expiry: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := Token(context.Background(), base)
	if err != nil || got != fresh {
		t.Fatalf("want the renewed token, got %q (%v)", got, err)
	}
	if sent.Get("grant_type") != "refresh_token" || sent.Get("refresh_token") != "refresh-1" || sent.Get("client_id") != "zae" {
		t.Fatalf("refresh request was not the RFC 6749 one: %v", sent)
	}
	e, ok, err := creds.Get(base)
	if err != nil || !ok {
		t.Fatalf("the entry vanished: %v", err)
	}
	if e.AccessToken != fresh || e.RefreshToken != "refresh-2" || !e.Expiry.After(time.Now()) {
		t.Fatalf("the renewal was not written back: %+v", e)
	}
	if e.Username != "ada" || e.Subject != "user-1" {
		t.Fatalf("the renewed identity was not recorded: %+v", e)
	}
	// And the next command uses the stored one without asking the issuer again.
	iss.Close()
	if got, err := Token(context.Background(), base); err != nil || got != fresh {
		t.Fatalf("second call must not need the issuer: %q (%v)", got, err)
	}
}

// A refresh token the provider no longer accepts is a session that ended.
// Say so in the words that fix it — and do not send the dead token.
func TestRefreshFailureSaysSessionExpired(t *testing.T) {
	isolate(t)
	const base = "https://media.example.org"
	iss := fakeIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"Token is not active"}`)
	})
	if err := creds.Put(base, creds.Entry{
		Issuer: iss.URL, ClientID: "zae",
		AccessToken: "stale", RefreshToken: "refresh-1",
		Expiry: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, base, nil)
	err := Authorize(context.Background(), req, base)
	if err == nil {
		t.Fatal("a dead session must be an error, not a silent anonymous call")
	}
	if !strings.Contains(err.Error(), "session expired") || !strings.Contains(err.Error(), "zae login --url "+base) {
		t.Fatalf("the message must say what to do: %q", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatalf("an expired token was attached anyway: %q", req.Header.Get("Authorization"))
	}
	// The cause travels with it: "sign in again" is right, but a person whose
	// second login fails too needs to know why the first one died.
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("the provider's reason was swallowed: %q", err)
	}
}

// An entry without a refresh token cannot be renewed: same answer, no round
// trip to an issuer that has nothing to offer.
func TestExpiredWithoutRefreshTokenIsExpired(t *testing.T) {
	isolate(t)
	const base = "https://media.example.org"
	if err := creds.Put(base, creds.Entry{AccessToken: "stale", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := Token(context.Background(), base); err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Fatalf("want a session-expired error, got %v", err)
	}
}

// Sessions are per instance: a bearer for one is worthless at another, and
// sending it there would leak it to a host the person never signed in to.
func TestSessionsAreScopedToTheirInstance(t *testing.T) {
	isolate(t)
	if err := creds.Put("https://one.example.org", creds.Entry{AccessToken: "one", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := Token(context.Background(), "https://two.example.org")
	if err != nil || got != "" {
		t.Fatalf("another instance's session must not travel: %q (%v)", got, err)
	}
	if got, err := Token(context.Background(), "https://one.example.org/"); err != nil || got != "one" {
		t.Fatalf("a trailing slash must not split the session: %q (%v)", got, err)
	}
}

// The file holds bearer tokens: 0600 in a 0700 directory, every write.
func TestCredentialFilePermissions(t *testing.T) {
	dir := isolate(t)
	const base = "https://media.example.org"
	if err := creds.Put(base, creds.Entry{AccessToken: "secret", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	path, err := creds.Path()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, creds.Dir, creds.Name); path != want {
		t.Fatalf("XDG_CONFIG_HOME ignored: %q, want %q", path, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file is %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("credentials directory is %o, want 700", di.Mode().Perm())
	}

	// A directory someone widened is narrowed again on the next write.
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := creds.Put(base, creds.Entry{AccessToken: "secret-2", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	di, _ = os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("a widened directory stayed %o", di.Mode().Perm())
	}
}
