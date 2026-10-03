package doctor

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zae/internal/instance"
)

// Credentials the tests sign in with. Distinctive on purpose: a substring
// search for them in everything a run printed or wrote is the point.
const (
	testUser     = "verifier-USER-must-never-be-printed"
	testPassword = "PASSWORD-must-never-be-printed"
	realmPath    = "/auth/realms/zaentrum"
	webClient    = "chino-web"
)

// world is one instance on one origin, the way the demo serves it: the
// identity provider under /auth, chino-api under /api, portal-api under
// /api/portal, the catalog console's API under /api/manage, and the apps at
// their mounts. Each part misbehaves on request.
type world struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD /path"
	forms    []url.Values
	tokens   map[string]bool // access tokens issued

	// The identity provider.
	clients       map[string]bool // known client ids
	allowRedirect func(string) bool
	password      string
	requiredFor   string // a required action pending: UPDATE_PASSWORD, VERIFY_PROFILE, …
	otp           bool   // a second factor after the password
	formAction    string // where the login form posts, when not to the provider itself
	formMethod    string // how it posts, when not with POST
	authRedirect  string // where the authorization endpoint redirects instead of answering
	pending       map[string]authRequest
	codes         map[string]authRequest
	metaIssuer    string // the issuer the metadata names, when not the realm's own URL
	idIssuer      string // the issuer the ID token names
	accessIssuer  string
	accessAud     []string
	idNonce       string // a nonce other than the request's
	stateOverride string
	callbackIss   string // the issuer the redirect names, when not the metadata's
	noIDToken     bool

	// The instance.
	config       func(w http.ResponseWriter)
	launchpad    func(w http.ResponseWriter)
	movies       []map[string]any
	series       []map[string]any
	details      map[string]map[string]any
	people       []map[string]any
	persons      map[string]map[string]any
	posters      map[string]bool
	portraits    map[string]bool
	packaged     []string
	peopleTotal  *int            // a total other than the list's length
	gone         map[string]bool // listed as packaged, and the package is not on disk
	masterCaps   string
	items        func(w http.ResponseWriter, r *http.Request) bool // overrides /api/v1/items when it answers true
	graphql      func(w http.ResponseWriter, r *http.Request)
	pages        map[string]string // path → HTML
	assets       map[string]string // path → content type ("" is a 404)
	acceptsToken func(tok string) bool
}

type authRequest struct {
	clientID, redirectURI, state, nonce, challenge string
}

func newWorld(t *testing.T) *world {
	w := &world{
		t:             t,
		tokens:        map[string]bool{},
		clients:       map[string]bool{webClient: true, "zaentrum-web": true},
		allowRedirect: func(string) bool { return true },
		password:      testPassword,
		pending:       map[string]authRequest{},
		codes:         map[string]authRequest{},
		details:       map[string]map[string]any{},
		persons:       map[string]map[string]any{},
		posters:       map[string]bool{},
		gone:          map[string]bool{},
		portraits:     map[string]bool{},
		pages:         map[string]string{},
		assets:        map[string]string{},
	}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	w.withCatalog()
	w.withApps()
	return w
}

func (w *world) issuer() string { return w.srv.URL + realmPath }

// withCatalog is a small catalog: one movie with three credits, one person
// with a portrait, a poster, a packaged title.
func (w *world) withCatalog() {
	w.movies = []map[string]any{{"id": "m1", "type": "movie", "title": "Sintel", "poster_url": "/api/v1/items/m1/poster"}}
	w.series = []map[string]any{}
	w.details["m1"] = map[string]any{"id": "m1", "type": "movie", "title": "Sintel", "poster_url": "/api/v1/items/m1/poster",
		"cast": []map[string]any{
			{"person_id": "p1", "name": "Thom Hoffman", "role": "actor", "character": "Shaman", "order": 0},
			{"person_id": "p2", "name": "Halina Reijn", "role": "actor", "character": "Sintel", "order": 1},
			{"person_id": "p3", "name": "Colin Levy", "role": "director", "job": "Director"},
		}}
	w.people = []map[string]any{{"id": "p1", "name": "Thom Hoffman", "credits": 2, "has_profile": true, "profile_url": "/api/v1/people/p1/profile"}}
	w.persons["p1"] = map[string]any{"id": "p1", "name": "Thom Hoffman", "has_profile": true, "profile_url": "/api/v1/people/p1/profile",
		"items": []map[string]any{{"id": "m1", "title": "Sintel"}}}
	w.posters["m1"] = true
	w.portraits["p1"] = true
	w.packaged = []string{"m1"}
}

// withApps is the portal, the web client and the catalog console, each a
// built single-page app with its bundle.
func (w *world) withApps() {
	for _, mount := range []string{"/portal/", "/chino/", "/katalog/"} {
		w.pages[mount] = fmt.Sprintf(`<!doctype html><html><head><title>app</title>
<script type="module" crossorigin src="%sassets/index-abc.js"></script>
<link rel="stylesheet" crossorigin href="%sassets/index-abc.css"></head><body><div id="root"></div></body></html>`, mount, mount)
		w.assets[mount+"assets/index-abc.js"] = "application/javascript"
		w.assets[mount+"assets/index-abc.css"] = "text/css"
	}
}

// record notes a request; the tests read it to prove the run wrote nothing.
func (w *world) record(r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.requests = append(w.requests, r.Method+" "+r.URL.Path)
}

// writes is every request that was not a read, except the ones a sign-in and
// a GraphQL query make.
func (w *world) writes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, r := range w.requests {
		switch {
		case strings.HasPrefix(r, "GET "):
		case r == "POST "+realmPath+"/login-actions/authenticate",
			r == "POST "+realmPath+"/protocol/openid-connect/token",
			r == "POST "+consoleAPI:
		default:
			out = append(out, r)
		}
	}
	return out
}

func (w *world) called(prefix string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, r := range w.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (w *world) serve(rw http.ResponseWriter, r *http.Request) {
	w.record(r)
	p := r.URL.Path
	switch {
	case p == realmPath+"/.well-known/openid-configuration":
		iss := w.issuer()
		if w.metaIssuer != "" {
			iss = w.metaIssuer
		}
		writeJSON(rw, map[string]any{"issuer": iss,
			"authorization_endpoint":        w.issuer() + "/protocol/openid-connect/auth",
			"token_endpoint":                w.issuer() + "/protocol/openid-connect/token",
			"device_authorization_endpoint": w.issuer() + "/protocol/openid-connect/auth/device"})
	case p == realmPath+"/protocol/openid-connect/auth":
		w.authorize(rw, r)
	case p == realmPath+"/login-actions/authenticate":
		w.authenticate(rw, r)
	case p == realmPath+"/login-actions/required-action":
		w.requiredActionPage(rw, r)
	case p == realmPath+"/protocol/openid-connect/token":
		w.token(rw, r)
	case p == "/api/config":
		if w.config != nil {
			w.config(rw)
			return
		}
		writeJSON(rw, map[string]any{"product": "chino", "apiBase": w.srv.URL + "/api", "oidcIssuer": w.issuer(),
			"oidcAudience": "chino", "oidcEnabled": true,
			"oidcClientId": map[string]string{"web": webClient, "portal": "zaentrum-web", "tv": "chino-tv"}})
	case strings.HasPrefix(p, "/api/"):
		w.api(rw, r)
	default:
		if body, ok := w.pages[p]; ok {
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(rw, body)
			return
		}
		if ct, ok := w.assets[p]; ok && ct != "" {
			rw.Header().Set("Content-Type", ct)
			fmt.Fprint(rw, "/* bundle */ export {};")
			return
		}
		http.NotFound(rw, r)
	}
}

// The identity provider.

const loginPage = `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><title>sign in · zaentrum</title>
<script>var notAForm = "<form action='/elsewhere'>";</script></head>
<body><main class="auth"><div class="card"><h1>sign in</h1>
%s
<form id="kc-form-login" action="%s" method="%s" autocomplete="off" novalidate>
  <label for="username">Username or email</label>
  <input id="username" name="username" type="text" autofocus value="" dir="ltr">
  <label for="password">Password</label>
  <input id="password" name="password" type="password" autocomplete="current-password">
  <input type="hidden" id="id-hidden-input" name="credentialId" value="">
  <label class="remember"><input type="checkbox" name="rememberMe" > Remember me</label>
  <button type="submit" name="login" id="kc-login">sign in</button>
</form>
<div class="links"><a class="link" href="%s">Forgot password?</a></div>
</div></main></body></html>`

// errorPage is the stock theme's error page, which a custom login theme
// leaves to its parent.
const errorPage = `<!DOCTYPE html><html><head><title>Sign in to zaentrum</title></head><body>
<div class="pf-v5-c-login__main"><h1 id="kc-page-title">We are sorry...</h1>
<div id="kc-error-message"><p class="instruction">%s</p></div></div></body></html>`

func (w *world) authorize(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if w.authRedirect != "" {
		http.SetCookie(rw, &http.Cookie{Name: "AUTH_SESSION_ID", Value: "s-outside", Path: "/"})
		http.Redirect(rw, r, w.authRedirect, http.StatusFound)
		return
	}
	if !w.clients[q.Get("client_id")] {
		rw.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(rw, errorPage, "Client not found.")
		return
	}
	if !w.allowRedirect(q.Get("redirect_uri")) {
		rw.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(rw, errorPage, "Invalid parameter: redirect_uri")
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Redirect(rw, r, q.Get("redirect_uri")+"?error=invalid_request&error_description=Missing+parameter%3A+code_challenge_method&state="+q.Get("state"), http.StatusFound)
		return
	}
	if q.Get("response_type") != "code" || q.Get("state") == "" || q.Get("nonce") == "" {
		w.t.Errorf("the authorization request lacks what a browser sends: %v", q)
	}
	session := fmt.Sprintf("s%d", len(w.pending)+1)
	w.mu.Lock()
	w.pending[session] = authRequest{clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"),
		state: q.Get("state"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	w.mu.Unlock()
	// As Keycloak 26 sets it, over plain http too: Secure, SameSite=None.
	http.SetCookie(rw, &http.Cookie{Name: "AUTH_SESSION_ID", Value: session, Path: realmPath + "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteNoneMode})
	w.loginForm(rw, session, "")
}

func (w *world) loginForm(rw http.ResponseWriter, session, alert string) {
	action := w.issuer() + "/login-actions/authenticate?session_code=sc-" + session + "&execution=e1&client_id=" + webClient + "&tab_id=t1"
	if w.formAction != "" {
		action = w.formAction
	}
	if alert != "" {
		alert = `<div class="alert error" role="alert">` + alert + `</div>`
	}
	method := "post"
	if w.formMethod != "" {
		method = w.formMethod
	}
	rw.Header().Set("Content-Type", "text/html;charset=utf-8")
	fmt.Fprintf(rw, loginPage, alert, html.EscapeString(action), method,
		html.EscapeString(realmPath+"/login-actions/reset-credentials?client_id="+webClient+"&tab_id=t1"))
}

func (w *world) authenticate(rw http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("AUTH_SESSION_ID")
	w.mu.Lock()
	req, ok := w.pending[cookieValue(c, err)]
	w.mu.Unlock()
	if !ok || r.URL.Query().Get("session_code") != "sc-"+cookieValue(c, err) {
		rw.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(rw, errorPage, "Cookie not found. Please make sure cookies are enabled in your browser.")
		return
	}
	_ = r.ParseForm()
	w.mu.Lock()
	w.forms = append(w.forms, r.PostForm)
	w.mu.Unlock()
	if r.PostForm.Get("username") != testUser || r.PostForm.Get("password") != w.password {
		w.loginForm(rw, cookieValue(c, err), "Invalid username or password.")
		return
	}
	switch {
	case w.requiredFor != "":
		http.Redirect(rw, r, realmPath+"/login-actions/required-action?execution="+w.requiredFor+"&client_id="+webClient+"&tab_id=t1", http.StatusFound)
		return
	case w.otp:
		rw.Header().Set("Content-Type", "text/html;charset=utf-8")
		fmt.Fprintf(rw, `<html><head><title>Sign in</title></head><body>
<form id="kc-otp-login-form" action="%s" method="post"><input id="otp" name="otp" type="text" autocomplete="off"></form></body></html>`,
			html.EscapeString(w.issuer()+"/login-actions/authenticate?session_code=sc2&execution=e2&client_id="+webClient+"&tab_id=t1"))
		return
	}
	code := "code-" + cookieValue(c, err)
	w.mu.Lock()
	w.codes[code] = req
	w.mu.Unlock()
	state := req.state
	if w.stateOverride != "" {
		state = w.stateOverride
	}
	iss := w.issuer()
	switch {
	case w.callbackIss != "":
		iss = w.callbackIss
	case w.metaIssuer != "":
		iss = w.metaIssuer
	}
	http.Redirect(rw, r, req.redirectURI+"?state="+url.QueryEscape(state)+"&session_state=x&iss="+url.QueryEscape(iss)+"&code="+code, http.StatusFound)
}

// requiredActionPage is the stock theme's page for a pending required action:
// a form of its own, posting to the required-action step.
func (w *world) requiredActionPage(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/html;charset=utf-8")
	action := w.issuer() + "/login-actions/required-action?session_code=sc3&execution=" + r.URL.Query().Get("execution") + "&client_id=" + webClient + "&tab_id=t1"
	fmt.Fprintf(rw, `<!DOCTYPE html><html><head><title>Update password</title></head><body>
<div class="pf-v5-c-alert pf-m-warning"><span class="kc-feedback-text">You need to change your password to activate your account.</span></div>
<form id="kc-passwd-update-form" action="%s" method="post">
<input type="password" id="password-new" name="password-new"><input type="password" id="password-confirm" name="password-confirm">
</form></body></html>`, html.EscapeString(action))
}

func cookieValue(c *http.Cookie, err error) string {
	if err != nil || c == nil {
		return ""
	}
	return c.Value
}

func (w *world) token(rw http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	w.mu.Lock()
	req, ok := w.codes[f.Get("code")]
	delete(w.codes, f.Get("code"))
	w.mu.Unlock()
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	switch {
	case f.Get("grant_type") != "authorization_code" || !ok:
		oauthError(rw, "invalid_grant", "Code not valid")
		return
	case f.Get("redirect_uri") != req.redirectURI || f.Get("client_id") != req.clientID:
		oauthError(rw, "invalid_grant", "Incorrect redirect_uri")
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != req.challenge:
		oauthError(rw, "invalid_grant", "PKCE verification failed: Invalid code verifier")
		return
	}
	exp := time.Now().Add(5 * time.Minute).Unix()
	iss := w.issuer()
	if w.metaIssuer != "" {
		iss = w.metaIssuer
	}
	idIss, accIss := iss, iss
	if w.idIssuer != "" {
		idIss = w.idIssuer
	}
	if w.accessIssuer != "" {
		accIss = w.accessIssuer
	}
	nonce := req.nonce
	if w.idNonce != "" {
		nonce = w.idNonce
	}
	aud := w.accessAud
	if aud == nil {
		aud = []string{"chino", "account"}
	}
	access := jwt(map[string]any{"iss": accIss, "aud": aud, "azp": req.clientID, "sub": "u1", "exp": exp,
		"preferred_username": testUser})
	w.mu.Lock()
	w.tokens[access] = true
	w.mu.Unlock()
	out := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300, "refresh_token": "refresh-1"}
	if !w.noIDToken {
		out["id_token"] = jwt(map[string]any{"iss": idIss, "aud": req.clientID, "azp": req.clientID, "sub": "u1",
			"nonce": nonce, "exp": exp})
	}
	writeJSON(rw, out)
}

func oauthError(rw http.ResponseWriter, code, desc string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(rw).Encode(map[string]string{"error": code, "error_description": desc})
}

func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

// The instance's APIs. Every one wants a token the identity provider issued
// (or one the test accepts), as the real ones do.

func (w *world) authorized(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if w.acceptsToken != nil {
		return w.acceptsToken(tok)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tokens[tok]
}

func (w *world) api(rw http.ResponseWriter, r *http.Request) {
	if !w.authorized(r) {
		http.Error(rw, "invalid token: oidc: expected audience", http.StatusUnauthorized)
		return
	}
	p := r.URL.Path
	switch {
	case p == launchpadPath:
		if w.launchpad != nil {
			w.launchpad(rw)
			return
		}
		writeJSON(rw, map[string]any{"spaces": []map[string]any{
			{"key": "apps", "tiles": []map[string]any{
				{"key": "chino.open", "href": "/chino/"},
				{"key": "tv.open", "href": "/tv/", "disabled": true},
				{"key": "musig.open", "href": "", "disabled": true},
			}},
			{"key": "manage", "tiles": []map[string]any{
				{"key": "katalog.catalog", "href": "/katalog/"},
				{"key": "sample.open", "href": "/portal/app/sample"},
				{"key": "docs", "href": "https://docs.example.org/", "external": true},
				{"key": "handbook", "href": "/handbook/", "external": true},
				{"key": "partner.open", "href": "https://partner.example.org/app/"},
			}},
		}})
	case p == consoleAPI:
		if w.graphql != nil {
			w.graphql(rw, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("__typename")) {
			http.Error(rw, "unexpected query", http.StatusBadRequest)
			return
		}
		writeJSON(rw, map[string]any{"data": map[string]any{"__typename": "Query"}})
	case p == itemsPath:
		if w.items != nil && w.items(rw, r) {
			return
		}
		list := w.movies
		if r.URL.Query().Get("type") == "series" {
			list = w.series
		}
		writeJSON(rw, map[string]any{"product": "chino", "items": list, "source": "katalog"})
	case p == peoplePath:
		total := len(w.people)
		if w.peopleTotal != nil {
			total = *w.peopleTotal
		}
		writeJSON(rw, map[string]any{"people": w.people, "total": total})
	case p == packagedPath:
		writeJSON(rw, map[string]any{"ids": w.packaged})
	case strings.HasPrefix(p, itemsPath+"/"):
		w.item(rw, r, strings.TrimPrefix(p, itemsPath+"/"))
	case strings.HasPrefix(p, peoplePath+"/"):
		rest := strings.TrimPrefix(p, peoplePath+"/")
		if id, ok := strings.CutSuffix(rest, "/profile"); ok {
			image(rw, r, w.portraits[id])
			return
		}
		if d, ok := w.persons[rest]; ok {
			writeJSON(rw, d)
			return
		}
		http.Error(rw, "person not found", http.StatusNotFound)
	default:
		http.NotFound(rw, r)
	}
}

func (w *world) item(rw http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case strings.HasSuffix(rest, "/poster"):
		image(rw, r, w.posters[strings.TrimSuffix(rest, "/poster")])
	case strings.HasSuffix(rest, "/play/master.m3u8"):
		w.mu.Lock()
		w.masterCaps = r.URL.Query().Get("caps")
		w.mu.Unlock()
		packaged := false
		for _, id := range w.packaged {
			packaged = packaged || id == strings.TrimSuffix(rest, "/play/master.m3u8")
		}
		if !packaged || w.gone[strings.TrimSuffix(rest, "/play/master.m3u8")] {
			http.Error(rw, "no such source", http.StatusNotFound)
			return
		}
		rw.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(rw, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nv0/playlist.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=4000\nv1/playlist.m3u8\n")
	default:
		if d, ok := w.details[rest]; ok {
			writeJSON(rw, d)
			return
		}
		http.Error(rw, "not found", http.StatusNotFound)
	}
}

func image(rw http.ResponseWriter, r *http.Request, ok bool) {
	if !ok {
		http.Error(rw, "no artwork", http.StatusNotFound)
		return
	}
	rw.Header().Set("Content-Type", "image/jpeg")
	_, _ = rw.Write(bytes.Repeat([]byte{0xff}, 2048))
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

// Credentials and the clock are ambient: a ZAE_TOKEN or a stored session in
// the shell running `go test` must not change what a test sees, and nothing
// the tests set may leak into another.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zae-doctor-test-config-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Unsetenv(instance.TokenEnv)
	os.Unsetenv(UserEnv)
	os.Unsetenv(PasswordEnv)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// doctor runs `zae doctor` against the world with the static suite stubbed
// out — it asks ghcr.io, and what follows it is what these tests are about.
func doctor(t *testing.T, args ...string) (code int, out, errs string) {
	t.Helper()
	var ob, eb bytes.Buffer
	stdout, stderr = &ob, &eb
	realStatic := staticChecks
	staticChecks = func(*url.URL, string) []Result { return []Result{{Name: "tls", Status: OK, Detail: "stubbed"}} }
	defer func() { stdout, stderr, staticChecks = os.Stdout, os.Stderr, realStatic }()
	code = Run(args, "test")
	return code, ob.String(), eb.String()
}

// signedIn sets the password sign-in's credentials for one test.
func signedIn(t *testing.T) {
	t.Setenv(UserEnv, testUser)
	t.Setenv(PasswordEnv, testPassword)
}

// noSecrets is the rule the password sign-in rests on.
func noSecrets(t *testing.T, where string, texts ...string) {
	t.Helper()
	for _, s := range texts {
		for _, secret := range []string{testPassword, testUser} {
			if strings.Contains(s, secret) {
				t.Fatalf("%s: a credential reached the output: %q in %q", where, secret, s)
			}
		}
	}
}

// line finds the output line of one check — "✓ name   detail" — by its
// name exactly: "sign-in" is not "sign-in: issuer".
func line(out, name string) string {
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		m, rest, ok := strings.Cut(t, " ")
		if !ok || len([]rune(m)) != 1 {
			continue
		}
		if rest == name || strings.HasPrefix(rest, name+" ") {
			return t
		}
	}
	return ""
}

// fixOf is the fix printed under a check, "" when it has none.
func fixOf(out, name string) string {
	want := line(out, name)
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if want != "" && strings.TrimSpace(l) == want && i+1 < len(lines) {
			if f := strings.TrimSpace(lines[i+1]); strings.HasPrefix(f, "fix: ") {
				return strings.TrimPrefix(f, "fix: ")
			}
			return ""
		}
	}
	return ""
}

// mark is a check's mark: ✓, !, ✗ or -, "" when the check is not there.
func mark(out, name string) string {
	if l := line(out, name); l != "" {
		return strings.Fields(l)[0]
	}
	return ""
}
