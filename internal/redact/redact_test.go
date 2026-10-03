package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// The shapes the portal redacts, and the prose it must leave alone. A line
// must read the same in the console and in a terminal, so these are the
// portal's own cases.
func TestSecrets(t *testing.T) {
	for name, c := range map[string]struct {
		in         string
		mustRedact []string // must NOT survive
		keep       []string // must survive
	}{
		"a basic auth header (base64 of user:pass)": {
			in: `Authorization: Basic cG9ydGFsOnMzY3JldA==`, mustRedact: []string{"cG9ydGFsOnMzY3JldA=="}, keep: []string{"Basic"}},
		"bare basic base64": {
			in: `curl -H 'basic cG9ydGFsOnMzY3JldA==' http://x`, mustRedact: []string{"cG9ydGFsOnMzY3JldA=="}},
		"a bearer in an authorization header": {
			in: `Authorization: Bearer sk-9f8a7b6c5d4e3f2a1b0c9d8e`, mustRedact: []string{"sk-9f8a7b6c5d4e3f2a1b0c9d8e"}, keep: []string{"Bearer"}},
		"bearer prose": {
			in: `oidc discovery succeeded; bearer verification active`, keep: []string{"bearer verification active"}},
		"a JWT on its own": {
			in: `token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyLTEifQ.c2lnbmF0dXJl seen`, mustRedact: []string{"eyJzdWIiOiJ1c2VyLTEifQ"}, keep: []string{"seen"}},
		"a postgres dsn with @ in the password": {
			in: `dsn postgres://portal:p@ssw0rd@host:5432/db loaded`, mustRedact: []string{"ssw0rd"},
			keep: []string{"postgres://portal:", "@host:5432/db", "loaded"}},
		"a redis dsn without a user": {
			in: `REDIS_URL=redis://:mypassw0rd@valkey:6379/0`, mustRedact: []string{"mypassw0rd"}, keep: []string{"redis://:", "@valkey:6379/0"}},
		"key=value with commas in the secret": {
			in: `password=aaa,bbb,ccc next=field`, mustRedact: []string{"aaa,bbb,ccc", "bbb", "ccc"}, keep: []string{"password", "next=field"}},
		"a quoted JSON pair": {
			in: `{"clientSecret":"s3cr3t-value","name":"web"}`, mustRedact: []string{"s3cr3t-value"}, keep: []string{`"name":"web"`}},
		"an ordinary line": {
			in: `keyframe.uploaded item=49131b58 kind=backdrop bytes=176656`, keep: []string{"keyframe.uploaded", "49131b58", "176656"}},
	} {
		got := Secrets(c.in)
		for _, r := range c.mustRedact {
			if strings.Contains(got, r) {
				t.Errorf("%s: %q survived: %q", name, r, got)
			}
		}
		for _, k := range c.keep {
			if !strings.Contains(got, k) {
				t.Errorf("%s: %q must survive, got %q", name, k, got)
			}
		}
	}
}

// zae scrubs what the portal already scrubbed. The second pass must change
// nothing — a marker matched again would turn every redacted line into noise.
func TestSecretsTwiceIsOnce(t *testing.T) {
	for _, in := range []string{
		`Authorization: Bearer sk-9f8a7b6c5d4e3f2a1b0c9d8e`,
		`password=aaa,bbb,ccc next=field`,
		`dsn postgres://portal:p@ssw0rd@host:5432/db loaded`,
		`token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyLTEifQ.c2lnbmF0dXJl`,
		`curl -H 'basic cG9ydGFsOnMzY3JldA==' http://x`,
	} {
		once := Secrets(in)
		if twice := Secrets(once); twice != once {
			t.Errorf("a second pass changed %q into %q", once, twice)
		}
	}
}

// JSON redacts values and leaves a document that still parses — which a
// pattern run over the text cannot promise: here a secret is followed by an
// escaped quote, whose backslash the value pattern would swallow.
func TestJSONRedactsValuesAndStaysJSON(t *testing.T) {
	raw := []byte(`{"line":"token=abc\"def","count":12345678901234567890,"nested":[{"dsn":"postgres://u:pw@db:5432/x"}],` +
		`"html":"<b>&</b>","clientSecret":"two words","token-service-6d4f/app":"a plain log line","secretKeys":["database.password"]}`)
	out, err := JSON(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("the result is not JSON any more: %v\n%s", err, out)
	}
	s := string(out)
	for _, gone := range []string{"abc", "pw@", "two words", "words"} {
		if strings.Contains(s, gone) {
			t.Errorf("%q survived: %s", gone, s)
		}
	}
	for _, kept := range []string{
		`12345678901234567890`, `"<b>&</b>"`, "postgres://u:", "@db:5432/x",
		// A key with a credential's word in it is not a credential's name
		// when the text pass would not read it as one: a pod's log, keyed by
		// pod and container, stays a log.
		`"token-service-6d4f/app":"a plain log line"`,
		// What a key names is redacted only when it is a string: a list of
		// secret input PATHS is not a list of secrets.
		`"secretKeys":["database.password"]`,
	} {
		if !strings.Contains(s, kept) {
			t.Errorf("%q must survive as written: %s", kept, s)
		}
	}
	if !strings.Contains(s, `"clientSecret":"***REDACTED***"`) {
		t.Errorf("a credential's value goes whole, not to its first space: %s", s)
	}
	if _, err := JSON([]byte(`{"open":`), ""); err == nil {
		t.Error("a document that does not parse is an error, not an empty answer")
	}
}
