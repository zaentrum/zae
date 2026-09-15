package addon

import (
	"strings"
	"testing"
)

// A secret input file loses exactly one trailing newline — the one an editor
// or `echo` adds — and nothing else.
func TestReadSecretFileTrimsOneNewline(t *testing.T) {
	for content, want := range map[string]string{
		"s3cret\n":     "s3cret",
		"s3cret\r\n":   "s3cret",
		"s3cret":       "s3cret",
		"s3cret\n\n":   "s3cret\n",
		"line1\nline2": "line1\nline2",
	} {
		got, err := readSecretFile(writeFile(t, "secret", content))
		if err != nil || got != want {
			t.Errorf("readSecretFile(%q) = %q, %v; want %q", content, got, err, want)
		}
	}
}

// Nothing a malformed secret document produces may quote what is in it.
func TestParseSecretValuesNeverEchoes(t *testing.T) {
	if m, err := parseSecretValues([]byte(`{"database.password":"hunter2-secret","api.key":"k"}`), "secrets.json"); err != nil || m["api.key"] != "k" {
		t.Fatalf("a valid document must parse: %v %v", m, err)
	}
	for doc, wantErr := range map[string]string{
		`{"database.password": hunter2-secret}`:  "not valid JSON (at byte",
		`["hunter2-secret"]`:                     "not one JSON object",
		`{"database.password": 42}`:              "is not a string",
		`{"database.password": ""}`:              "is empty",
		`{"bad path": "hunter2-secret"}`:         "not a dotted path",
		`{"zaentrum.x": "hunter2-secret"}`:       "set by the platform",
		`{"a": "hunter2-secret"} {"b": "x"}`:     "more than one JSON value",
		`{"a": "hunter2-secret", "b": {"c": 1}}`: "is not a string",
	} {
		_, err := parseSecretValues([]byte(doc), "secrets.json")
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("parseSecretValues(%s): want an error containing %q, got %v", doc, wantErr, err)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error quotes the secret: %v", err)
		}
	}
}

func TestCheckSecretArgs(t *testing.T) {
	ok := []secretArg{{arg: "database.password"}, {arg: "api.key=value"}, {file: true, arg: "tls.key=/tmp/key"}}
	if err := checkSecretArgs(ok, []string{"old.token"}); err != nil {
		t.Fatalf("valid arguments refused: %v", err)
	}
	if got := prompted(ok); len(got) != 1 || got[0] != "database.password" {
		t.Fatalf("prompted: %q", got)
	}
	for name, tc := range map[string]struct {
		args  []secretArg
		clear []string
	}{
		"empty value":       {args: []secretArg{{arg: "a.b="}}},
		"file without path": {args: []secretArg{{file: true, arg: "a.b"}}},
		"file without name": {args: []secretArg{{file: true, arg: "a.b="}}},
		"path too long":     {args: []secretArg{{arg: strings.Repeat("a", 251) + "=x"}}},
		"bad path":          {args: []secretArg{{arg: "a..b=x"}}},
		"platform path":     {args: []secretArg{{arg: "zaentrum.issuer=x"}}},
		"set and cleared":   {args: []secretArg{{arg: "a.b=x"}}, clear: []string{"a.b"}},
		"bad clear":         {clear: []string{"a b"}},
	} {
		if err := checkSecretArgs(tc.args, tc.clear); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	if err := checkSecretArgs([]secretArg{{arg: strings.Repeat("a", 250) + "=x"}}, nil); err != nil {
		t.Errorf("a 250-character path is allowed: %v", err)
	}
}

func TestParseSecretRefs(t *testing.T) {
	refs, err := parseSecretRefs("example", []string{"database.password=zaentrum-addon-example-values-x7k2p/database.password"})
	if err != nil || refs["database.password"] != (secretRef{Name: "zaentrum-addon-example-values-x7k2p", Key: "database.password"}) {
		t.Fatalf("parseSecretRefs: %v %v", refs, err)
	}
	for _, arg := range []string{
		"database.password", // no target
		"database.password=zaentrum-addon-example-values-x7k2p", // no key
		"database.password=platform-db/password",                // not the addon's values Secret
		"database.password=zaentrum-addon-other-values-x/key",   // another addon's
		"database.password=Zaentrum-addon-example-values-x/key", // not a Secret name
		"database.password=zaentrum-addon-example-values-x/k y", // not a Secret key
		"bad path=zaentrum-addon-example-values-x/key",
	} {
		if _, err := parseSecretRefs("example", []string{arg}); err == nil {
			t.Errorf("parseSecretRefs(%q) must be refused", arg)
		}
	}
}
