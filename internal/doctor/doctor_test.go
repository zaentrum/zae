package doctor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The issuer trap in miniature: discovery serves fine but names a different
// issuer than the one it is reached by. That mismatch is the signature of the
// number-one self-host boot failure, and it must surface as a warning — not
// pass silently because "the endpoint answered 200".
func TestIssuerIdentityMismatchWarns(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/config":
			fmt.Fprintf(w, `{"oidcIssuer":%q}`, srvURL+"/auth/realms/test")
		case "/auth/realms/test/.well-known/openid-configuration":
			// Discovery names an issuer host that is NOT how we reached it.
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer": "https://sso.internal.example/auth/realms/test",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	base, _ := url.Parse(srv.URL)
	results := checkIssuer(base, "test")
	if len(results) != 2 {
		t.Fatalf("want reachability + identity results, got %d: %+v", len(results), results)
	}
	if results[0].Status != OK {
		t.Fatalf("discovery is served, reachability must be OK: %+v", results[0])
	}
	if results[1].Status != Warn {
		t.Fatalf("issuer identity mismatch must WARN: %+v", results[1])
	}
}

// External-identity instances legitimately advertise an issuer on another
// host. Reachable-and-consistent discovery there must be a clean OK with no
// second result — a doctor that cries wolf trains people to ignore it.
func TestConsistentIssuerIsQuiet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/realms/zaentrum/.well-known/openid-configuration" {
			host := "http://" + r.Host
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": host + "/auth/realms/zaentrum"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL)
	results := checkIssuer(base, "zaentrum")
	if len(results) != 1 || results[0].Status != OK {
		t.Fatalf("consistent issuer must be a single OK: %+v", results)
	}
}

// No issuer reachable at all: FAIL with a fix, never a panic and never OK.
func TestUnreachableIssuerFails(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	results := checkIssuer(base, "zaentrum")
	if len(results) != 1 || results[0].Status != Fail || results[0].Fix == "" {
		t.Fatalf("unreachable issuer must FAIL with a fix: %+v", results)
	}
}
