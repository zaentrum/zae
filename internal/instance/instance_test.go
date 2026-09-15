package instance

import (
	"net/http"
	"strings"
	"testing"
)

func TestBase(t *testing.T) {
	for raw, want := range map[string]string{
		"https://media.example.org":    "https://media.example.org",
		"https://media.example.org/":   "https://media.example.org",
		"http://127.0.0.1:8080//":      "http://127.0.0.1:8080",
		"https://example.org/zaentrum": "https://example.org/zaentrum",
	} {
		if got, err := Base(raw); err != nil || got != want {
			t.Errorf("Base(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "media.example.org", "ftp://example.org", "https://"} {
		if _, err := Base(raw); err == nil {
			t.Errorf("Base(%q) must be refused", raw)
		}
	}
}

// The hint must say what to DO: send a bearer when none was sent, and never
// suggest sending one when the one sent was refused.
func TestForbiddenHint(t *testing.T) {
	t.Setenv(TokenEnv, "")
	if h := ForbiddenHint("needs admin"); !strings.Contains(h, "supply a bearer via "+TokenEnv) {
		t.Fatalf("no token: %q", h)
	}
	t.Setenv(TokenEnv, "tok")
	if h := ForbiddenHint("needs admin"); !strings.Contains(h, "was refused — needs admin") {
		t.Fatalf("refused token: %q", h)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.org", nil)
	Authorize(req)
	if req.Header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("bearer not attached: %q", req.Header.Get("Authorization"))
	}
}
