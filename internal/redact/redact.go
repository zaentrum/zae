// Package redact is the portal's redaction, applied once more on this side of
// the wire.
//
// Every debug view zae reads — container logs, the event tap, the support
// bundle — is scrubbed by portal-api before it leaves the cluster. zae scrubs
// it again with the SAME rules before it prints or writes a byte: a portal-api
// older than a fix to those rules, or a field the portal does not scrub (an
// event's key), must not put a credential in a terminal, a scrollback buffer
// or a file someone attaches to a bug report.
//
// The rules are the portal's (its redact package), kept identical on purpose
// so that a line reads the same in the console and in a terminal. They are
// best-effort by design — the common credential shapes, layered on admin-only
// access, never instead of it — and applying them twice changes nothing: every
// marker they write is outside every shape they match.
package redact

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// The portal's patterns, unchanged.
var (
	// key: value / key=value / "key":"value" where the KEY looks like a
	// credential. The unquoted value runs to whitespace, a quote or a brace —
	// not to a comma: a secret may contain commas, and stopping at the first
	// one would leak its tail.
	secretKV = regexp.MustCompile(`(?i)([a-z0-9_.-]*(?:password|passwd|secret|token|apikey|api_key|access[_-]?key|private[_-]?key|client[_-]?secret|credential)[a-z0-9_.-]*)("?\s*[:=]\s*"?)([^\s"'}]+)`)
	// The value of an Authorization or Proxy-Authorization header, any scheme:
	// the scheme stays, the credential goes. Basic <base64> decodes straight
	// to user:password.
	authHeader = regexp.MustCompile(`(?i)((?:proxy-)?authorization"?\s*[:=]\s*"?(?:bearer|basic|negotiate|digest)\s+)[^\s"']+`)
	// A bare "bearer <token>" or "basic <base64>" outside such a header. Sixteen
	// characters at least, so prose ("bearer verification active") survives.
	bearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{16,}`)
	basic  = regexp.MustCompile(`(?i)(basic\s+)[A-Za-z0-9+/=_-]{16,}`)
	// A JWT: three base64url segments.
	jwt = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`)
	// Credentials inside a URI (scheme://[user]:pass@host). The user is
	// optional — redis://:pass@ is Redis's own form — and the password runs to
	// the LAST '@' before the host, so an '@' inside it leaves no tail.
	uriCreds = regexp.MustCompile(`(?i)((?:postgres(?:ql)?|redis|amqp|mongodb|https?)://[^:@/\s]*:)[^\s"']+(@[^@/\s]+)`)
	// secretKey is a whole object key that secretKV would read as a
	// credential's name: the same words, the same characters around them.
	secretKey = regexp.MustCompile(`(?i)^[a-z0-9_.-]*(?:password|passwd|secret|token|apikey|api_key|access[_-]?key|private[_-]?key|client[_-]?secret|credential)[a-z0-9_.-]*$`)
)

// Secrets returns s with credential-shaped values replaced by markers, in the
// portal's order.
func Secrets(s string) string {
	s = authHeader.ReplaceAllString(s, `${1}***REDACTED***`)
	s = bearer.ReplaceAllString(s, `${1}***REDACTED***`)
	s = basic.ReplaceAllString(s, `${1}***REDACTED***`)
	s = jwt.ReplaceAllString(s, `***REDACTED-JWT***`)
	s = secretKV.ReplaceAllString(s, `${1}${2}***REDACTED***`)
	s = uriCreds.ReplaceAllString(s, `${1}***REDACTED***${2}`)
	return s
}

// JSON redacts every string value in a JSON document and encodes it again,
// indented by indent ("" for one line).
//
// It works on the decoded document, never on its text: a pattern run over
// serialized JSON can swallow the backslash of an escaped quote and leave a
// document that no longer parses. Numbers keep their spelling, and nothing
// is HTML-escaped, so a value reads as the portal wrote it.
func JSON(raw []byte, indent string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return Encode(Value(v), indent)
}

// Value redacts every string inside a decoded JSON value, in place where it
// can, and returns the result. A string under a key that names a credential —
// {"clientSecret": "…"} — goes whole: it is the pair the portal's pass over
// the text reads as key=value. Keys themselves are structure, and stay.
func Value(v any) any {
	switch t := v.(type) {
	case string:
		return Secrets(t)
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok && s != "" && secretKey.MatchString(k) {
				t[k] = marker
				continue
			}
			t[k] = Value(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = Value(x)
		}
		return t
	}
	return v
}

// marker is what a redacted value reads as.
const marker = "***REDACTED***"

// Encode writes v as JSON without HTML escaping, indented by indent.
func Encode(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
