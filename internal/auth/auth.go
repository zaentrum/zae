// Package auth is `zae login`, `zae logout` and `zae whoami`: signing a
// person in to an instance from a terminal, and saying who they are signed in
// as.
//
// It is part of the static core for the same reason doctor is — the moment
// you need to sign in is the moment nothing else works yet — and it learns
// everything instance-specific at runtime: the instance says which issuer it
// validates against and which client a CLI should use (the `auth` object in
// its discovery document), and the issuer says where its endpoints are. zae
// compiles in no realm, no URL and no client but a default name.
//
// The grant is the OAuth 2.0 Device Authorization Grant (RFC 8628) with PKCE:
// a CLI is a public client that cannot keep a secret and cannot receive a
// browser redirect, and the device grant is the one flow designed for exactly
// that. The terminal prints a URL and a code; the browser — possibly on
// another machine entirely — does the signing in.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/zaentrum/zae/internal/capability"
	"github.com/zaentrum/zae/internal/creds"
	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
	"github.com/zaentrum/zae/internal/oidc"
)

// DefaultClientID is the client a CLI signs in as when an instance does not
// name one. It matches the portal's PORTAL_CLI_CLIENT_ID default, so an
// operator who registered the documented client needs no flags.
const DefaultClientID = "zae"

// defaultScope is what the verified device request carries. Kept minimal on
// purpose: a scope an identity provider has not assigned to the client is
// refused outright, and the roles the API cares about ride in the token
// regardless. --scope overrides it.
const defaultScope = "openid"

// Streams are variables so tests can read what a command printed — and prove
// that no token is among it.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

func errf(format string, a ...any) { fmt.Fprintf(stderr, "zae: "+format+"\n", a...) }

// newClient builds the OAuth client. A variable so a test can pace polling
// without waiting out real intervals.
var newClient = func() *oidc.Client { return &oidc.Client{} }

// openBrowser hands a URL to the desktop's own opener. Best effort by
// design: no opener is the normal case on a server, in a container or over
// ssh, and the URL is on the screen either way — so nothing here may fail a
// login. A variable so tests can see what would have been opened.
var openBrowser = func(raw string) {
	u, err := url.Parse(raw)
	// Only ever hand a browser an http(s) URL: the address came from an
	// identity provider over the network, and `open` will happily act on
	// schemes that are not a web page at all.
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", raw)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", raw)
	default:
		cmd = exec.Command("xdg-open", raw)
	}
	if err := cmd.Start(); err != nil {
		return // no opener installed: the printed URL is the fallback
	}
	// Reap it rather than leaving a zombie; its outcome changes nothing.
	go func() { _ = cmd.Wait() }()
}

// interruptible returns a context that ends on Ctrl-C or SIGTERM, so a poll
// that would otherwise wait out the code's ten minutes stops at once.
func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Login runs `zae login --url https://…`.
func Login(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	issuerFlag := fs.String("issuer", "", "OIDC issuer to sign in against (only needed when the instance does not advertise one)")
	clientFlag := fs.String("client-id", "", "OIDC client id to sign in as (default: what the instance advertises)")
	scope := fs.String("scope", defaultScope, "OAuth scopes to request")
	noBrowser := fs.Bool("no-browser", false, "do not open a browser; only print the URL and code")
	if err := fs.Parse(args); err != nil {
		return exitcode.Usage
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		errf("usage: %v", err)
		return exitcode.Usage
	}

	ctx, stop := interruptible()
	defer stop()

	issuer, clientID, code := resolveAuth(ctx, base, *issuerFlag, *clientFlag)
	if code != exitcode.OK {
		return code
	}

	c := newClient()
	ep, err := c.Discover(ctx, issuer)
	if err != nil {
		return discoveryFailure(issuer, err)
	}

	// Say where the credentials are about to be typed, before a browser opens.
	// The issuer came from the instance over the network; a person is entitled
	// to see which identity provider they are about to authenticate to.
	fmt.Fprintf(stdout, "signing in to %s\n  identity provider: %s\n  client:            %s\n\n", base, ep.Issuer, clientID)

	dev, err := c.Authorize(ctx, ep, clientID, *scope)
	if err != nil {
		return authorizeFailure(ctx, ep, clientID, err)
	}

	fmt.Fprintf(stdout, "open %s\n", dev.Open())
	if dev.VerificationURIComplete != "" {
		fmt.Fprintf(stdout, "  (it already carries the code; confirm that it shows %s)\n", dev.UserCode)
	} else {
		fmt.Fprintf(stdout, "  and enter the code: %s\n", dev.UserCode)
	}
	if !*noBrowser {
		openBrowser(dev.Open())
	}
	fmt.Fprintf(stdout, "\nwaiting for you to finish in the browser (Ctrl-C to stop) …\n")

	tok, err := c.Poll(ctx, ep, clientID, dev)
	if err != nil {
		return pollFailure(ctx, err)
	}

	e := creds.Entry{
		Issuer:       ep.Issuer,
		ClientID:     clientID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
		SavedAt:      time.Now(),
	}
	claims, parsed := oidc.ParseClaims(tok.AccessToken)
	if parsed {
		e.Subject, e.Username = claims.Subject, claims.Username
	}
	if err := creds.Put(base, e); err != nil {
		errf("failed: signed in, but the credentials could not be stored: %v", err)
		return exitcode.Failed
	}
	path, _ := creds.Path()

	fmt.Fprintf(stdout, "\nsigned in to %s\n", base)
	if parsed {
		fmt.Fprintf(stdout, "  as         %s\n", identity(claims))
	}
	// Whether this sign-in makes an admin is the instance's to say; an older
	// portal-api cannot, and then the token is read.
	if m, how, _ := askMe(ctx, base, tok.AccessToken); how == meAnswered {
		fmt.Fprintf(stdout, "  admin role %s\n", m.adminLine())
	} else if parsed {
		fmt.Fprintf(stdout, "  admin role %s\n", roleState(claims))
	}
	if !tok.Expiry.IsZero() {
		fmt.Fprintf(stdout, "  expires    %s\n", tok.Expiry.Local().Format(time.RFC1123))
		if tok.RefreshToken != "" {
			fmt.Fprintf(stdout, "             renewed automatically while the session lives\n")
		}
	}
	fmt.Fprintf(stdout, "  stored in  %s\n", path)
	return exitcode.OK
}

// resolveAuth decides which issuer and client to sign in with: the flags
// first, then what the instance advertises. Both halves must be known before
// a login can start, and each way of not knowing them gets its own answer.
func resolveAuth(ctx context.Context, base, issuerFlag, clientFlag string) (issuer, clientID string, code int) {
	issuer, clientID = strings.TrimRight(strings.TrimSpace(issuerFlag), "/"), strings.TrimSpace(clientFlag)
	if issuer != "" && clientID != "" {
		return issuer, clientID, exitcode.OK // fully specified: no need to ask the instance
	}

	doc, err := capability.Fetch(ctx, base)
	switch {
	case err == nil || errors.Is(err, capability.ErrSchema):
		// A newer schema still carries `auth` where v1 put it; reading it is
		// better than refusing to sign in to an instance that is ahead of us.
	case errors.Is(err, capability.ErrUnsupported):
		if issuer == "" || clientID == "" {
			errf("not offered: %s does not serve capability discovery, so it cannot say where to sign in — give both --issuer and --client-id, or upgrade the instance", base)
			return "", "", exitcode.NotOffered
		}
	default:
		errf("undetermined: cannot reach %s to ask where to sign in (%v) — not concluding it cannot be signed in to", base, err)
		return "", "", exitcode.Undetermined
	}

	if doc != nil && doc.Auth != nil {
		if issuer == "" {
			issuer = strings.TrimRight(doc.Auth.Issuer, "/")
		}
		if clientID == "" {
			clientID = doc.Auth.ClientID
		}
	}
	if clientID == "" && issuer != "" {
		// An issuer without a client: the documented default is a better guess
		// than an error, and a wrong guess fails loudly at the provider.
		clientID = DefaultClientID
	}
	if issuer == "" {
		errf("not offered: %s does not advertise how to sign in (no `auth` in its discovery document: its portal predates the field, or it has no identity provider configured) — give --issuer and --client-id, or upgrade the instance", base)
		return "", "", exitcode.NotOffered
	}
	return issuer, clientID, exitcode.OK
}

// discoveryFailure maps a failure to read the issuer's metadata. The device
// endpoint being absent is the one an operator can act on, so it says what to
// enable rather than what went wrong.
func discoveryFailure(issuer string, err error) int {
	switch {
	case errors.Is(err, oidc.ErrNoDeviceGrant):
		errf("not offered: %s advertises no device_authorization_endpoint — an operator must enable the OAuth 2.0 Device Authorization Grant (RFC 8628) on the identity provider and on the CLI's client (in Keycloak: Clients → the CLI client → Settings → 'OAuth 2.0 Device Authorization Grant')", issuer)
		return exitcode.NotOffered
	case errors.Is(err, context.Canceled):
		return interrupted()
	default:
		errf("undetermined: cannot read the identity provider's metadata (%v)", err)
		return exitcode.Undetermined
	}
}

// authorizeFailure maps a refused device authorization request.
func authorizeFailure(ctx context.Context, ep *oidc.Endpoints, clientID string, err error) int {
	var oe *oidc.Error
	switch {
	case ctx.Err() != nil:
		return interrupted()
	case oidc.IsMissingPKCE(err):
		// The provider's client requires PKCE and the request carried none.
		// This zae always sends a challenge, so this is an older zae (upgrade)
		// or a proxy stripping the form.
		errf("failed: %s refused the device request because it carried no PKCE challenge (%v) — this zae always sends one, so upgrade zae, or check what sits between it and the identity provider", ep.Device, err)
		return exitcode.Failed
	case errors.As(err, &oe) && oe.Code == "invalid_client":
		errf("forbidden: the identity provider does not accept the client %q (%v) — an operator must create it as a public client with the device grant enabled, or point --client-id at the right one", clientID, err)
		return exitcode.Forbidden
	case errors.As(err, &oe) && oe.Code == "unauthorized_client":
		errf("forbidden: the client %q may not use the device grant (%v) — an operator must enable 'OAuth 2.0 Device Authorization Grant' on it", clientID, err)
		return exitcode.Forbidden
	case errors.Is(err, oidc.ErrUnreachable):
		errf("undetermined: cannot start a device authorization at %s (%v)", ep.Device, err)
		return exitcode.Undetermined
	default:
		errf("failed: the identity provider refused the device authorization request: %v", err)
		return exitcode.Failed
	}
}

// pollFailure maps the ways a wait can end other than with a token.
func pollFailure(ctx context.Context, err error) int {
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return interrupted()
	case errors.Is(err, oidc.ErrDenied):
		errf("forbidden: the sign-in was refused (%v)", err)
		return exitcode.Forbidden
	case errors.Is(err, oidc.ErrCodeExpired):
		errf("failed: the code expired before it was used (%v) — run `zae login` again", err)
		return exitcode.Failed
	case errors.Is(err, oidc.ErrUnreachable):
		errf("undetermined: lost contact with the identity provider while waiting (%v)", err)
		return exitcode.Undetermined
	default:
		errf("failed: the sign-in did not complete: %v", err)
		return exitcode.Failed
	}
}

func interrupted() int {
	fmt.Fprintln(stdout, "\nstopped; nothing was stored.")
	return 130
}

// Logout runs `zae logout --url https://…` or `zae logout --all`.
//
// Signing out ends the session where it lives, then forgets it here: the
// refresh token is revoked at the issuer (RFC 7009, at the endpoint the
// issuer's metadata names), and the stored entry is removed. Forgetting alone
// would leave a session that outlives the file it was copied from.
//
// The entry is removed whatever the issuer answers: a person who signs out
// must not be kept signed in by an identity provider that is down or gone.
// Whether it ended there too is the exit code — 0 it did, 3 the issuer offers
// no revocation, 4 it could not be asked, 1 it refused — and the message says
// how long the session lives on when it did not. Ctrl-C before the answer
// keeps the entry, so the sign-out can be done again.
func Logout(args []string) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "instance whose session to end and forget")
	all := fs.Bool("all", false, "end and forget every stored session")
	if err := fs.Parse(args); err != nil {
		return exitcode.Usage
	}
	ctx, stop := interruptible()
	defer stop()
	if *all {
		if *rawURL != "" {
			errf("usage: zae logout takes --url or --all, not both")
			return exitcode.Usage
		}
		f, err := creds.Load()
		if err != nil {
			// Unreadable, so nothing in it can be revoked: forgetting it is all
			// there is to do, as it always was.
			if _, cerr := creds.Clear(); cerr != nil {
				errf("failed: %v", err)
				return exitcode.Failed
			}
			path, _ := creds.Path()
			errf("warning: %v — removed it without ending its sessions at their identity providers", err)
			fmt.Fprintf(stdout, "removed 0 stored session(s); %s is gone\n", path)
			return exitcode.OK
		}
		bases, _ := creds.List()
		code := exitcode.OK
		for _, base := range bases {
			c := endSession(ctx, base, f.Instances[base])
			if ctx.Err() != nil {
				return interruptedLogout()
			}
			if code == exitcode.OK {
				code = c
			}
		}
		n, err := creds.Clear()
		if err != nil {
			errf("failed: %v", err)
			return exitcode.Failed
		}
		path, _ := creds.Path()
		fmt.Fprintf(stdout, "removed %d stored session(s); %s is gone\n", n, path)
		return code
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		errf("usage: %v (or `zae logout --all`)", err)
		return exitcode.Usage
	}
	e, ok, err := creds.Get(base)
	if err != nil {
		errf("failed: %v", err)
		return exitcode.Failed
	}
	if !ok {
		// Not an error: the end state the caller asked for is the one that holds.
		fmt.Fprintf(stdout, "no stored session for %s\n", base)
		return exitcode.OK
	}
	code := endSession(ctx, base, e)
	if ctx.Err() != nil {
		return interruptedLogout()
	}
	if _, err := creds.Remove(base); err != nil {
		errf("failed: %v", err)
		return exitcode.Failed
	}
	fmt.Fprintf(stdout, "removed the stored session for %s\n", base)
	if os.Getenv(instance.TokenEnv) != "" {
		fmt.Fprintf(stdout, "note: %s is still set in this shell, and still wins over a stored session\n", instance.TokenEnv)
	}
	return code
}

func interruptedLogout() int {
	fmt.Fprintln(stdout, "\nstopped; the stored session is kept — run zae logout again to end it")
	return 130
}

// endSession revokes a stored session at its issuer and says how that went:
// on stdout when it ended, on stderr — with how long it lives on — when it did
// not. It returns the exit code of the outcome.
//
// The refresh token is what keeps a session alive, and revoking it ends the
// session at the issuer; an access token already issued is a bearer the
// instance checks only until it expires, minutes later. An entry with no
// refresh token has its access token revoked instead.
func endSession(ctx context.Context, base string, e creds.Entry) int {
	token, hint, what := e.RefreshToken, "refresh_token", "refresh token"
	if token == "" {
		token, hint, what = e.AccessToken, "access_token", "access token"
	}
	issuer := strings.TrimRight(e.Issuer, "/")
	lives := "it stays valid there until it expires"
	switch {
	case token == "":
		return exitcode.OK // nothing that could still be used
	case issuer == "":
		errf("not offered: the session for %s names no identity provider, so it cannot be ended there — %s", base, lives)
		return exitcode.NotOffered
	}
	c := newClient()
	ep, err := c.Metadata(ctx, issuer)
	if err == nil {
		err = c.Revoke(ctx, ep, e.ClientID, token, hint)
	}
	var oe *oidc.Error
	switch {
	case err == nil:
		fmt.Fprintf(stdout, "ended the session for %s at %s: its %s is revoked\n", base, issuer, what)
		return exitcode.OK
	case ctx.Err() != nil:
		return 130
	case errors.Is(err, oidc.ErrNoRevocation):
		errf("not offered: %s offers no token revocation (no revocation_endpoint in its metadata), so the session for %s cannot be ended there — %s", issuer, base, lives)
		return exitcode.NotOffered
	case errors.As(err, &oe):
		errf("failed: %s refused to revoke the session for %s (%v) — %s; end it from the identity provider's own account page to end it now", issuer, base, err, lives)
		return exitcode.Failed
	default:
		errf("undetermined: could not end the session for %s at %s (%v) — %s; end it from the identity provider's own account page to end it now", base, issuer, err, lives)
		return exitcode.Undetermined
	}
}

// Whoami runs `zae whoami --url https://…`: who the instance says the bearer
// zae would send is, and whether it grants that bearer its admin role.
//
// It ASKS (GET /api/portal/me): which role makes an admin is the instance's
// setting, and a portal may take that role only on tokens issued to its own
// clients — neither is in the token. Against a portal-api older than the
// answer, and when the instance cannot be reached, it reads the token's own
// claims as before and says that it did. It never prints the token.
//
// Exit codes: 0 answered (or read from the token, from an older portal), 4
// the instance could not be asked, 5 no credentials, or the instance refused
// the bearer.
func Whoami(args []string) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	role := fs.String("role", instance.AdminRole, "a realm role to report on as well — and, against an older portal, the admin role to look for in the token")
	asJSON := fs.Bool("json", false, "print the answer as JSON")
	if err := fs.Parse(args); err != nil {
		return exitcode.Usage
	}
	roleSet := false
	fs.Visit(func(f *flag.Flag) { roleSet = roleSet || f.Name == "role" })
	base, err := instance.Base(*rawURL)
	if err != nil {
		errf("usage: %v", err)
		return exitcode.Usage
	}

	entry, stored, err := creds.Get(base)
	if err != nil {
		errf("failed: %v", err)
		return exitcode.Failed
	}
	env := strings.TrimSpace(os.Getenv(instance.TokenEnv))
	if env == "" && !stored {
		errf("forbidden: no credentials for %s — run: zae login --url %s, or supply a bearer via %s", base, base, instance.TokenEnv)
		return exitcode.Forbidden
	}

	ctx, stop := interruptible()
	defer stop()
	m, how, said := askMe(ctx, base, "")
	if ctx.Err() != nil {
		return interrupted()
	}
	if how == meNoSession {
		errf("forbidden: %s", said)
		return exitcode.Forbidden
	}
	// Asking renews an expired session and writes it back: read what is
	// stored now.
	if env == "" {
		entry, _, _ = creds.Get(base)
	}
	w := whoami{base: base, env: env != "", stored: stored, entry: entry, me: m, role: *role, roleSet: roleSet}
	w.token = entry.AccessToken
	if env != "" {
		w.token = env
	}
	w.claims, w.jwt = oidc.ParseClaims(w.token)

	if *asJSON {
		w.printJSON()
	} else {
		w.print(how)
	}
	switch how {
	case meRefused:
		errf("forbidden: %s refuses this bearer (%s) — %s", base, said, instance.ForbiddenHint(base, ""))
		return exitcode.Forbidden
	case meUnreachable:
		errf("undetermined: cannot ask %s who this bearer is (%s) — shown is what the token itself claims; the instance decides", base, said)
		return exitcode.Undetermined
	}
	return exitcode.OK
}

// whoami is what Whoami found out, to print.
type whoami struct {
	base        string
	env, stored bool
	entry       creds.Entry
	token       string
	claims      *oidc.Claims
	jwt         bool
	me          *me
	role        string
	roleSet     bool
}

func (w *whoami) source() string {
	if w.env {
		return instance.TokenEnv
	}
	return "the stored session"
}

// expiry is when the bearer stops working: the token's own exp, else what the
// identity provider said when it issued the stored one.
func (w *whoami) expiry() time.Time {
	if w.jwt && !w.claims.Expiry.IsZero() {
		return w.claims.Expiry
	}
	if !w.env {
		return w.entry.Expiry
	}
	return time.Time{}
}

func (w *whoami) print(how meOutcome) {
	fmt.Fprintf(stdout, "%s\n  bearer from %s\n", w.base, w.source())
	if w.stored && w.env {
		fmt.Fprintf(stdout, "              (a stored session exists too; %s wins)\n", instance.TokenEnv)
	}
	if !w.env {
		fmt.Fprintf(stdout, "  issuer      %s\n  client      %s\n", w.entry.Issuer, w.entry.ClientID)
	} else if w.me != nil && w.me.Client != "" {
		fmt.Fprintf(stdout, "  client      %s\n", w.me.Client)
	}
	if w.jwt {
		fmt.Fprintf(stdout, "  subject     %s\n", orUnset(w.claims.Subject))
	}
	switch {
	case w.me != nil:
		fmt.Fprintf(stdout, "  username    %s\n", orUnset(w.me.Username))
		fmt.Fprintf(stdout, "  admin role  %s\n", w.me.adminLine())
		if len(w.me.Roles) > 0 {
			fmt.Fprintf(stdout, "  roles       %s\n", strings.Join(w.me.Roles, ", "))
		}
		if w.roleSet {
			fmt.Fprintf(stdout, "  role        %s\n", listed(w.me.Roles, w.role))
		}
	case !w.jwt:
		// An opaque token is a legitimate thing for a provider to issue; zae
		// cannot read it, and saying so beats guessing.
		fmt.Fprintln(stdout, "  identity    cannot be read from this token (it is not a JWT) — the instance still validates it")
	default:
		if how == meOlder {
			fmt.Fprintln(stdout, "  answered    by the token itself — this portal-api does not say who a bearer is (GET /api/portal/me)")
		}
		fmt.Fprintf(stdout, "  username    %s\n", orUnset(w.claims.Username))
		fmt.Fprintf(stdout, "  admin role  %s\n", roleStateFor(w.claims, w.role))
	}
	if exp := w.expiry(); !exp.IsZero() {
		fmt.Fprintf(stdout, "  expires     %s%s\n", exp.Local().Format(time.RFC1123), expiryNote(exp, !w.env && w.entry.RefreshToken != ""))
	}
}

// listed says whether the instance lists a role for this bearer.
func listed(roles []string, role string) string {
	for _, r := range roles {
		if r == role {
			return fmt.Sprintf("%q — yes, the instance lists it", role)
		}
	}
	return fmt.Sprintf("%q — no, the instance does not list it", role)
}

// whoamiDoc is whoami as --json prints it. From says who answered: the
// instance, or — against an older or unreachable one — the token itself.
type whoamiDoc struct {
	URL       string   `json:"url"`
	Bearer    string   `json:"bearer"`
	Issuer    string   `json:"issuer,omitempty"`
	ClientID  string   `json:"clientId,omitempty"`
	Subject   string   `json:"subject,omitempty"`
	Username  string   `json:"username,omitempty"`
	Roles     []string `json:"roles"`
	Admin     bool     `json:"admin"`
	AdminRole string   `json:"adminRole,omitempty"`
	From      string   `json:"from"`
	Expires   string   `json:"expires,omitempty"`
}

func (w *whoami) printJSON() {
	d := whoamiDoc{URL: w.base, Bearer: "session", Roles: []string{}}
	if w.env {
		d.Bearer = instance.TokenEnv
	} else {
		d.Issuer, d.ClientID = w.entry.Issuer, w.entry.ClientID
	}
	if w.jwt {
		d.Subject = w.claims.Subject
	}
	switch {
	case w.me != nil:
		d.From, d.Username, d.Admin, d.AdminRole = "instance", w.me.Username, *w.me.IsAdmin, w.me.AdminRole
		if w.me.Roles != nil {
			d.Roles = w.me.Roles
		}
		if w.env && w.me.Client != "" {
			d.ClientID = w.me.Client
		}
	case w.jwt:
		d.From, d.Username, d.Admin, d.AdminRole = "token", w.claims.Username, w.claims.HasRole(w.role), w.role
		if w.claims.Roles != nil {
			d.Roles = w.claims.Roles
		}
	default:
		d.From = "nothing"
	}
	if exp := w.expiry(); !exp.IsZero() {
		d.Expires = exp.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(d)
	fmt.Fprintln(stdout, string(b))
}

func orUnset(s string) string {
	if s == "" {
		return "(not in the token)"
	}
	return s
}

func identity(c *oidc.Claims) string {
	switch {
	case c.Username != "" && c.Subject != "":
		return fmt.Sprintf("%s (%s)", c.Username, c.Subject)
	case c.Username != "":
		return c.Username
	case c.Subject != "":
		return c.Subject
	default:
		return "(the token names nobody)"
	}
}

func roleState(c *oidc.Claims) string { return roleStateFor(c, instance.AdminRole) }

func roleStateFor(c *oidc.Claims, role string) string {
	if c.HasRole(role) {
		return fmt.Sprintf("yes — the token carries %q", role)
	}
	return fmt.Sprintf("no — the token does not carry %q, so admin commands will be refused (exit 5)", role)
}

func expiryNote(exp time.Time, renewable bool) string {
	if time.Now().Before(exp) {
		if renewable {
			return " (renewed automatically)"
		}
		return ""
	}
	if renewable {
		return " — expired; the next command renews it"
	}
	return " — expired; run zae login again"
}
