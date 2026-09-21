package invoke

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zaentrum/zae/internal/capability"
	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// A fake instance: discovery declares one service with two commands, and the
// proxied paths behave as the test dictates. Every exit code in the contract
// gets exercised against it.
func fakeInstance(t *testing.T, discoveryStatus int, schema int, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(capability.Path, func(w http.ResponseWriter, r *http.Request) {
		if discoveryStatus != 200 {
			w.WriteHeader(discoveryStatus)
			return
		}
		fmt.Fprintf(w, `{"capabilityVersion":%d,"services":[{"service":"sample","kind":"addon",
		  "commands":[
		    {"name":"list","summary":"list things","method":"GET","path":"/api/things"},
		    {"name":"get","summary":"one thing","method":"GET","path":"/api/things/{id}"},
		    {"name":"create","summary":"make one","method":"POST","path":"/api/things","role":"admin"}
		  ]}]}`, schema)
	})
	for p, h := range handlers {
		mux.HandleFunc(proxyPrefix+"sample"+p, h)
	}
	return httptest.NewServer(mux)
}

// Credentials are ambient: a stored session, or a ZAE_TOKEN in the shell
// running `go test`, would change what these tests see. Point the credentials
// file at a directory that starts empty and cannot be the developer's own.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zae-invoke-test-config-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Unsetenv(instance.TokenEnv)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func capture(t *testing.T, fn func() int) (code int, out, errs string) {
	t.Helper()
	var ob, eb bytes.Buffer
	stdout, stderr = &ob, &eb
	defer func() { stdout, stderr = os.Stdout, os.Stderr }()
	code = fn()
	return code, ob.String(), eb.String()
}

func TestRunsAndPrintsBody(t *testing.T) {
	srv := fakeInstance(t, 200, 1, map[string]http.HandlerFunc{
		"/api/things": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[{"id":1}]`) },
	})
	defer srv.Close()
	code, out, _ := capture(t, func() int { return Run("sample", "list", []string{"--url", srv.URL}) })
	if code != exitcode.OK || strings.TrimSpace(out) != `[{"id":1}]` {
		t.Fatalf("want 0 + body, got %d %q", code, out)
	}
}

func TestPlaceholderAndQuery(t *testing.T) {
	var seen string
	srv := fakeInstance(t, 200, 1, map[string]http.HandlerFunc{
		"/api/things/": func(w http.ResponseWriter, r *http.Request) { seen = r.URL.String(); fmt.Fprint(w, `{}`) },
	})
	defer srv.Close()
	code, _, _ := capture(t, func() int {
		return Run("sample", "get", []string{"--url", srv.URL, "--arg", "id=a b", "--query", "expand=yes"})
	})
	if code != exitcode.OK || !strings.Contains(seen, "/api/things/a%20b?expand=yes") {
		t.Fatalf("placeholder/query not rendered: code=%d url=%q", code, seen)
	}
}

// Usage errors are the SCRIPT's fault and must be exit 2 — never confused with
// the instance's answer.
func TestUsageErrors(t *testing.T) {
	srv := fakeInstance(t, 200, 1, nil)
	defer srv.Close()
	cases := map[string][]string{
		"missing --url":       {},
		"missing placeholder": {"--url", srv.URL},
	}
	for name, args := range cases {
		cmd := "list"
		if name == "missing placeholder" {
			cmd = "get"
		}
		code, _, errs := capture(t, func() int { return Run("sample", cmd, args) })
		if code != exitcode.Usage || !strings.Contains(errs, "usage") {
			t.Errorf("%s: want exit 2 with a usage message, got %d %q", name, code, errs)
		}
	}
}

// The heart of the contract: not-offered is DEFINITIVE and says what is there.
func TestNotOfferedIsDefinitiveAndInformative(t *testing.T) {
	srv := fakeInstance(t, 200, 1, nil)
	defer srv.Close()

	code, _, errs := capture(t, func() int { return Run("nosuch", "list", []string{"--url", srv.URL}) })
	if code != exitcode.NotOffered || !strings.Contains(errs, `declares no service "nosuch"`) || !strings.Contains(errs, "sample") {
		t.Fatalf("absent service: want 3 naming what IS declared, got %d %q", code, errs)
	}
	code, _, errs = capture(t, func() int { return Run("sample", "lst", []string{"--url", srv.URL}) })
	if code != exitcode.NotOffered || !strings.Contains(errs, `declares no command "lst"`) || !strings.Contains(errs, "list, get, create") {
		t.Fatalf("absent command: want 3 listing the declared commands, got %d %q", code, errs)
	}
	if strings.Contains(errs, "Usage:") {
		t.Fatalf("a dynamic miss must not dump usage: %q", errs)
	}
}

// Could-not-find-out is exit 4 and must SAY it is not concluding removal.
func TestUndeterminedNeverReadsAsRemoved(t *testing.T) {
	// unreachable
	code, _, errs := capture(t, func() int { return Run("sample", "list", []string{"--url", "http://127.0.0.1:1"}) })
	if code != exitcode.Undetermined || !strings.Contains(errs, "not concluding") {
		t.Fatalf("unreachable: want 4 + 'not concluding', got %d %q", code, errs)
	}
	// instance predates discovery
	srv := fakeInstance(t, 404, 1, nil)
	defer srv.Close()
	code, _, errs = capture(t, func() int { return Run("sample", "list", []string{"--url", srv.URL}) })
	if code != exitcode.Undetermined || !strings.Contains(errs, "does not implement") {
		t.Fatalf("no discovery: want 4, got %d %q", code, errs)
	}
}

func TestSchemaMismatchIsExit6(t *testing.T) {
	srv := fakeInstance(t, 200, 2, nil)
	defer srv.Close()
	code, _, errs := capture(t, func() int { return Run("sample", "list", []string{"--url", srv.URL}) })
	if code != exitcode.ContractMismatch || !strings.Contains(errs, "upgrade zae") {
		t.Fatalf("want 6 + upgrade hint, got %d %q", code, errs)
	}
}

func TestForbiddenIsExit5(t *testing.T) {
	srv := fakeInstance(t, 200, 1, map[string]http.HandlerFunc{
		"/api/things": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) },
	})
	defer srv.Close()
	code, _, errs := capture(t, func() int { return Run("sample", "list", []string{"--url", srv.URL}) })
	if code != exitcode.Forbidden || !strings.Contains(errs, instance.TokenEnv) {
		t.Fatalf("want 5 mentioning the token env, got %d %q", code, errs)
	}
}

func TestServerErrorIsExit1(t *testing.T) {
	srv := fakeInstance(t, 200, 1, map[string]http.HandlerFunc{
		"/api/things": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) },
	})
	defer srv.Close()
	code, _, errs := capture(t, func() int { return Run("sample", "list", []string{"--url", srv.URL}) })
	if code != exitcode.Failed || !strings.Contains(errs, "boom") {
		t.Fatalf("want 1 with the server's message, got %d %q", code, errs)
	}
}

// Stale-view honesty: a 404 on execution re-discovers once. If the command is
// now gone that is exit 3, not a confusing server error.
func TestExec404ReclassifiesWhenSurfaceChanged(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc(capability.Path, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 { // first discovery: command present
			fmt.Fprint(w, `{"capabilityVersion":1,"services":[{"service":"sample","kind":"addon","commands":[{"name":"list","method":"GET","path":"/api/things"}]}]}`)
			return
		}
		fmt.Fprint(w, `{"capabilityVersion":1,"services":[]}`) // then it is gone
	})
	mux.HandleFunc(proxyPrefix+"sample/api/things", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	code, _, errs := capture(t, func() int { return Run("sample", "list", []string{"--url", srv.URL}) })
	if code != exitcode.NotOffered || !strings.Contains(errs, "no longer declares") {
		t.Fatalf("want 3 'no longer declares', got %d %q", code, errs)
	}
}

func TestRequire(t *testing.T) {
	srv := fakeInstance(t, 200, 1, nil)
	defer srv.Close()
	for spec, want := range map[string]int{
		"sample":        exitcode.OK,
		"sample.list":   exitcode.OK,
		"sample.nope":   exitcode.NotOffered,
		"missing":       exitcode.NotOffered,
		"missing.thing": exitcode.NotOffered,
	} {
		code, out, _ := capture(t, func() int { return Require([]string{spec, "--url", srv.URL}) })
		if code != want || out != "" {
			t.Errorf("require %s: want %d with empty stdout, got %d stdout=%q", spec, want, code, out)
		}
	}
	code, _, _ := capture(t, func() int { return Require([]string{"sample", "--url", "http://127.0.0.1:1"}) })
	if code != exitcode.Undetermined {
		t.Errorf("require against unreachable: want 4, got %d", code)
	}
}
