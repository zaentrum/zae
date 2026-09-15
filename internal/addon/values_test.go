package addon

import (
	"encoding/json"
	"flag"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// regexpLine reports whether any line of out matches re.
func regexpLine(out, re string) bool { return regexp.MustCompile("(?m)" + re).MatchString(out) }

func TestParseChart(t *testing.T) {
	for _, tc := range []struct {
		raw, version string
		want         chartRef
	}{
		{"oci://ghcr.io/example/charts/example", "1.2.0", chartRef{"oci://ghcr.io/example/charts/example", "1.2.0", true}},
		{"oci://ghcr.io/example/charts/example:1.2.0", "", chartRef{"oci://ghcr.io/example/charts/example", "1.2.0", true}},
		{"oci://ghcr.io/example/charts/example:1.2.0", "1.2.0", chartRef{"oci://ghcr.io/example/charts/example", "1.2.0", true}},
		{"oci://registry.example.org:5000/example", "0.1.0", chartRef{"oci://registry.example.org:5000/example", "0.1.0", true}},
		{"https://example.org/charts/example-1.2.0.tgz", "", chartRef{"https://example.org/charts/example-1.2.0.tgz", "", false}},
	} {
		got, err := parseChart(tc.raw, tc.version)
		if err != nil || got != tc.want {
			t.Errorf("parseChart(%q, %q) = %+v, %v; want %+v", tc.raw, tc.version, got, err, tc.want)
		}
	}
	for _, raw := range []string{
		"oci://ghcr.io/example/charts/example",        // no version anywhere
		"oci://ghcr.io",                               // no repository
		"http://example.org/charts/example-1.2.0.tgz", // not https
		"https://example.org",                         // no archive
		"example",                                     // not a reference
	} {
		if _, err := parseChart(raw, ""); err == nil {
			t.Errorf("parseChart(%q) must be refused", raw)
		}
	}
	if _, err := parseChart("oci://ghcr.io/example/charts/example:1.2.0", "1.3.0"); err == nil {
		t.Error("a tag and a different --version must be refused, not guessed between")
	}
}

func TestDefaultName(t *testing.T) {
	for ref, want := range map[string]string{
		"oci://ghcr.io/example/charts/example":                  "example",
		"oci://ghcr.io/example/charts/example:1.2.0":            "example",
		"oci://registry.example.org:5000/example-worker":        "example-worker",
		"https://example.org/charts/example-1.2.0.tgz":          "example",
		"https://example.org/charts/example-worker-1.2.0.tgz":   "example-worker",
		"https://example.org/charts/example-2-1.0.0.tgz":        "example-2",
		"https://example.org/charts/example-1.0.0-rc.1.tgz":     "example",
		"https://example.org/charts/example-1.0.0-2.tgz":        "example",
		"https://example.org/dl/example.tar.gz?sig=abc#section": "example",
	} {
		if got := defaultName(ref); got != want {
			t.Errorf("defaultName(%q) = %q, want %q", ref, got, want)
		}
	}
}

// A --set value is JSON when it parses as JSON and a string otherwise; quotes
// force a string, which is how a version like 1.10 stays one.
func TestSetValueTyping(t *testing.T) {
	for in, want := range map[string]any{
		"2":           json.Number("2"),
		"1.10":        json.Number("1.10"),
		"true":        true,
		"null":        nil,
		`"1.10"`:      "1.10",
		`"true"`:      "true",
		"worker":      "worker",
		"True":        "True",
		"":            "",
		"2 3":         "2 3",
		"nullable":    "nullable",
		`["a","b"]`:   []any{"a", "b"},
		`{"k":1}`:     map[string]any{"k": json.Number("1")},
		"ghcr.io/x:1": "ghcr.io/x:1",
	} {
		if got := setValue(in); !reflect.DeepEqual(got, want) {
			t.Errorf("setValue(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestSetPath(t *testing.T) {
	values := map[string]any{"worker": map[string]any{"logLevel": "info"}, "tag": "1.0"}
	if err := setPath(values, "worker.replicas", json.Number("2")); err != nil {
		t.Fatal(err)
	}
	if err := setPath(values, "config.nested.key", "v"); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(values)
	if string(b) != `{"config":{"nested":{"key":"v"}},"tag":"1.0","worker":{"logLevel":"info","replicas":2}}` {
		t.Fatalf("setPath: %s", b)
	}
	if err := setPath(values, "tag.major", "1"); err == nil || !strings.Contains(err.Error(), "tag is already a value") {
		t.Fatalf("setting below a scalar must be refused: %v", err)
	}
	if v, ok := lookup(values, "config.nested.key"); !ok || v != "v" {
		t.Fatalf("lookup: %v %v", v, ok)
	}
	if _, ok := lookup(values, "tag.major"); ok {
		t.Fatal("lookup below a scalar must miss")
	}
}

// A malformed --set-secret must never echo what might be the secret.
func TestAssignmentNeverEchoesTheValue(t *testing.T) {
	for _, arg := range []string{"hunter2-secret", "bad path=hunter2-secret", "zaentrum.x=hunter2-secret"} {
		_, _, err := assignment("set-secret", arg)
		if err == nil {
			t.Errorf("assignment(%q) must be refused", arg)
			continue
		}
		if strings.Contains(err.Error(), "hunter2-secret") {
			t.Errorf("the error echoes the value: %v", err)
		}
	}
	if p, v, err := assignment("set", "a.b-c.d_e=x=y"); err != nil || p != "a.b-c.d_e" || v != "x=y" {
		t.Fatalf("assignment: %q %q %v", p, v, err)
	}
}

func TestParseValues(t *testing.T) {
	if m, err := parseValues([]byte(` {"worker":{"replicas":2}} `), "values.json"); err != nil || m["worker"] == nil {
		t.Fatalf("a JSON object must parse: %v %v", m, err)
	}
	for in, wantErr := range map[string]string{
		"worker:\n  replicas: 2\n":      "JSON only",
		`["a"]`:                         "not an object",
		`{"a":1} {"b":2}`:               "more than one JSON value",
		`{"a":`:                         "not valid JSON",
		"   ":                           "empty",
		`{"zaentrum":{"hostname":"x"}}`: "set by the platform",
	} {
		if _, err := parseValues([]byte(in), "values"); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("parseValues(%q): want an error containing %q, got %v", in, wantErr, err)
		}
	}
}

func TestParseTakesFlagsAndPositionalsInAnyOrder(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	url := fs.String("url", "", "")
	yes := fs.Bool("yes", false, "")
	pos, _, ok := parse(fs, []string{"--url", "https://example.org", "first", "--yes", "second", "--", "--third"})
	if !ok || *url != "https://example.org" || !*yes || !reflect.DeepEqual(pos, []string{"first", "second", "--third"}) {
		t.Fatalf("parse: ok=%v url=%q yes=%v pos=%q", ok, *url, *yes, pos)
	}
}

func TestSchemaInputs(t *testing.T) {
	inputs, err := schemaInputs(schemaDoc(exampleSchema))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]input{}
	var paths []string
	for _, in := range inputs {
		got[in.path] = in
		paths = append(paths, in.path)
	}
	if !reflect.DeepEqual(paths, []string{"config.key", "database.host", "database.password", "worker.logLevel", "worker.replicas"}) {
		t.Fatalf("inputs by path (the platform key is not one): %v", paths)
	}
	if in := got["database.password"]; !in.secret || !in.required || in.generate != "" {
		t.Errorf("database.password: %+v", in)
	}
	if in := got["config.key"]; !in.secret || in.generate != "random-base64-32" {
		t.Errorf("config.key: %+v", in)
	}
	if in := got["worker.replicas"]; in.secret || in.required || string(in.def) != "1" {
		t.Errorf("worker.replicas: %+v", in)
	}
	if in, err := schemaInputs(""); in != nil || err != nil {
		t.Errorf("a chart without a schema has no inputs: %v %v", in, err)
	}
}

func TestSchemaDocAcceptsStringOrObject(t *testing.T) {
	var p struct {
		S schemaDoc `json:"valuesSchema"`
	}
	for _, body := range []string{`{"valuesSchema":"{\"type\":\"object\"}"}`, `{"valuesSchema":{"type":"object"}}`} {
		if err := json.Unmarshal([]byte(body), &p); err != nil || !strings.Contains(string(p.S), `"type"`) {
			t.Errorf("%s: %q %v", body, p.S, err)
		}
	}
}

func TestPortString(t *testing.T) {
	for raw, want := range map[string]string{
		`8080`:       "8080",
		`"8080/TCP"`: "8080/TCP",
		`{"name":"http","containerPort":8080,"protocol":"TCP"}`: "http 8080/TCP",
		`{"port":80}`: "80",
	} {
		if got := portString(json.RawMessage(raw)); got != want {
			t.Errorf("portString(%s) = %q, want %q", raw, got, want)
		}
	}
}
