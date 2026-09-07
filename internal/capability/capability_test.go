package capability

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
}

// The three outcomes must stay distinguishable by error identity, because the
// CLI maps them to exit codes without reading message text.
func TestFetchOutcomes(t *testing.T) {
	ok := serve(200, `{"capabilityVersion":1,"services":[{"service":"a","kind":"addon"}]}`)
	defer ok.Close()
	if d, err := Fetch(context.Background(), ok.URL); err != nil || len(d.Services) != 1 {
		t.Fatalf("healthy fetch: %v %+v", err, d)
	}

	old := serve(404, "")
	defer old.Close()
	if _, err := Fetch(context.Background(), old.URL); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("404 must be ErrUnsupported, got %v", err)
	}

	broken := serve(200, "not json")
	defer broken.Close()
	if _, err := Fetch(context.Background(), broken.URL); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("garbage must be ErrUnreachable (could not find out), got %v", err)
	}

	if _, err := Fetch(context.Background(), "http://127.0.0.1:1"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("dead host must be ErrUnreachable, got %v", err)
	}

	newer := serve(200, `{"capabilityVersion":2,"services":[]}`)
	defer newer.Close()
	d, err := Fetch(context.Background(), newer.URL)
	if !errors.Is(err, ErrSchema) || d == nil {
		t.Fatalf("newer schema must be ErrSchema and still return the document, got %v %v", err, d)
	}
}

// Exact matching only. Fuzzy-matching "wantd" to "wanted" would run a command
// the script did not name.
func TestFindIsExact(t *testing.T) {
	d := &Document{Services: []Descriptor{{Service: "acquire", Commands: []Command{{Name: "wanted"}}}}}
	if lk := d.Find("acquire", "wanted"); lk.Command == nil {
		t.Fatal("exact match must resolve")
	}
	if lk := d.Find("acquire", "wantd"); lk.Service == nil || lk.Command != nil {
		t.Fatal("near-miss command must resolve the service but not the command")
	}
	if lk := d.Find("Acquire", "wanted"); lk.Service != nil {
		t.Fatal("service names are case-sensitive; no guessing")
	}
}
