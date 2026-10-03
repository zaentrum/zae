package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/instance"
	"github.com/zaentrum/zae/internal/oidc"
)

// The signed-in checks, opt-in with --sign-in: the doctor signs in the way a
// person does, then uses the platform the way its web client does — reading,
// never writing.
//
// They exist so that a platform can verify ITSELF after an update without a
// password leaving it: the operator runs this binary in the platform's own
// namespace, as a test account it created, and records the outcome. Nothing
// in a cluster stands where a user stands, so the checks take the user's path
// on purpose — the web client's client id, the login page, the redirect back,
// the APIs the pages call — and each step is its own result, with what to do
// when it is the one that broke.

// The password sign-in reads its credentials from the environment, never from
// flags: a flag is visible to every local user in the process list, and kept
// in shell history.
const (
	UserEnv     = "ZAE_DOCTOR_USER"
	PasswordEnv = "ZAE_DOCTOR_PASSWORD"
)

const (
	// configPath is the instance's public self-description: the issuer every
	// client signs in at and the client id each kind of client uses.
	configPath = "/api/config"
	// callbackPath is where the web client returns after a sign-in. It is the
	// site root's, not under the client's mount: the web client builds it that
	// way, and registers it so.
	callbackPath = "/auth/callback"
	// webScope is what the web client asks for; signing in as it does means
	// asking for the same.
	webScope = "openid profile email"
	// maxHops bounds the redirects a sign-in follows inside the identity
	// provider.
	maxHops = 8
)

var (
	// requestTimeout bounds every request: a hung check reads as a hung
	// platform, so none may hang. A variable so a test of a hang is short.
	requestTimeout = 10 * time.Second
	// now is a variable so a test can move the clock.
	now = time.Now
)

// instanceConfig is GET /api/config, the part the doctor reads.
type instanceConfig struct {
	Issuer   string            `json:"oidcIssuer"`
	Audience string            `json:"oidcAudience"`
	Enabled  *bool             `json:"oidcEnabled"`
	ClientID map[string]string `json:"oidcClientId"`
}

// session is one signed-in run: the instance, what it says about itself, the
// token, and the HTTP every check shares — short timeouts, and redirects
// followed only where a check follows them itself.
type session struct {
	base  *url.URL
	agent string
	cfg   instanceConfig
	token string
	http  *http.Client
	// noAPIs and noChino, when set, say why the token's own claims show that
	// every API, or chino-api, will refuse it: the checks that need them then
	// say so once, instead of failing one by one for a reason already named.
	noAPIs, noChino string
}

func newSession(base *url.URL, version string) *session {
	return &session{base: base, agent: "zae/" + version + " (doctor)", http: &http.Client{
		Timeout:       requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// signedInChecks runs the whole signed-in suite.
//
// The credentials never reach a result, and not by filtering: nothing here
// formats either of them into text, and nothing an identity provider sends
// back is printed but the message it shows a person — which Keycloak never
// fills with what was typed. A filter would do worse than nothing: cutting a
// username like "demo" out of an address shows exactly where it was.
func signedInChecks(ctx context.Context, base *url.URL, version string) []Result {
	return newSession(base, version).run(ctx)
}

// notRun is the one line that stands for every check a failure above made
// impossible to run.
func notRun(why string) Result {
	return Result{Name: "signed-in checks", Status: Skip, Detail: "not run — " + why}
}

func (s *session) run(ctx context.Context) []Result {
	user, pass := strings.TrimSpace(os.Getenv(UserEnv)), os.Getenv(PasswordEnv)
	var out []Result
	password := false
	switch {
	case user != "" && pass != "":
		password = true
		out = append(out, Result{Name: "sign-in", Status: OK,
			Detail: "with a password through the login page, as a person does (" + UserEnv + ", " + PasswordEnv + ")"})
	case user != "" || pass != "":
		set, unset := UserEnv, PasswordEnv
		if user == "" {
			set, unset = PasswordEnv, UserEnv
		}
		return []Result{{Name: "sign-in", Status: Fail, Detail: set + " is set and " + unset + " is not",
			Fix: "set both for a password sign-in — or neither, to use the session zae login stored"}}
	default:
		r, ok := s.borrow(ctx)
		out = append(out, r)
		if !ok {
			return out
		}
	}

	r, ok := s.readConfig(ctx)
	out = append(out, r)
	switch {
	case !ok && r.Status == Skip:
		return out // sign-in is off on this instance, and the line says so
	case !ok:
		return append(out, notRun("the instance does not say where to sign in"))
	}
	if password {
		steps, ok := s.passwordSignIn(ctx, user, pass)
		out = append(out, steps...)
		if !ok {
			return append(out, notRun("the sign-in did not complete"))
		}
	}
	out = append(out, s.checkAccessToken(password))
	if s.noAPIs != "" {
		return append(out, notRun(s.noAPIs))
	}
	return append(out, s.deepChecks(ctx)...)
}

// borrow takes the bearer every other command would send — ZAE_TOKEN, else
// the session `zae login` stored for this instance, renewed when it has
// expired. Not having one is not a failure of the platform: the signed-in
// checks are skipped, and the line says how to run them.
func (s *session) borrow(ctx context.Context) (Result, bool) {
	tok, err := instance.Token(ctx, s.base.String())
	switch {
	case err != nil:
		return Result{Name: "sign-in", Status: Skip, Detail: "the stored session cannot be used: " + err.Error(),
			Fix: "run zae login --url " + s.base.String() + ", or set " + UserEnv + " and " + PasswordEnv}, false
	case tok == "":
		return Result{Name: "sign-in", Status: Skip,
			Detail: "no credentials, so the signed-in checks are skipped — set " + UserEnv + " and " + PasswordEnv +
				" for a password sign-in, or sign in with zae login --url " + s.base.String()}, false
	}
	s.token = tok
	how := "with the session zae login stored for this instance"
	if strings.TrimSpace(os.Getenv(instance.TokenEnv)) != "" {
		how = "with the bearer in " + instance.TokenEnv
	}
	return Result{Name: "sign-in", Status: OK, Detail: how + " — the login page is not exercised"}, true
}

// readConfig is GET /api/config: the issuer and the web client's id.
func (s *session) readConfig(ctx context.Context) (Result, bool) {
	const name = "chino-api: config"
	a, err := s.get(ctx, configPath, "application/json", false, 1<<20)
	if err != nil || a.status != http.StatusOK {
		r := apiFailure(name, configPath, a, err)
		r.Fix = "every client learns where to sign in from " + configPath + " — " + r.Fix
		return r, false
	}
	if err := json.Unmarshal(a.body, &s.cfg); err != nil {
		return Result{Name: name, Status: Fail, Detail: configPath + " answered something that is not JSON (" + a.ctype + ")",
			Fix: "the route for /api must reach chino-api"}, false
	}
	web := strings.TrimSpace(s.cfg.ClientID["web"])
	switch {
	case s.cfg.Enabled != nil && !*s.cfg.Enabled:
		return Result{Name: name, Status: Skip, Detail: "sign-in is disabled on this instance (oidcEnabled false) — there is nothing to sign in to"}, false
	case strings.TrimSpace(s.cfg.Issuer) == "":
		return Result{Name: name, Status: Fail, Detail: configPath + " names no oidcIssuer",
			Fix: "chino-api's OIDC issuer is not configured — no client can sign in"}, false
	case web == "":
		return Result{Name: name, Status: Fail, Detail: configPath + " names no web client (oidcClientId.web)",
			Fix: "chino-api's web client id is not configured — the web client cannot sign in"}, false
	}
	return Result{Name: name, Status: OK, Detail: fmt.Sprintf("the issuer %s and the web client %q", s.cfg.Issuer, web)}, true
}

// passwordSignIn walks the sign-in a person does, one result per step. ok is
// false when a step failed; nothing after it runs.
func (s *session) passwordSignIn(ctx context.Context, user, pass string) ([]Result, bool) {
	var out []Result
	add := func(r Result) bool { out = append(out, r); return r.Status != Fail }
	clientID := strings.TrimSpace(s.cfg.ClientID["web"])
	oc := &oidc.Client{HTTP: &http.Client{Timeout: requestTimeout}}

	// The issuer's metadata says where the login page is.
	ep, err := oc.Metadata(ctx, s.cfg.Issuer)
	switch {
	case err != nil:
		add(Result{Name: "sign-in: issuer", Status: Fail, Detail: err.Error(),
			Fix: "the issuer the instance advertises must serve its metadata from where users stand"})
		return out, false
	case ep.Authorization == "":
		add(Result{Name: "sign-in: issuer", Status: Fail, Detail: s.cfg.Issuer + " advertises no authorization_endpoint",
			Fix: "a web client signs in through the authorization endpoint — this issuer cannot sign anyone in to one"})
		return out, false
	}
	add(Result{Name: "sign-in: issuer", Status: OK, Detail: "the login page is at " + ep.Authorization})

	ac, err := oidc.NewAuthCode(clientID, s.base.String()+callbackPath, webScope)
	if err != nil {
		add(Result{Name: "sign-in: authorization", Status: Fail, Detail: err.Error()})
		return out, false
	}
	authURL, err := ac.URL(ep)
	if err != nil {
		add(Result{Name: "sign-in: authorization", Status: Fail, Detail: err.Error(),
			Fix: "the issuer's metadata must name an absolute authorization_endpoint"})
		return out, false
	}
	idp, _ := url.Parse(ep.Authorization)
	b := newBrowser(idp, ac, s.agent)

	// The authorization request: a login page is the answer a person sees.
	at, err := b.get(ctx, authURL)
	callback := ""
	switch {
	case err != nil:
		add(Result{Name: "sign-in: authorization", Status: Fail, Detail: "the authorization request got no answer: " + err.Error(),
			Fix: "the identity provider must answer at the authorization endpoint from where users stand"})
		return out, false
	case at.callback != "":
		// Straight back to the web client, before any page: with no session in
		// this browser that is the provider refusing the request itself.
		if _, _, cerr := ac.Callback(at.callback); cerr != nil {
			add(authorizationRefused(cerr, clientID, ac.RedirectURI))
			return out, false
		}
		add(Result{Name: "sign-in: authorization", Status: OK, Detail: "the identity provider answered with a code at once, without a login page"})
		callback = at.callback
	case at.page.loginForm() != nil:
		add(Result{Name: "sign-in: authorization", Status: OK,
			Detail: fmt.Sprintf("the login page answers for %q, its form posting to %s", clientID, authenticateAction)})
	default:
		add(authorizationPage(at, clientID, ac.RedirectURI))
		return out, false
	}

	// The login form, filled and sent the way a person does.
	if callback == "" {
		r, cb := b.submitLogin(ctx, at, user, pass)
		if !add(r) {
			return out, false
		}
		callback = cb
	}

	// The redirect back to the web client: the state has to be this request's.
	code, iss, err := ac.Callback(callback)
	var oe *oidc.Error
	switch {
	case errors.As(err, &oe):
		add(Result{Name: "sign-in: callback", Status: Fail,
			Detail: "the identity provider redirected back with an error: " + oe.Error(), Fix: oauthFix(oe, clientID, ac.RedirectURI)})
		return out, false
	case errors.Is(err, oidc.ErrStateMismatch):
		add(Result{Name: "sign-in: callback", Status: Fail, Detail: err.Error(),
			Fix: "the answer belongs to another sign-in — something between the identity provider and the web client caches or rewrites redirects"})
		return out, false
	case err != nil:
		add(Result{Name: "sign-in: callback", Status: Fail, Detail: err.Error(),
			Fix: "the identity provider must redirect back with a code (response_type=code)"})
		return out, false
	case iss != "" && iss != ep.Issuer:
		add(Result{Name: "sign-in: callback", Status: Fail,
			Detail: fmt.Sprintf("issuer mismatch: the redirect names %s, the issuer's metadata %s", iss, ep.Issuer),
			Fix:    "the answer comes from another issuer than the one asked — the identity provider is reached under another name than it calls itself"})
		return out, false
	}
	add(Result{Name: "sign-in: callback", Status: OK, Detail: "back at " + ac.RedirectURI + " with a code, and the state is this request's"})

	// The code, exchanged at the token endpoint with the PKCE verifier.
	tok, err := oc.Exchange(ctx, ep, ac, code)
	if err != nil {
		add(exchangeFailure(err, clientID))
		return out, false
	}
	issued := "an access token"
	switch {
	case tok.IDToken != "" && tok.RefreshToken != "":
		issued = "an access token, an ID token and a refresh token"
	case tok.IDToken != "":
		issued = "an access token and an ID token"
	}
	add(Result{Name: "sign-in: token exchange", Status: OK, Detail: "the code was exchanged for " + issued})
	s.token = tok.AccessToken

	// The ID token: does it answer THIS sign-in?
	if tok.IDToken == "" {
		add(Result{Name: "sign-in: id token", Status: Fail, Detail: "the token endpoint issued no ID token",
			Fix: "the request asked for the openid scope; an identity provider that answers it without an ID token is not speaking OpenID Connect to this client"})
		return out, false
	}
	claims, err := ac.CheckIDToken(tok.IDToken, ep.Issuer, now())
	if err != nil {
		add(idTokenFailure(err))
		return out, false
	}
	add(Result{Name: "sign-in: id token", Status: OK, Detail: fmt.Sprintf("issued by %s for %q, answering this sign-in, valid until %s",
		claims.Issuer, clientID, claims.Expiry.UTC().Format("15:04 UTC"))})
	return out, true
}

// checkAccessToken reads what the access token says about itself and compares
// it with what the instance says its APIs check: the issuer, and chino-api's
// audience. A token the APIs will refuse is named here, once, and the checks
// that need those APIs say so instead of failing one by one.
//
// For a token the doctor got by signing in as the web client, a mismatch is
// the platform's own: every person signing in gets the same token. A borrowed
// one — ZAE_TOKEN, or a session `zae login` stored, which belongs to another
// client — says nothing about the web client, so there the line is a skip
// that says how to run the checks.
func (s *session) checkAccessToken(signedIn bool) Result {
	const name = "sign-in: access token"
	claims, ok := oidc.ParseClaims(s.token)
	if !ok {
		return Result{Name: name, Status: Skip, Detail: "the access token is not a JWT, so zae cannot read what the APIs will check — they decide"}
	}
	verdict := Fail
	tail := ""
	if !signedIn {
		verdict = Skip
		tail = " — set " + UserEnv + " and " + PasswordEnv + " to sign in as the web client does"
	}
	want := strings.TrimRight(s.cfg.Issuer, "/")
	if strings.TrimRight(claims.Issuer, "/") != want {
		s.noAPIs = "every API validates tokens against " + want + ", and this one is from another issuer"
		return Result{Name: name, Status: verdict,
			Detail: fmt.Sprintf("issuer mismatch: the token names %q, and the instance validates tokens against %s", claims.Issuer, want),
			Fix:    "the identity provider must call itself by the address the instance advertises (in Keycloak: its hostname setting), or the instance must advertise the issuer it really has" + tail}
	}
	if aud := strings.TrimSpace(s.cfg.Audience); aud != "" && !claims.HasAudience(aud) {
		s.noChino = fmt.Sprintf("chino-api accepts only tokens for %q, and this one is for %s", aud, quoteList(claims.Audience))
		return Result{Name: name, Status: verdict,
			Detail: fmt.Sprintf("the token carries no %q audience, which chino-api requires (it is for %s)", aud, quoteList(claims.Audience)),
			Fix:    fmt.Sprintf("give the web client an audience mapper that adds %q to its access tokens", aud) + tail}
	}
	detail := "issued by the issuer the instance advertises"
	if aud := strings.TrimSpace(s.cfg.Audience); aud != "" {
		detail += fmt.Sprintf(", for %q as chino-api requires", aud)
	}
	return Result{Name: name, Status: OK, Detail: detail}
}

func quoteList(ss []string) string {
	if len(ss) == 0 {
		return "no audience"
	}
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}

// browser is as much of one as a sign-in needs: cookies kept per host, and
// redirects followed by hand, so that the one back to the web client is where
// it stops — the code in it is a credential, and nothing follows it.
type browser struct {
	http  *http.Client
	idp   *url.URL
	ac    *oidc.AuthCode
	agent string
}

func newBrowser(idp *url.URL, ac *oidc.AuthCode, agent string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{idp: idp, ac: ac, agent: agent, http: &http.Client{
		Timeout:       requestTimeout,
		Jar:           secureContextJar{jar},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// secureContextJar keeps cookies the way a browser does. A browser counts
// http://localhost and every name under .localhost as potentially trustworthy
// (W3C Secure Contexts) and sends them the cookies marked Secure — and
// Keycloak marks its login session's cookies Secure (SameSite=None) even over
// plain http. Go's jar sends a Secure cookie over https only, so without this
// a sign-in to http://zaentrum.localhost, which a browser completes, would
// lose its session at the login form.
type secureContextJar struct{ http.CookieJar }

func (j secureContextJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.CookieJar.SetCookies(secureContext(u), cookies)
}

func (j secureContextJar) Cookies(u *url.URL) []*http.Cookie {
	return j.CookieJar.Cookies(secureContext(u))
}

// secureContext is u as a browser regards it: a loopback name over http is a
// secure context, so it reads as https to the jar. Any other URL is itself.
func secureContext(u *url.URL) *url.URL {
	if u.Scheme != "http" || !loopbackName(u.Hostname()) {
		return u
	}
	c := *u
	c.Scheme = "https"
	return &c
}

// loopbackName says whether a browser treats host as this machine: localhost,
// a name under .localhost, or a loopback address.
func loopbackName(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// landing is where a request ended up: a page, or the redirect back to the web
// client.
type landing struct {
	status   int
	at       *url.URL
	page     *page
	callback string
}

func (b *browser) get(ctx context.Context, raw string) (*landing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	return b.do(req)
}

// do sends req and follows the identity provider's redirects. A redirect to
// anywhere but the identity provider itself or the web client's redirect URI
// ends it: the cookies of a login session go nowhere else.
func (b *browser) do(req *http.Request) (*landing, error) {
	for hop := 0; ; hop++ {
		req.Header.Set("User-Agent", b.agent)
		if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "text/html,application/xhtml+xml")
		}
		resp, err := b.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" {
			resp.Body.Close()
			next, err := req.URL.Parse(resp.Header.Get("Location"))
			if err != nil {
				return nil, fmt.Errorf("redirected to an address that is not a URL: %v", err)
			}
			if b.ac.IsCallback(next.String()) {
				return &landing{status: resp.StatusCode, at: req.URL, callback: next.String()}, nil
			}
			if !sameOrigin(next, b.idp) {
				return nil, fmt.Errorf("redirected to %s, which is neither the identity provider nor the web client", origin(next))
			}
			if hop >= maxHops {
				return nil, errors.New("more than " + fmt.Sprint(maxHops) + " redirects inside the identity provider")
			}
			// Whatever the status, a redirect is followed with a GET: a form is
			// sent once, and a password never twice.
			req, err = http.NewRequestWithContext(req.Context(), http.MethodGet, next.String(), nil)
			if err != nil {
				return nil, err
			}
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		return &landing{status: resp.StatusCode, at: req.URL, page: parsePage(body)}, nil
	}
}

// submitLogin fills the login form on the page and sends it, and reads what
// came back: the redirect to the web client, or a page that says why not.
func (b *browser) submitLogin(ctx context.Context, at *landing, user, pass string) (Result, string) {
	const name = "sign-in: login form"
	f := at.page.loginForm()
	action, err := at.at.Parse(f.action)
	switch {
	case err != nil || f.action == "":
		return Result{Name: name, Status: Fail, Detail: "the login form names no address to post to"}, ""
	case f.method != "post":
		// A password in a GET would be in the address, and so in every log the
		// address passes through. A person's browser would send it; this does not.
		return Result{Name: name, Status: Fail, Detail: "the login form is sent with " + strings.ToUpper(f.method) + ", which would put the password in an address",
			Fix: "a login form must post"}, ""
	case !sameOrigin(action, b.idp):
		return Result{Name: name, Status: Fail,
			Detail: fmt.Sprintf("the login form posts to %s, not to the identity provider at %s — the password was not sent", origin(action), origin(b.idp)),
			Fix:    "the identity provider renders its own address wrongly — set its hostname to the address users reach it at (the issuer trap)"}, ""
	}
	vals := f.values()
	if u := f.usernameField(); u != nil {
		vals.Set(u.name, user)
	} else {
		vals.Set("username", user)
	}
	vals.Set(f.passwordField().name, pass)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action.String(), strings.NewReader(vals.Encode()))
	if err != nil {
		return Result{Name: name, Status: Fail, Detail: err.Error()}, ""
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got, err := b.do(req)
	switch {
	case err != nil:
		return Result{Name: name, Status: Fail, Detail: "the login form got no answer: " + err.Error(),
			Fix: "the identity provider stopped answering between the login page and the form"}, ""
	case got.callback != "":
		return Result{Name: name, Status: OK, Detail: "the account signed in, and the identity provider redirected back to the web client"}, got.callback
	}
	return loginRefused(got), ""
}

// requiredActions names what a pending required action asks of the account,
// for the line that says why the sign-in stopped there.
var requiredActions = map[string]string{
	"UPDATE_PASSWORD":      "it must set a new password",
	"VERIFY_PROFILE":       "its profile must be completed (the fields the realm requires, such as an e-mail and names)",
	"UPDATE_PROFILE":       "its profile must be updated",
	"VERIFY_EMAIL":         "its e-mail address must be verified",
	"CONFIGURE_TOTP":       "it must set up a one-time-code authenticator",
	"TERMS_AND_CONDITIONS": "it must accept the terms and conditions",
	"webauthn-register":    "it must register a security key",
	"delete_credential":    "it must confirm deleting a credential",
}

// loginRefused reads the page the identity provider answered the form with.
func loginRefused(got *landing) Result {
	const name = "sign-in: login form"
	p := got.page
	said := ""
	if p.message != "" {
		said = ": “" + p.message + "”"
	}
	if action := p.requiredAction(); action != "" {
		what := requiredActions[action]
		if what == "" {
			what = "it must complete it before it can sign in"
		}
		return Result{Name: name, Status: Fail,
			Detail: fmt.Sprintf("required action pending: %s — the password is right, and %s", action, what),
			Fix:    "complete it once in a browser, or clear it in the identity provider (in Keycloak: Users → the account → Required user actions); a verification account must sign in without one"}
	}
	if p.loginForm() != nil {
		if wrongPassword(p.message) {
			return Result{Name: name, Status: Fail, Detail: "wrong password: the identity provider refused the credentials" + said,
				Fix: "check " + UserEnv + " and " + PasswordEnv + " — an unknown user, and an account the brute-force protection has locked for now, read the same"}
		}
		return Result{Name: name, Status: Fail, Detail: "the identity provider showed the login form again" + said,
			Fix: "it refused this sign-in; its message says why"}
	}
	if p.otherStep() != nil {
		return Result{Name: name, Status: Fail, Detail: "the password was accepted, and the identity provider asks for another step (a one-time code, or a security key)" + said,
			Fix: "the doctor signs in with a password alone — give the verification account no second factor"}
	}
	title := ""
	if p.title != "" {
		title = " “" + p.title + "”"
	}
	fix := "a lost login session reads like this (cookies dropped by a proxy, or identity provider replicas that do not share sessions) — so does a disabled account"
	if got.at != nil && got.at.Scheme == "http" && !loopbackName(got.at.Hostname()) &&
		strings.Contains(strings.ToLower(p.message+" "+p.title), "cookie") {
		fix = "serve the instance over https: the identity provider marks its login cookies Secure, and a browser sends those only over https or to a localhost name — over plain http on " +
			got.at.Hostname() + " nobody can sign in"
	}
	return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("the identity provider answered HTTP %d, a page%s with no way forward%s", got.status, title, said),
		Fix: fix}
}

// wrongPassword recognises the identity provider saying the credentials are
// not right. It decides only the wording: every refusal of the form fails.
func wrongPassword(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "invalid username or password") || strings.Contains(m, "invalid password") ||
		strings.Contains(m, "invalid user") || strings.Contains(m, "invalid credentials")
}

// authorizationPage reads a page that is not the login page.
func authorizationPage(at *landing, clientID, redirectURI string) Result {
	const name = "sign-in: authorization"
	p := at.page
	msg := strings.ToLower(p.message)
	said := ""
	if p.message != "" {
		said = ": “" + p.message + "”"
	}
	switch {
	case strings.Contains(msg, "redirect_uri") || strings.Contains(msg, "redirect uri"):
		return Result{Name: name, Status: Fail,
			Detail: fmt.Sprintf("redirect URI not allowed: the identity provider refuses %s for %q%s", redirectURI, clientID, said),
			Fix:    fmt.Sprintf("add %s to the valid redirect URIs of the client %q — it is where the web client returns after a sign-in", redirectURI, clientID)}
	case strings.Contains(msg, "client not found") || strings.Contains(msg, "unknown client") || strings.Contains(msg, "invalid client"):
		return Result{Name: name, Status: Fail,
			Detail: fmt.Sprintf("the identity provider does not know the client %q%s", clientID, said),
			Fix:    "the instance advertises a web client its identity provider does not have — create it, or correct oidcClientId.web"}
	case at.status >= 500:
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("the authorization endpoint answered HTTP %d%s", at.status, said),
			Fix: "the identity provider is failing — its logs say why"}
	}
	return Result{Name: name, Status: Fail,
		Detail: fmt.Sprintf("the authorization endpoint answered HTTP %d without a login form%s", at.status, said),
		Fix:    "a person would see this page instead of signing in"}
}

// authorizationRefused is the provider refusing the request with a redirect.
func authorizationRefused(err error, clientID, redirectURI string) Result {
	var oe *oidc.Error
	if errors.As(err, &oe) {
		return Result{Name: "sign-in: authorization", Status: Fail,
			Detail: "the identity provider refused the request: " + oe.Error(), Fix: oauthFix(oe, clientID, redirectURI)}
	}
	return Result{Name: "sign-in: authorization", Status: Fail, Detail: err.Error()}
}

// oauthFix names the likely cause of an OAuth error answer.
func oauthFix(oe *oidc.Error, clientID, redirectURI string) string {
	d := strings.ToLower(oe.Description)
	switch {
	case strings.Contains(d, "redirect"):
		return fmt.Sprintf("redirect URI not allowed: add %s to the valid redirect URIs of the client %q", redirectURI, clientID)
	case oe.Code == "unauthorized_client":
		return fmt.Sprintf("the client %q may not use the code flow — enable the standard flow on it", clientID)
	case oe.Code == "invalid_scope":
		return fmt.Sprintf("the client %q is not given one of the scopes the web client asks for (%s)", clientID, webScope)
	case oe.Code == "access_denied":
		return "the identity provider denied the account access to this client"
	case oe.Code == "temporarily_unavailable" || oe.Code == "server_error":
		return "the identity provider is failing — its logs say why"
	}
	return "the identity provider's own words say what to change"
}

// exchangeFailure is the token endpoint refusing the code.
func exchangeFailure(err error, clientID string) Result {
	const name = "sign-in: token exchange"
	var oe *oidc.Error
	switch {
	case errors.As(err, &oe) && oe.Code == "invalid_grant":
		return Result{Name: name, Status: Fail, Detail: "the token endpoint refused the code: " + oe.Error(),
			Fix: "a code is used once, soon, with the redirect URI and the PKCE verifier of its request — a proxy that replays requests, or a clock far off on the identity provider, reads like this"}
	case errors.As(err, &oe) && (oe.Code == "invalid_client" || oe.Code == "unauthorized_client"):
		return Result{Name: name, Status: Fail, Detail: "the token endpoint refused the client: " + oe.Error(),
			Fix: fmt.Sprintf("the web client %q must be a public client — a browser cannot keep a secret", clientID)}
	case errors.As(err, &oe):
		return Result{Name: name, Status: Fail, Detail: "the token endpoint refused the code: " + oe.Error()}
	}
	return Result{Name: name, Status: Fail, Detail: err.Error(),
		Fix: "the token endpoint must answer from where users stand"}
}

// idTokenFailure names the claim that did not answer the request.
func idTokenFailure(err error) Result {
	const name = "sign-in: id token"
	var ce *oidc.ClaimError
	if !errors.As(err, &ce) {
		return Result{Name: name, Status: Fail, Detail: err.Error()}
	}
	switch ce.Claim {
	case "iss":
		return Result{Name: name, Status: Fail, Detail: "issuer mismatch: " + ce.Error(),
			Fix: "the identity provider is reached under another name than it calls itself — set its hostname to the address users reach (the issuer trap)"}
	case "aud", "azp":
		return Result{Name: name, Status: Fail, Detail: ce.Error(),
			Fix: "the identity provider issued the sign-in for another client than the one that asked"}
	case "nonce":
		return Result{Name: name, Status: Fail, Detail: ce.Error(),
			Fix: "the token answers another sign-in — something between the identity provider and here replays or caches answers"}
	}
	return Result{Name: name, Status: Fail, Detail: ce.Error(),
		Fix: "a clock is off — the identity provider's, or this machine's"}
}

// sameOrigin compares scheme and host (with port).
func sameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }
