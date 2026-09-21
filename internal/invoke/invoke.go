// Package invoke runs commands the INSTANCE declared: `zae <service> <command>`.
//
// Nothing here knows any service. It resolves the pair against the instance's
// discovery document, renders the declared HTTP call, makes it with the
// caller's own credentials against the instance's own origin, and maps every
// way that can go wrong onto the exit-code contract in package exitcode —
// most importantly keeping "definitively not offered" (3) apart from "could
// not find out" (4), because a script must react to those in opposite ways.
package invoke

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/capability"
	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// proxyPrefix is how a service's paths are reached from OUTSIDE the cluster:
// the portal's app proxy forwards /api/portal/apps/<key>/<path> to the
// registered app. Descriptor paths are service-relative by contract; the key
// defaults to the service name (a descriptor may override it with proxyKey).
const proxyPrefix = "/api/portal/apps/"

var placeholderRe = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

// multiFlag collects a repeatable --k=v flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// stderr is a variable so tests can capture messages.
var stderr io.Writer = os.Stderr
var stdout io.Writer = os.Stdout

func errf(format string, a ...any) { fmt.Fprintf(stderr, "zae: "+format+"\n", a...) }

// Run executes `zae <service> <command> [flags]`.
func Run(service, command string, args []string) int {
	fs := flag.NewFlagSet(service+" "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	data := fs.String("data", "", "JSON request body (POST/PUT/PATCH)")
	var params, queries multiFlag
	fs.Var(&params, "arg", "path placeholder value, name=value (repeatable)")
	fs.Var(&queries, "query", "query parameter, name=value (repeatable)")
	if err := fs.Parse(args); err != nil {
		return exitcode.Usage
	}
	if *rawURL == "" {
		errf("usage: --url is required (the instance whose commands you are running)")
		return exitcode.Usage
	}
	base := strings.TrimRight(*rawURL, "/")

	doc, code := discover(base)
	if code != exitcode.OK {
		return code
	}
	lk := doc.Find(service, command)
	if code := notOffered(lk, doc, base, service, command); code != exitcode.OK {
		return code
	}
	cmd := lk.Command

	// Render the declared call. Every {placeholder} must be supplied; guessing
	// a path parameter would mean running a command the script did not name.
	path, missing := fillPath(cmd.Path, params)
	if len(missing) > 0 {
		errf("usage: %s %s needs --arg for: %s", service, command, strings.Join(missing, ", "))
		return exitcode.Usage
	}
	method := strings.ToUpper(cmd.Method)
	if *data != "" && (method == http.MethodGet || method == http.MethodHead) {
		errf("usage: %s %s is a %s and takes no --data", service, command, method)
		return exitcode.Usage
	}
	target, err := url.Parse(base + proxyPrefix + proxyKey(lk.Service) + path)
	if err != nil {
		errf("usage: declared path %q is not a valid URL: %v", cmd.Path, err)
		return exitcode.Usage
	}
	if len(queries) > 0 {
		q := target.Query()
		for _, kv := range queries {
			k, v, _ := strings.Cut(kv, "=")
			q.Add(k, v)
		}
		target.RawQuery = q.Encode()
	}

	var body io.Reader
	if *data != "" {
		body = bytes.NewBufferString(*data)
	}
	ctx := context.Background()
	req, _ := http.NewRequestWithContext(ctx, method, target.String(), body)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := instance.Authorize(ctx, req, base); err != nil {
		// zae has credentials for this instance and could not make them
		// usable. That is an authentication failure, exit 5 — the command
		// itself was never in doubt.
		errf("forbidden: %s %s: %v", service, command, err)
		return exitcode.Forbidden
	}

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		errf("undetermined: %s %s could not be executed against %s: %v — not concluding anything about the command", service, command, base, err)
		return exitcode.Undetermined
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		fmt.Fprint(stdout, string(out))
		if len(out) > 0 && out[len(out)-1] != '\n' {
			fmt.Fprintln(stdout)
		}
		return exitcode.OK
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		need := ""
		if cmd.Role != "" {
			need = fmt.Sprintf("the command declares role %q", cmd.Role)
		}
		errf("forbidden: %s %s is declared by %s but the instance answered %d — %s", service, command, base, resp.StatusCode, instance.ForbiddenHint(base, need))
		return exitcode.Forbidden
	case resp.StatusCode == 404:
		// The instance's surface may have changed since discovery. Ask again
		// ONCE and reclassify: gone is exit 3; still declared but 404 is a real
		// server-side failure, exit 1. Never let a stale view read as an
		// inexplicable error.
		fresh, fcode := discover(base)
		if fcode == exitcode.OK {
			if again := fresh.Find(service, command); again.Command == nil {
				errf("not offered: %s no longer declares %s %s (it did when this run started; the instance's surface changed)", base, service, command)
				return exitcode.NotOffered
			}
		}
		errf("failed: %s %s is declared but %s answered 404 at %s — the service's proxy key may differ from its name", service, command, base, target.Path)
		return exitcode.Failed
	default:
		errf("failed: %s %s: HTTP %d from %s: %s", service, command, resp.StatusCode, base, excerpt(out))
		return exitcode.Failed
	}
}

// Require answers `zae require <service>[.<command>] --url U` with an exit
// code and nothing on stdout, so a script can assert its prerequisites before
// doing work rather than failing halfway through.
func Require(args []string) int {
	// The natural spelling is `zae require acquire.wanted --url …` — spec first.
	// Go's flag package stops at the first positional, so split the spec out
	// before parsing flags rather than forcing scripts into flag-first order.
	spec, flagArgs := "", make([]string, 0, len(args))
	for _, a := range args {
		if spec == "" && !strings.HasPrefix(a, "-") {
			spec = a
			continue
		}
		flagArgs = append(flagArgs, a)
	}
	fs := flag.NewFlagSet("require", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	if err := fs.Parse(flagArgs); err != nil {
		return exitcode.Usage
	}
	if spec == "" || fs.NArg() != 0 || *rawURL == "" {
		errf("usage: zae require <service>[.<command>] --url https://…")
		return exitcode.Usage
	}
	service, command, _ := strings.Cut(spec, ".")
	base := strings.TrimRight(*rawURL, "/")
	doc, code := discover(base)
	if code != exitcode.OK {
		return code
	}
	lk := doc.Find(service, command)
	if command == "" {
		if lk.Service == nil {
			errf("not offered: %s declares no service %q (%s)", base, service, declared(doc))
			return exitcode.NotOffered
		}
		return exitcode.OK
	}
	return notOffered(lk, doc, base, service, command)
}

// discover fetches and maps discovery failures onto the contract.
func discover(base string) (*capability.Document, int) {
	doc, err := capability.Fetch(context.Background(), base)
	switch {
	case err == nil:
		return doc, exitcode.OK
	case errors.Is(err, capability.ErrSchema):
		errf("contract mismatch: %v — upgrade zae", err)
		return nil, exitcode.ContractMismatch
	case errors.Is(err, capability.ErrUnsupported):
		errf("undetermined: %s does not implement capability discovery — zae cannot know what it offers (not concluding the command is gone)", base)
		return nil, exitcode.Undetermined
	default:
		// Print the whole error: the wrapped cause (dial refused, TLS, timeout) is
		// what a reader needs, and Unwrap would hand back only the sentinel.
		errf("undetermined: cannot reach capability discovery on %s (%v) — not concluding the command is gone", base, err)
		return nil, exitcode.Undetermined
	}
}

// notOffered maps a lookup miss onto exit 3 with a message that says what IS
// there — the difference between an error and a dead end.
func notOffered(lk capability.Lookup, doc *capability.Document, base, service, command string) int {
	if lk.Service == nil {
		errf("not offered: %s declares no service %q (%s). Try: zae discover --url %s", base, service, declared(doc), base)
		return exitcode.NotOffered
	}
	if lk.Command == nil {
		errf("not offered: service %q on %s declares no command %q (it declares: %s)", service, base, command, strings.Join(lk.Service.CommandNames(), ", "))
		return exitcode.NotOffered
	}
	return exitcode.OK
}

func declared(doc *capability.Document) string {
	names := doc.ServiceNames()
	if len(names) == 0 {
		return "no services declare capabilities"
	}
	return "services declaring capabilities: " + strings.Join(names, ", ")
}

func proxyKey(s *capability.Descriptor) string {
	if s.ProxyKey != "" {
		return s.ProxyKey
	}
	return s.Service
}

// fillPath substitutes {name} placeholders from --arg pairs and reports the
// ones that were not supplied.
func fillPath(path string, params []string) (string, []string) {
	vals := map[string]string{}
	for _, kv := range params {
		k, v, _ := strings.Cut(kv, "=")
		vals[k] = v
	}
	var missing []string
	out := placeholderRe.ReplaceAllStringFunc(path, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := vals[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		return url.PathEscape(v)
	})
	return out, missing
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}
