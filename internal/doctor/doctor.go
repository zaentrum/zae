// Package doctor is zae's static diagnostic core: the checks that must exist
// before the platform can describe itself, run OUTSIDE-IN — from where a user
// or client actually stands. Several failure classes are invisible from inside
// the cluster (an issuer that does not resolve publicly, a route that answers
// in-cluster but is not published, a registry that refuses anonymous pulls),
// and each check here corresponds to a real incident that outside-in probing
// would have caught in seconds.
//
// With --sign-in it goes one step further in, and no further than a person
// does: it signs in through the login page as the web client does, and uses
// the platform with that token — reading only (signin.go, deep.go). That is
// what lets a platform verify itself after an update: the operator runs this
// binary in the platform's namespace as a test account, and --report hands it
// the outcome as data (report.go).
package doctor

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Streams are variables so tests can read what a run printed — and prove that
// no credential is among it.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

// staticChecks is the outside-in suite every run starts with. A variable so
// the tests of what follows it do not reach the network beyond their own fakes
// (the registry check asks ghcr.io).
var staticChecks = func(base *url.URL, realm string) []Result {
	results := []Result{
		checkTLS(base),
		checkRoutes(base),
	}
	results = append(results, checkIssuer(base, realm)...)
	results = append(results, checkRegistry())
	return append(results, checkDiscovery(base))
}

// Result is one check's outcome. Remediation is part of the contract: a
// diagnostic that says only "degraded" makes the reader do the diagnosis —
// the exact failure mode doctor exists to end.
type Result struct {
	Name   string
	Status Status
	Detail string
	Fix    string // what to actually do; empty when Status is OK
}

type Status int

const (
	OK Status = iota
	Warn
	Fail
	Skip
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Fail:
		return "FAIL"
	default:
		return "skip"
	}
}

// client: short timeouts everywhere. Doctor must never hang — a hung check
// reads as a hung platform.
func client() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}

// Run executes the static check suite against a public instance URL and then
// asks the instance what registered checks it offers; with --sign-in it then
// signs in and uses the platform. Exit code: 1 if any check FAILed, else 0
// (warnings and skips do not fail the run).
func Run(args []string, version string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "public URL of the instance (required), e.g. https://media.example.org")
	realm := fs.String("realm", "zaentrum", "realm name used when the instance does not advertise its issuer")
	signIn := fs.Bool("sign-in", false, "also sign in the way a person does and use the platform with the token, read-only — as "+
		UserEnv+"/"+PasswordEnv+" when set, else with the session zae login stored")
	report := fs.String("report", "", "write a compact JSON summary of the run to this file — at most 4096 bytes, so it fits /dev/termination-log")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		// Go's flags stop at the first word that is not one, so everything
		// after it — a --sign-in, a --report — would be ignored without a word.
		fmt.Fprintf(stderr, "doctor: takes no arguments, only flags (%q, and every flag after it, would be ignored)\n", fs.Arg(0))
		return 2
	}
	if *rawURL == "" {
		fmt.Fprintln(stderr, "doctor: --url is required (the instance's public address)")
		return 2
	}
	base, err := url.Parse(strings.TrimRight(*rawURL, "/"))
	if err != nil || base.Host == "" {
		fmt.Fprintf(stderr, "doctor: %q is not a URL\n", *rawURL)
		return 2
	}

	fmt.Fprintf(stdout, "zae %s · doctor · %s\n\n", version, base)

	results := staticChecks(base, *realm)
	list(results)
	if *signIn {
		more := signedInChecks(context.Background(), base, version)
		fmt.Fprintln(stdout)
		list(more)
		results = append(results, more...)
	}

	if *report != "" {
		if err := writeReport(*report, version, base.String(), results); err != nil {
			// The run's verdict is the run's: a report that could not be written
			// is said, and does not change what the platform did.
			fmt.Fprintf(stderr, "doctor: could not write the report to %s: %v\n", *report, err)
		}
	}

	fmt.Fprintln(stdout)
	for _, r := range results {
		if r.Status == Fail {
			fmt.Fprintln(stdout, "doctor: FAILING — see fixes above")
			return 1
		}
	}
	fmt.Fprintln(stdout, "doctor: no failures")
	return 0
}

// list prints results the way doctor always has: a mark, the name, the
// detail, and the fix under it.
func list(results []Result) {
	for _, r := range results {
		mark := map[Status]string{OK: "✓", Warn: "!", Fail: "✗", Skip: "-"}[r.Status]
		fmt.Fprintf(stdout, "  %s %-28s %s\n", mark, r.Name, r.Detail)
		if r.Fix != "" {
			fmt.Fprintf(stdout, "      fix: %s\n", r.Fix)
		}
	}
}

// checkTLS reports certificate health for the public host. Expiry is the
// classic silent rot: nothing fails until everything does.
func checkTLS(base *url.URL) Result {
	if base.Scheme != "https" {
		return Result{Name: "tls", Status: Warn, Detail: "instance is plain http",
			Fix: "fine on a LAN profile; anything public should terminate TLS"}
	}
	host := base.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	conn, err := tls.Dial("tcp", host, &tls.Config{ServerName: base.Hostname()})
	if err != nil {
		return Result{Name: "tls", Status: Fail, Detail: err.Error(),
			Fix: "certificate rejected from the outside — check issuer chain and hostname coverage"}
	}
	defer conn.Close()
	left := time.Until(conn.ConnectionState().PeerCertificates[0].NotAfter)
	switch {
	case left < 0:
		return Result{Name: "tls", Status: Fail, Detail: "certificate is EXPIRED", Fix: "renew the certificate"}
	case left < 14*24*time.Hour:
		return Result{Name: "tls", Status: Warn,
			Detail: fmt.Sprintf("certificate expires in %d days", int(left.Hours()/24)),
			Fix:    "renew before it lapses"}
	}
	return Result{Name: "tls", Status: OK, Detail: fmt.Sprintf("certificate valid, %d days left", int(left.Hours()/24))}
}

// checkRoutes probes the published front door. It asserts "answers", not
// "returns 200": most surfaces redirect anonymous visitors to sign-in, and
// that IS healthy. What it must catch is the route that is not published at
// all — a 404 from the ingress, or no answer.
func checkRoutes(base *url.URL) Result {
	paths := []string{"/", "/portal", "/api/config"}
	var bad []string
	for _, p := range paths {
		resp, err := client().Get(base.String() + p)
		if err != nil {
			bad = append(bad, p+" ("+err.Error()+")")
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 500 || resp.StatusCode == 404 {
			bad = append(bad, fmt.Sprintf("%s (%d)", p, resp.StatusCode))
		}
	}
	if len(bad) > 0 {
		return Result{Name: "routes", Status: Fail, Detail: "not serving: " + strings.Join(bad, ", "),
			Fix: "check the ingress/route map — the path is not published or its backend is down"}
	}
	return Result{Name: "routes", Status: OK, Detail: fmt.Sprintf("%d public paths answer", len(paths))}
}

// checkIssuer hunts the number-one self-host boot failure: an OIDC issuer that
// clients cannot reach, or that names a different host than it is reached by.
// The platform advertises its issuer at /api/config; when that is not up we
// fall back to the bundled realm convention.
func checkIssuer(base *url.URL, realm string) []Result {
	issuer := ""
	if resp, err := client().Get(base.String() + "/api/config"); err == nil {
		var cfg struct {
			OIDCIssuer string `json:"oidcIssuer"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&cfg)
		resp.Body.Close()
		issuer = cfg.OIDCIssuer
	}
	src := "advertised by /api/config"
	if issuer == "" {
		issuer = base.String() + "/auth/realms/" + realm
		src = "assumed (bundled-realm convention)"
	}

	resp, err := client().Get(strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration")
	if err != nil {
		return []Result{{Name: "oidc issuer", Status: Fail,
			Detail: fmt.Sprintf("%s (%s) unreachable from here: %v", issuer, src, err),
			Fix:    "clients will fail exactly like this — the issuer must resolve and serve from where users stand"}}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return []Result{{Name: "oidc issuer", Status: Fail,
			Detail: fmt.Sprintf("%s (%s) answered %d to discovery", issuer, src, resp.StatusCode),
			Fix:    "issuer discovery must return 200; check the identity mode and realm name"}}
	}
	var disc struct {
		Issuer string `json:"issuer"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&disc)
	out := []Result{{Name: "oidc issuer", Status: OK, Detail: issuer + " serves discovery (" + src + ")"}}
	if disc.Issuer != "" && disc.Issuer != strings.TrimRight(issuer, "/") {
		// Not always fatal (external mode fronts another host on purpose), but
		// it is the signature of the issuer trap, so say it loudly.
		out = append(out, Result{Name: "issuer identity", Status: Warn,
			Detail: fmt.Sprintf("discovery names itself %q but is reached as %q", disc.Issuer, issuer),
			Fix:    "tokens are validated against the discovery value — every client and service must use that exact issuer"})
	}
	return out
}

// checkRegistry proves the platform's images are pullable anonymously, using
// one canary the platform cannot start without. A dead credential and a
// private image produce the same 403 at pull time; from the outside, anonymous
// is the only vantage that matters for a public platform.
func checkRegistry() Result {
	const repo = "zaentrum/portal-api"
	tokResp, err := client().Get("https://ghcr.io/token?scope=repository:" + repo + ":pull&service=ghcr.io")
	if err != nil {
		return Result{Name: "image registry", Status: Warn, Detail: "ghcr.io unreachable from here: " + err.Error(),
			Fix: "if your cluster has the same egress, pulls will fail there too"}
	}
	defer tokResp.Body.Close()
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&tok)
	req, _ := http.NewRequest("HEAD", "https://ghcr.io/v2/"+repo+"/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json")
	resp, err := client().Do(req)
	if err != nil {
		return Result{Name: "image registry", Status: Warn, Detail: err.Error()}
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return Result{Name: "image registry", Status: Fail,
			Detail: fmt.Sprintf("anonymous pull of ghcr.io/%s:latest -> %d", repo, resp.StatusCode),
			Fix:    "platform images must be publicly pullable; a node that cannot pull sits in ImagePullBackOff while old pods keep serving"}
	}
	return Result{Name: "image registry", Status: OK, Detail: "ghcr.io/" + repo + ":latest pulls anonymously"}
}
