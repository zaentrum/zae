package doctor

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// An instance installed a minute ago has no library yet, and is not broken
// for it: every check that needs a title, a person or a package says so, and
// nothing fails.
func TestAnEmptyInstancePasses(t *testing.T) {
	w := newWorld(t)
	w.movies, w.series, w.people, w.packaged = []map[string]any{}, []map[string]any{}, []map[string]any{}, []string{}
	w.launchpad = func(rw http.ResponseWriter) { writeJSON(rw, map[string]any{"spaces": []any{}}) }
	signedIn(t)
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 0 || strings.Contains(out, "✗") {
		t.Fatalf("an empty instance must pass, got %d\n%s", code, out)
	}
	if !strings.Contains(line(out, "chino-api: items"), "an empty catalog") {
		t.Errorf("the empty catalog is said: %s", line(out, "chino-api: items"))
	}
	for _, name := range []string{"chino-api: item detail", "chino-api: poster", "chino-api: person", "chino-api: portrait", "chino-api: playback"} {
		if mark(out, name) != "-" {
			t.Errorf("%s needs data the instance does not have — a skip: %s", name, line(out, name))
		}
	}
	// The search still has to answer, data or not.
	if mark(out, "chino-api: people search") != "✓" || w.called("GET "+peoplePath) != 1 {
		t.Errorf("the people search runs on an empty catalog: %s", line(out, "chino-api: people search"))
	}
	// The portal's own page is checked even when the launchpad lists nothing.
	if mark(out, "app /portal/") != "✓" {
		t.Errorf("the portal is checked: %s", line(out, "app /portal/"))
	}
}

// A catalog of series only: the list falls through to them.
func TestASeriesOnlyCatalog(t *testing.T) {
	w := newWorld(t)
	w.movies = []map[string]any{}
	w.series = []map[string]any{{"id": "m1", "type": "series", "title": "Pioneer One"}}
	signedIn(t)
	_, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if l := line(out, "chino-api: items"); !strings.Contains(l, "no movies; 1 series on the first page") {
		t.Fatalf("the series are found: %s", l)
	}
	if mark(out, "chino-api: item detail") != "✓" {
		t.Errorf("the series opens: %s", line(out, "chino-api: item detail"))
	}
}

// Each deep check, broken in the way that check exists to catch.
func TestDeepChecksCatchWhatTheyAreFor(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(w *world)
		check  string
		mark   string
		detail string
		fix    string
	}{
		{"the catalog behind chino-api is down", func(w *world) {
			w.items = func(rw http.ResponseWriter, r *http.Request) bool {
				rw.Header().Set("Content-Type", "application/json")
				rw.WriteHeader(http.StatusBadGateway)
				_, _ = rw.Write([]byte(`{"error":"catalog unavailable"}`))
				return true
			}
		}, "chino-api: items", "✗", "HTTP 502", "katalog-api"},
		{"a credit without a person", func(w *world) {
			w.details["m1"]["cast"] = []map[string]any{{"person_id": "p1", "name": "Thom Hoffman", "role": "actor"}, {"name": "Nobody", "role": "actor"}}
		}, "chino-api: item detail", "✗", "1 of 2 credits are malformed — credit 2 has no person_id", "person_id"},
		{"a role that is not a token", func(w *world) {
			w.details["m1"]["cast"] = []map[string]any{{"person_id": "p1", "name": "Thom Hoffman", "role": "Lead Actor"}}
		}, "chino-api: item detail", "✗", `has the role "Lead Actor"`, "role"},
		{"a title the list names and the detail does not", func(w *world) { delete(w.details, "m1") },
			"chino-api: item detail", "✗", "its detail answers 404", "different catalogs"},
		{"a title without a poster", func(w *world) { w.posters["m1"] = false },
			"chino-api: poster", "-", `"Sintel" has no poster`, ""},
		{"a portrait the record claims and the store lost", func(w *world) { w.portraits["p1"] = false },
			"chino-api: portrait", "✗", "its profile_url answers 404", "artwork is missing"},
		{"a person without a portrait", func(w *world) {
			w.persons["p1"]["has_profile"] = false
			delete(w.persons["p1"], "profile_url")
		}, "chino-api: portrait", "-", "has no portrait", ""},
		{"a people total that is not the list's", func(w *world) { n := 7; w.peopleTotal = &n },
			"chino-api: people search", "✗", "total says 7, and the list holds 1", "length of the list"},
		{"a credited person the search cannot find", func(w *world) { w.people = []map[string]any{} },
			"chino-api: people search", "!", "does not find them", "name index"},
		{"an app whose bundle is missing behind a single-page fallback", func(w *world) {
			w.pages["/chino/assets/index-abc.js"] = "<!doctype html><html></html>"
			delete(w.assets, "/chino/assets/index-abc.js")
		}, "app /chino/", "✗", "single-page fallback", "stale page"},
		{"an app whose bundle answers 404", func(w *world) { w.assets["/katalog/assets/index-abc.js"] = "" },
			"app /katalog/", "✗", "answers HTTP 404", "did not ship its assets"},
		{"a module script with the wrong type", func(w *world) { w.assets["/chino/assets/index-abc.js"] = "application/octet-stream" },
			"app /chino/", "✗", "browsers refuse to run it", "text/javascript"},
		{"an app that is not published", func(w *world) { delete(w.pages, "/katalog/") },
			"app /katalog/", "✗", "answered HTTP 404", "not published"},
		{"the launchpad is failing", func(w *world) {
			w.launchpad = func(rw http.ResponseWriter) { http.Error(rw, "boom", http.StatusInternalServerError) }
		}, "portal: launchpad", "✗", "only the portal's own page is checked", "logs"},
		{"the console refuses a viewer, in its own words", func(w *world) {
			w.graphql = func(rw http.ResponseWriter, r *http.Request) { http.Error(rw, "forbidden", http.StatusForbidden) }
		}, "katalog-manager: graphql", "✓", "refused for this account (HTTP 403", ""},
		{"the console's refusal is a gateway's page", func(w *world) {
			w.graphql = func(rw http.ResponseWriter, r *http.Request) {
				rw.Header().Set("Content-Type", "text/html")
				rw.WriteHeader(http.StatusForbidden)
				_, _ = rw.Write([]byte("<html><body>Access denied</body></html>"))
			}
		}, "katalog-manager: graphql", "!", "does not answer like the service", "gateway"},
		{"the console's API is failing", func(w *world) {
			w.graphql = func(rw http.ResponseWriter, r *http.Request) {
				http.Error(rw, "upstream", http.StatusServiceUnavailable)
			}
		}, "katalog-manager: graphql", "✗", "HTTP 503", "not ready"},
		{"the console's route reaches something else", func(w *world) {
			w.graphql = func(rw http.ResponseWriter, r *http.Request) {
				rw.Header().Set("Content-Type", "text/html")
				_, _ = rw.Write([]byte("<html>the portal</html>"))
			}
		}, "katalog-manager: graphql", "✗", "not GraphQL", "something other than katalog-manager"},
		{"packages listed and gone from disk", func(w *world) {
			w.packaged = []string{"a", "b", "c", "d", "m1"}
			w.gone = map[string]bool{"a": true, "b": true, "c": true, "d": true}
		}, "chino-api: playback", "✗", "3 titles listed as packaged answered 404", "packages volume"},
		{"a package that went a moment ago", func(w *world) {
			w.packaged = []string{"a", "m1"}
			w.gone = map[string]bool{"a": true}
		}, "chino-api: playback", "!", "1 title listed as packaged answered 404 first", "lags behind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			c.setup(w)
			signedIn(t)
			code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
			if got := mark(out, c.check); got != c.mark {
				t.Fatalf("%s: want %s, got %q (%s)\n%s", c.check, c.mark, got, line(out, c.check), out)
			}
			if !strings.Contains(line(out, c.check), c.detail) {
				t.Errorf("the line lacks %q: %s", c.detail, line(out, c.check))
			}
			if c.fix != "" && !strings.Contains(fixOf(out, c.check), c.fix) {
				t.Errorf("the fix lacks %q: %q", c.fix, fixOf(out, c.check))
			}
			if wantCode := map[string]int{"✗": 1, "!": 0, "✓": 0, "-": 0}[c.mark]; code != wantCode {
				t.Errorf("exit %d, want %d\n%s", code, wantCode, out)
			}
			if writes := w.writes(); len(writes) != 0 {
				t.Errorf("the run wrote to the instance: %v", writes)
			}
		})
	}
}

// A hang is a failure, and a bounded one: doctor never hangs with the
// platform.
func TestAHangingAPIFailsInBoundedTime(t *testing.T) {
	old := requestTimeout
	requestTimeout = 200 * time.Millisecond
	defer func() { requestTimeout = old }()
	w := newWorld(t)
	release := make(chan struct{})
	defer close(release)
	w.graphql = func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	signedIn(t)
	start := time.Now()
	code, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if code != 1 || mark(out, "katalog-manager: graphql") != "✗" || !strings.Contains(line(out, "katalog-manager: graphql"), "no answer") {
		t.Fatalf("a hang must fail, got %d\n%s", code, out)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the run took %s — a hang must cost one timeout", time.Since(start))
	}
}

// Credentials never travel off the instance: a poster on another origin is
// not fetched with the token.
func TestTheTokenStaysOnTheInstance(t *testing.T) {
	w := newWorld(t)
	w.movies[0]["poster_url"] = "https://cdn.example.org/posters/m1.jpg"
	signedIn(t)
	_, out, _ := doctor(t, "--url", w.srv.URL, "--sign-in")
	if mark(out, "chino-api: poster") != "-" || !strings.Contains(line(out, "chino-api: poster"), "points away from the instance") {
		t.Fatalf("an off-instance poster is not fetched with the token: %s", line(out, "chino-api: poster"))
	}
}
