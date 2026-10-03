package debug

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/zae/internal/exitcode"
)

// exampleBundle is the portal's support bundle, as an older portal-api might
// assemble it: one credential left in a log.
const exampleBundle = `{
  "kind": "zaentrum-support-bundle",
  "version": 1,
  "generatedAt": "2026-10-03T12:00:00Z",
  "namespace": "zaentrum",
  "sections": {
    "config": {"oidcIssuer": "https://media.example.org/auth/realms/zaentrum", "adminRole": "zaentrum-admin"},
    "registry": {"apps": [], "spaces": [], "tiles": []},
    "pods": [{"pod": "postgres-0", "phase": "Running", "containers": ["postgres"]}],
    "logs": {"postgres-0/postgres": "2026-10-03T12:00:00Z password=` + leaked + `\n", "_note": "truncated: support-bundle log size cap reached"}
  }
}`

func TestBundleWritesANewFileRedacted(t *testing.T) {
	p, srv := newPortal(t)
	p.bundle = exampleBundle
	path := filepath.Join(t.TempDir(), "support.json")
	code, out, errs := run(t, "bundle", "-o", path, "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if p.called("GET "+bundlePath+"?config=1&instances=1&kafka=1&logs=1&registry=1") != 1 {
		t.Fatalf("every section is asked for: %v", p.calls)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the bundle holds logs: 0600, got %v %v", fi, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), leaked) {
		t.Fatalf("the file carries a credential:\n%s", b)
	}
	var doc struct {
		Kind     string `json:"kind"`
		Sections struct {
			Client struct {
				Kind, Version, OS string
			} `json:"client"`
			Logs map[string]string `json:"logs"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.Kind != bundleKind {
		t.Fatalf("the file is the bundle, still JSON: %v\n%s", err, b)
	}
	if doc.Sections.Client.Kind != "zae" || doc.Sections.Client.Version != "v9.9.9-test" || doc.Sections.Client.OS == "" {
		t.Errorf("zae adds its own section, as the console adds the browser's: %+v", doc.Sections.Client)
	}
	if !strings.Contains(doc.Sections.Logs["postgres-0/postgres"], "password=***REDACTED***") {
		t.Errorf("the log stays, with the credential replaced: %q", doc.Sections.Logs["postgres-0/postgres"])
	}
	for _, s := range []string{"wrote " + path, "config, registry, pods, logs of 1 container (cut short", "client"} {
		if !strings.Contains(errs, s) {
			t.Errorf("the summary lacks %q: %q", s, errs)
		}
	}
	if out != "" {
		t.Errorf("with -o FILE nothing goes to stdout: %q", out)
	}
}

func TestBundleToStdoutAndWithoutSections(t *testing.T) {
	p, srv := newPortal(t)
	p.bundle = exampleBundle
	code, out, errs := run(t, "bundle", "-o", "-", "--without", "logs,kafka", "--without", "client", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d %s", code, errs)
	}
	if p.called("GET "+bundlePath+"?config=1&instances=1&kafka=0&logs=0&registry=1") != 1 {
		t.Fatalf("--without turns sections off: %v", p.calls)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout carries the bundle and nothing else: %v\n%s", err, out)
	}
	if _, ok := doc["sections"].(map[string]any)["client"]; ok {
		t.Errorf("--without client leaves zae's section out: %s", out)
	}
	if !strings.Contains(errs, "wrote stdout") {
		t.Errorf("the summary goes to stderr: %q", errs)
	}

	p.bundle = `<html>sign in</html>`
	if code, _, errs := run(t, "bundle", "-o", "-", "--url", srv.URL); code != exitcode.Undetermined || !strings.Contains(errs, "not a support bundle") {
		t.Fatalf("not a bundle: want 4, got %d %q", code, errs)
	}
}

func TestBundleUsageAndRefusals(t *testing.T) {
	p, srv := newPortal(t)
	existing := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(existing, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	usageCases(t, p, map[string][]string{
		"without -o":         {"bundle", "--url", srv.URL},
		"over a file":        {"bundle", "-o", existing, "--url", srv.URL},
		"no such directory":  {"bundle", "-o", filepath.Join(t.TempDir(), "no", "such", "dir", "b.json"), "--url", srv.URL},
		"an unknown section": {"bundle", "-o", "-", "--without", "secrets", "--url", srv.URL},
		"nothing left":       {"bundle", "-o", "-", "--without", "logs,instances,kafka,registry,config,client", "--url", srv.URL},
		"a positional":       {"bundle", "everything", "-o", "-", "--url", srv.URL},
	})
	if b, _ := os.ReadFile(existing); string(b) != "keep me" {
		t.Fatalf("an existing file was written over: %q", b)
	}
	p.token = "admin-bearer"
	if code, _, errs := run(t, "bundle", "-o", "-", "--url", srv.URL); code != exitcode.Forbidden || !strings.Contains(errs, adminNeed) {
		t.Errorf("without a bearer: want 5 naming the role, got %d %q", code, errs)
	}
}

// "Never over a file" holds even for one that appears while the portal
// assembles: the write itself refuses it.
func TestBundleIsWrittenToANewFileOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeNew(path, []byte(`{"kind":"zaentrum-support-bundle"}`)); err == nil {
		t.Fatal("a file that exists must not be written over")
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me" {
		t.Fatalf("the file was changed: %q", b)
	}
}

// JSON that is not a support bundle — another service's answer — is not
// written as one.
func TestBundleThatIsNotOne(t *testing.T) {
	p, srv := newPortal(t)
	p.bundle = `{"kind":"something-else","sections":{}}`
	if code, out, errs := run(t, "bundle", "-o", "-", "--url", srv.URL); code != exitcode.Undetermined || out != "" {
		t.Fatalf("want 4 and nothing written, got %d %q %q", code, out, errs)
	}
}
