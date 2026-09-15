// Package instance is what every zae command that talks to an instance
// shares: how the instance is named on the command line, how the caller's
// credentials travel, and what to say when the instance refuses them. One
// place, so that `zae login` replaces the stopgap once and not per command.
package instance

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// TokenEnv is the stopgap until `zae login` exists: a bearer minted elsewhere
// (a service account, a browser session) is honored as-is. It is documented
// as exactly that — a stopgap — not hidden.
const TokenEnv = "ZAE_TOKEN"

// Authorize attaches the caller's bearer to req when one is set.
func Authorize(req *http.Request) {
	if tok := os.Getenv(TokenEnv); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// ForbiddenHint says what to do about a 401 or 403: supply a bearer when none
// was sent, or — when one was — that it was refused, followed by need (what
// the call requires, e.g. a role) when the caller knows it.
func ForbiddenHint(need string) string {
	if os.Getenv(TokenEnv) == "" {
		return "supply a bearer via " + TokenEnv + " (zae login is not available yet)"
	}
	hint := "the token in " + TokenEnv + " was refused"
	if need != "" {
		hint += " — " + need
	}
	return hint
}

// Base validates --url and returns the instance's origin without a trailing
// slash. The error is phrased for a usage message.
func Base(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("--url is required (the instance's public address, e.g. https://media.example.org)")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("--url %q is not an http(s) address like https://media.example.org", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}
