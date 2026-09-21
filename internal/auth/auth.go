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
func Logout(args []string) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "instance whose stored session to remove")
	all := fs.Bool("all", false, "remove every stored session")
	if err := fs.Parse(args); err != nil {
		return exitcode.Usage
	}
	if *all {
		if *rawURL != "" {
			errf("usage: zae logout takes --url or --all, not both")
			return exitcode.Usage
		}
		n, err := creds.Clear()
		if err != nil {
			errf("failed: %v", err)
			return exitcode.Failed
		}
		path, _ := creds.Path()
		fmt.Fprintf(stdout, "removed %d stored session(s); %s is gone\n", n, path)
		return exitcode.OK
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		errf("usage: %v (or `zae logout --all`)", err)
		return exitcode.Usage
	}
	removed, err := creds.Remove(base)
	if err != nil {
		errf("failed: %v", err)
		return exitcode.Failed
	}
	if !removed {
		// Not an error: the end state the caller asked for is the one that holds.
		fmt.Fprintf(stdout, "no stored session for %s\n", base)
		return exitcode.OK
	}
	fmt.Fprintf(stdout, "removed the stored session for %s\n", base)
	if os.Getenv(instance.TokenEnv) != "" {
		fmt.Fprintf(stdout, "note: %s is still set in this shell, and still wins over a stored session\n", instance.TokenEnv)
	}
	return exitcode.OK
}

// Whoami runs `zae whoami --url https://…`: who the bearer zae would send
// says it is, and whether it carries the platform's admin role.
//
// It reads the token's own claims and never prints the token. What it says is
// what the token CLAIMS; the instance verifies it on every call, which is why
// a `whoami` that looks right can still be refused — and why that refusal
// names a missing role rather than a missing login.
func Whoami(args []string) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	role := fs.String("role", instance.AdminRole, "the realm role to report on")
	if err := fs.Parse(args); err != nil {
		return exitcode.Usage
	}
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
	token, source := env, instance.TokenEnv
	if token == "" {
		if !stored {
			errf("forbidden: no credentials for %s — run: zae login --url %s, or supply a bearer via %s", base, base, instance.TokenEnv)
			return exitcode.Forbidden
		}
		token, source = entry.AccessToken, "the stored session"
	}

	fmt.Fprintf(stdout, "%s\n  bearer from %s\n", base, source)
	if stored && env != "" {
		fmt.Fprintf(stdout, "              (a stored session exists too; %s wins)\n", instance.TokenEnv)
	}
	if stored && env == "" {
		fmt.Fprintf(stdout, "  issuer      %s\n  client      %s\n", entry.Issuer, entry.ClientID)
	}

	claims, ok := oidc.ParseClaims(token)
	if !ok {
		// An opaque token is a legitimate thing for a provider to issue; zae
		// cannot read it, and saying so beats guessing.
		fmt.Fprintln(stdout, "  identity    cannot be read from this token (it is not a JWT) — the instance still validates it")
		return exitcode.OK
	}
	fmt.Fprintf(stdout, "  subject     %s\n", orUnset(claims.Subject))
	fmt.Fprintf(stdout, "  username    %s\n", orUnset(claims.Username))
	fmt.Fprintf(stdout, "  admin role  %s\n", roleStateFor(claims, *role))
	if !claims.Expiry.IsZero() {
		fmt.Fprintf(stdout, "  expires     %s%s\n", claims.Expiry.Local().Format(time.RFC1123), expiryNote(claims.Expiry, stored && env == "" && entry.RefreshToken != ""))
	}
	return exitcode.OK
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
