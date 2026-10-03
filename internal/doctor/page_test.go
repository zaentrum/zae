package doctor

import (
	"strings"
	"testing"
)

// The bundled realm's own login theme, as an instance serves it.
const themeLogin = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>sign in · zaentrum</title>
  <link rel="stylesheet" href="/auth/resources/5z064/login/zaentrum/css/zaentrum.css">
</head>
<body>
  <main class="auth">
    <div class="card">
      <div class="brand" aria-label="zaentrum"><span class="gt">&gt;</span><span class="c">z</span></div>
      <h1>sign in</h1>
      %s
      <form id="kc-form-login" action="https://media.example.org/auth/realms/zaentrum/login-actions/authenticate?session_code=Rcrg0&amp;execution=e663&amp;client_id=chino-web&amp;tab_id=Dy-nG" method="post" autocomplete="off" novalidate>
        <label for="username">Username or email</label>
        <input id="username" name="username" type="text" autofocus autocomplete="username"
               value="" dir="ltr">
        <label for="password">Password</label>
        <input id="password" name="password" type="password" autocomplete="current-password">
          <label class="remember">
            <input type="checkbox" name="rememberMe" > Remember me
          </label>
        <button type="submit" name="login" id="kc-login">sign in</button>
      </form>
        <div class="links">
            <a class="link" href="/auth/realms/zaentrum/login-actions/reset-credentials?client_id=chino-web&amp;tab_id=Dy-nG">Forgot password?</a>
        </div>
    </div>
  </main>
</body>
</html>`

// Keycloak's stock theme, which a realm without a theme of its own shows —
// and the one a custom theme falls back to for every other page.
const stockLogin = `<!DOCTYPE html><html class="login-pf"><head><title>Sign in to zaentrum</title>
<script type="importmap">{"imports": {"rfc4648": "/auth/resources/x/common/keycloak/vendor/rfc4648/rfc4648.js"}}</script>
<script>const f = "<form id='fake' action='/login-actions/authenticate'><input type='password'>";</script>
</head><body>
<!-- <form action="/login-actions/authenticate"><input type="password" name="commented"></form> -->
<div class="pf-v5-c-alert pf-m-inline pf-m-danger">
  <div class="pf-v5-c-alert__icon"><span class="fa fa-fw fa-exclamation-circle"></span></div>
  <span class="kc-feedback-text">Invalid username or password.</span>
</div>
<form id="kc-form-login" class="pf-v5-c-form" onsubmit="login.disabled = true; return true;" action='https://idp.example.org/realms/z/login-actions/authenticate?session_code=a&amp;execution=b&amp;client_id=web&amp;tab_id=c' method=post novalidate="novalidate">
  <input tabindex="2" id="username" class="pf-v5-c-form-control" name="username" value="ada" type="text" autofocus autocomplete="off" aria-invalid="true"/>
  <input tabindex="3" id="password" class="pf-v5-c-form-control" name="password" type="password" autocomplete="off" aria-invalid="true"/>
  <span id="input-error" class="pf-v5-c-helper-text__item-text" aria-live="polite">Invalid username or password.</span>
  <input type="hidden" id="id-hidden-input" name="credentialId"/>
  <input type="checkbox" id="rememberMe" name="rememberMe" checked> Remember me
  <input tabindex="7" class="pf-v5-c-button" name="login" id="kc-login" type="submit" value="Sign In"/>
</form></body></html>`

func TestParsePageReadsTheThemesLoginForm(t *testing.T) {
	p := parsePage([]byte(strings.Replace(themeLogin, "%s", "", 1)))
	f := p.loginForm()
	if f == nil {
		t.Fatalf("no login form found: %+v", p.forms)
	}
	want := "https://media.example.org/auth/realms/zaentrum/login-actions/authenticate?session_code=Rcrg0&execution=e663&client_id=chino-web&tab_id=Dy-nG"
	if f.action != want || f.method != "post" {
		t.Fatalf("the action is read with its entities: %q %q", f.action, f.method)
	}
	if f.usernameField().name != "username" || f.passwordField().name != "password" {
		t.Fatalf("fields: %+v", f.inputs)
	}
	// Empty fields are sent, as a browser sends them; an unchecked checkbox
	// and a button are not.
	if v := f.values(); v.Encode() != "password=&username=" {
		t.Errorf("values: %q", v.Encode())
	}
	if p.message != "" || p.title != "sign in · zaentrum" || p.requiredAction() != "" {
		t.Errorf("message %q, title %q, action %q", p.message, p.title, p.requiredAction())
	}

	// The same page, saying no.
	p = parsePage([]byte(strings.Replace(themeLogin, "%s", `<div class="alert error" role="alert">Invalid username or password.</div>`, 1)))
	if p.message != "Invalid username or password." || p.loginForm() == nil {
		t.Fatalf("the refusal is read: %q", p.message)
	}
}

// What a script holds and what a comment hides are not forms; quoting is
// whatever the page chose; a checked box is sent as a browser sends it.
func TestParsePageReadsTheStockTheme(t *testing.T) {
	p := parsePage([]byte(stockLogin))
	if len(p.forms) != 1 {
		t.Fatalf("a form in a script or a comment was taken for one: %+v", p.forms)
	}
	f := p.loginForm()
	if f == nil || f.action != "https://idp.example.org/realms/z/login-actions/authenticate?session_code=a&execution=b&client_id=web&tab_id=c" {
		t.Fatalf("the single-quoted action: %+v", f)
	}
	if f.method != "post" {
		t.Errorf("an unquoted method: %q", f.method)
	}
	v := f.values()
	if v.Get("username") != "ada" || v.Get("rememberMe") != "on" || !v.Has("credentialId") || v.Has("login") {
		t.Errorf("values: %v", v)
	}
	if p.message != "Invalid username or password." {
		t.Errorf("the message: %q", p.message)
	}
}

// A required action is recognised by where its form posts, whatever the page
// looks like.
func TestParsePageFindsARequiredAction(t *testing.T) {
	p := parsePage([]byte(`<html><head><title>Update password</title></head><body>
<div id="kc-content"><div class="pf-v5-c-alert"><span class="kc-feedback-text">You need to change your password to activate your account.</span></div>
<form id="kc-passwd-update-form" action="/auth/realms/z/login-actions/required-action?session_code=x&amp;execution=UPDATE_PASSWORD&amp;client_id=web&amp;tab_id=t" method="post">
<input type="password" id="password-new" name="password-new"></form></div></body></html>`))
	if got := p.requiredAction(); got != "UPDATE_PASSWORD" {
		t.Fatalf("required action: %q", got)
	}
	if p.loginForm() != nil || p.otherStep() != nil {
		t.Errorf("a required action's form is neither the login form nor another step")
	}
	if !strings.Contains(p.message, "change your password") {
		t.Errorf("message: %q", p.message)
	}
	// A link to it counts as much as a form (the e-mail verification page has
	// no form of its own).
	p = parsePage([]byte(`<p>Check your inbox. <a href="/auth/realms/z/login-actions/required-action?execution=VERIFY_EMAIL&amp;client_id=web">resend</a></p>`))
	if got := p.requiredAction(); got != "VERIFY_EMAIL" {
		t.Fatalf("a link to a required action: %q", got)
	}
}

// An alert's text is all of it, however deep its elements nest.
func TestInnerTextAcrossNestedElements(t *testing.T) {
	p := parsePage([]byte(`<div role="alert"><div class="icon"></div><div>Your account is <b>temporarily</b> disabled.</div>
	<div>Contact&nbsp;your administrator.</div></div><div>after</div>`))
	if p.message != "Your account is temporarily disabled. Contact your administrator." {
		t.Fatalf("message: %q", p.message)
	}
	long := parsePage([]byte(`<span class="kc-feedback-text">` + strings.Repeat("ab ", 200) + `</span>`))
	if n := len([]rune(long.message)); n > 200 || !strings.HasSuffix(long.message, "…") {
		t.Fatalf("a long message is clipped to 200 characters, marked: %d", n)
	}
}

// A page that is not HTML at all, or is broken mid-tag, reads as a page with
// nothing on it — never a panic.
func TestParsePageSurvivesWhatIsNotAPage(t *testing.T) {
	for _, s := range []string{"", "{\"json\":true}", "<", "<form action=\"x", "<form action='/login-actions/authenticate'><input type=password", "<<<>>>", "<!-- never closed"} {
		p := parsePage([]byte(s))
		if p.loginForm() != nil && !strings.Contains(s, "type=password") {
			t.Errorf("%q: a login form out of nothing", s)
		}
	}
}
