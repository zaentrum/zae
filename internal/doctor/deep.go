package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The deep checks: the platform used with the token, the way its web client
// and its catalog console use it.
//
// Every one of them is a READ — GETs, and one GraphQL query — and every one is
// DATA-INDEPENDENT: an empty instance passes. A check that needs data the
// instance does not have (a title, a person, a portrait, a packaged title) is
// a skip that says so, never a failure, because an instance installed a
// minute ago is not broken for having no library yet.

const (
	launchpadPath = "/api/portal/launchpad"
	// portalMount is the portal shell itself, which the launchpad does not
	// list because it IS the launchpad.
	portalMount  = "/portal/"
	itemsPath    = "/api/v1/items"
	peoplePath   = "/api/v1/people"
	packagedPath = "/api/v1/play/packaged-ids"
	// consoleAPI is where the catalog console posts its GraphQL.
	consoleAPI = "/api/manage/query"
	// allCaps claims every codec, so a packaged title is served from its
	// package: the playlist is read off disk and nothing is transcoded for a
	// check. Without it a package in a codec the default set lacks would fall
	// back to an on-demand transcode, which is work, not a read.
	allCaps = "avc,hvc,av1,vp9,aac,mp3,opus,vorbis,ac3,eac3,aacmc"
)

// answer is one response, read as far as a check needs.
type answer struct {
	status   int
	ctype    string
	body     []byte
	size     int64
	at       *url.URL
	location string
}

// errOffInstance: a URL an API handed back points away from the instance, and
// the token is only ever sent to the instance itself.
var errOffInstance = errors.New("points away from the instance")

// target resolves a path or URL against the instance: a site-rooted path is
// the instance's own, an absolute URL is taken as it is.
func (s *session) target(ref string) (*url.URL, error) {
	if strings.HasPrefix(ref, "/") && !strings.HasPrefix(ref, "//") {
		return url.Parse(s.base.String() + ref)
	}
	return s.base.Parse(ref)
}

// get makes one GET. withToken sends the bearer, and only to the instance's
// own origin — a URL elsewhere is refused with errOffInstance instead.
func (s *session) get(ctx context.Context, ref, accept string, withToken bool, limit int64) (*answer, error) {
	return s.request(ctx, http.MethodGet, ref, accept, "", withToken, limit)
}

// post sends one JSON document — the catalog console's GraphQL query, which
// reads.
func (s *session) post(ctx context.Context, ref, doc string) (*answer, error) {
	return s.request(ctx, http.MethodPost, ref, "application/json", doc, true, 1<<20)
}

func (s *session) request(ctx context.Context, method, ref, accept, doc string, withToken bool, limit int64) (*answer, error) {
	u, err := s.target(ref)
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL", ref)
	}
	if withToken && !sameOrigin(u, s.base) {
		return nil, errOffInstance
	}
	var body io.Reader
	if doc != "" {
		body = strings.NewReader(doc)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.agent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if doc != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if withToken && s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("the answer broke off: %v", err)
	}
	size := resp.ContentLength
	if size < 0 {
		size = int64(len(b))
	}
	return &answer{status: resp.StatusCode, ctype: resp.Header.Get("Content-Type"), body: b, size: size,
		at: u, location: resp.Header.Get("Location")}, nil
}

// page GETs an app's page or asset — public, so without the token — and
// follows up to three redirects on the instance's own origin, as a browser
// would from a mount without its trailing slash.
func (s *session) page(ctx context.Context, ref string, limit int64) (*answer, error) {
	for hop := 0; ; hop++ {
		a, err := s.get(ctx, ref, "text/html,*/*", false, limit)
		if err != nil || a.status < 300 || a.status >= 400 || a.location == "" {
			return a, err
		}
		next, err := a.at.Parse(a.location)
		if err != nil || !sameOrigin(next, s.base) || hop >= 3 {
			return a, nil
		}
		ref = next.String()
	}
}

// apiFailure turns an answer that is not the expected 200 into a failure: what
// the instance said, and what that usually means.
func apiFailure(name, path string, a *answer, err error) Result {
	switch {
	case err != nil:
		return Result{Name: name, Status: Fail, Detail: path + ": no answer — " + err.Error(),
			Fix: "a hang or a refused connection: the service behind " + path + ", or its route, is down"}
	case a.status == http.StatusUnauthorized || a.status == http.StatusForbidden:
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%s answered HTTP %d: %s", path, a.status, excerpt(a)),
			Fix: "the API refused the token the identity provider issued: it validates tokens against another issuer or audience than the identity provider puts in them — compare its OIDC settings with the instance's " + configPath}
	case a.status >= 300 && a.status < 400:
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%s redirected (HTTP %d) to %s", path, a.status, a.location),
			Fix: "an API that redirects is answering with a sign-in page or a moved route"}
	case a.status == http.StatusNotFound:
		return Result{Name: name, Status: Fail, Detail: path + " answered 404: " + excerpt(a),
			Fix: "the route is not published, or the service behind it predates this endpoint"}
	case a.status == http.StatusBadGateway || a.status == http.StatusGatewayTimeout:
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%s answered HTTP %d: %s", path, a.status, excerpt(a)),
			Fix: "the service answered, and the one behind it did not — for chino-api, that is the catalog (katalog-api)"}
	case a.status == http.StatusServiceUnavailable:
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%s answered HTTP 503: %s", path, excerpt(a)),
			Fix: "the service, or one it depends on, is not ready: starting, or failing its readiness probe"}
	case a.status >= 500:
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%s answered HTTP %d: %s", path, a.status, excerpt(a)),
			Fix: "the service is failing — its logs say why"}
	}
	return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%s answered HTTP %d: %s", path, a.status, excerpt(a))}
}

// excerpt is what an answer said, for one line: an HTML page reduced to its
// text, everything clipped.
func excerpt(a *answer) string {
	s := string(a.body)
	if isHTML(a.ctype) || strings.HasPrefix(strings.TrimSpace(s), "<") {
		s = anyTag.ReplaceAllString(s, " ")
	}
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "(empty body)"
	}
	return clip(s, 120)
}

func isHTML(ctype string) bool { return strings.Contains(strings.ToLower(ctype), "text/html") }

func isJS(ctype string) bool {
	c := strings.ToLower(ctype)
	return strings.Contains(c, "javascript") || strings.Contains(c, "ecmascript")
}

func isImage(ctype string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ctype)), "image/")
}

// deepChecks runs everything the token is for.
func (s *session) deepChecks(ctx context.Context) []Result {
	var out []Result
	mounts, r := s.launchpad(ctx)
	out = append(out, r)
	for _, m := range mounts {
		out = append(out, s.app(ctx, m))
	}
	if s.noChino != "" {
		out = append(out, Result{Name: "chino-api", Status: Skip, Detail: "not run — " + s.noChino})
	} else {
		out = append(out, s.chino(ctx)...)
	}
	return append(out, s.catalogConsole(ctx))
}

// launchpad reads where the apps are mounted. The doctor compiles in no
// mount but the portal's own: the launchpad is where the instance says what
// it runs and where, and an app is checked where the launchpad sends people.
func (s *session) launchpad(ctx context.Context) ([]string, Result) {
	const name = "portal: launchpad"
	mounts := []string{portalMount}
	a, err := s.get(ctx, launchpadPath, "application/json", true, 1<<20)
	if err != nil || a.status != http.StatusOK {
		r := apiFailure(name, launchpadPath, a, err)
		r.Detail += " — only the portal's own page is checked"
		return mounts, r
	}
	var lp struct {
		Spaces []struct {
			Tiles []struct {
				Href     string `json:"href"`
				Disabled bool   `json:"disabled"`
				External bool   `json:"external"`
			} `json:"tiles"`
		} `json:"spaces"`
	}
	if err := json.Unmarshal(a.body, &lp); err != nil {
		return mounts, Result{Name: name, Status: Fail, Detail: launchpadPath + " answered something that is not the launchpad (" + a.ctype + ")",
			Fix: "the route for /api/portal must reach portal-api"}
	}
	tiles := 0
	for _, sp := range lp.Spaces {
		for _, t := range sp.Tiles {
			if t.Disabled || t.External || strings.TrimSpace(t.Href) == "" {
				continue
			}
			tiles++
			if m := s.mountOf(t.Href); m != "" && !contains(mounts, m) {
				mounts = append(mounts, m)
			}
		}
	}
	return mounts, Result{Name: name, Status: OK,
		Detail: fmt.Sprintf("%s open apps at %s", count(tiles, "tile", "tiles"), strings.Join(mounts[1:], ", "))}
}

// mountOf is the app a launchpad link opens: the first segment of its path on
// the instance's own origin ("/portal/app/x" is the portal's), "" for a link
// elsewhere — another site's app is not this platform's to verify.
func (s *session) mountOf(href string) string {
	u, err := s.base.Parse(strings.TrimSpace(href))
	if err != nil || !sameOrigin(u, s.base) {
		return ""
	}
	seg := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)[0]
	if seg == "" {
		return "/"
	}
	return "/" + seg + "/"
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// app checks that a mount serves its page, and that the page's own script (or
// stylesheet) is there: a page whose bundle is missing is a blank screen that
// a status code alone calls healthy.
func (s *session) app(ctx context.Context, mount string) Result {
	name := "app " + mount
	down := "the route for " + mount + " is not published, or the app's pods are down"
	a, err := s.page(ctx, mount, 1<<20)
	switch {
	case err != nil:
		return Result{Name: name, Status: Fail, Detail: mount + ": no answer — " + err.Error(), Fix: down}
	case a.status != http.StatusOK:
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%s answered HTTP %d", mount, a.status), Fix: down}
	case !isHTML(a.ctype):
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%s answered %s, not a page", mount, a.ctype),
			Fix: "the route for " + mount + " reaches something other than the app"}
	}
	asset, module := pageAsset(a)
	if asset == nil {
		return Result{Name: name, Status: Warn, Detail: mount + " serves a page that loads no script and no stylesheet",
			Fix: "a built single-page app names its bundle in its page — is this the app?"}
	}
	shown := asset.Path
	if !sameOrigin(asset, s.base) {
		shown = asset.String()
	}
	missing := "a stale page cached in front of a newer build, or a build that did not ship its assets"
	b, err := s.page(ctx, asset.String(), 256<<10)
	switch {
	case err != nil:
		return Result{Name: name, Status: Fail, Detail: shown + ": no answer — " + err.Error(), Fix: down}
	case b.status != http.StatusOK:
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("the page loads %s, which answers HTTP %d", shown, b.status), Fix: missing}
	case isHTML(b.ctype):
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("the page loads %s, which answers with a page — the single-page fallback, so the asset is missing", shown), Fix: missing}
	case module && !isJS(b.ctype):
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("the page loads %s as a module script, served as %q — browsers refuse to run it", shown, b.ctype),
			Fix: "serve scripts as text/javascript"}
	}
	return Result{Name: name, Status: OK, Detail: fmt.Sprintf("serves its page and %s (%s)", shown, mediaType(b.ctype))}
}

func mediaType(ctype string) string {
	if t, _, ok := strings.Cut(ctype, ";"); ok {
		return strings.TrimSpace(t)
	}
	return strings.TrimSpace(ctype)
}

// pageAsset is the one asset a page check loads: the module script that
// starts the app, else any script, else a stylesheet. module is true for the
// first, whose content type a browser insists on.
func pageAsset(a *answer) (*url.URL, bool) {
	tags := scanTags(string(a.body))
	pick := func(match func(t tag) (string, bool)) *url.URL {
		for _, t := range tags {
			if ref, ok := match(t); ok && strings.TrimSpace(ref) != "" {
				if u, err := a.at.Parse(strings.TrimSpace(ref)); err == nil {
					return u
				}
			}
		}
		return nil
	}
	if u := pick(func(t tag) (string, bool) {
		return t.attrs["src"], t.name == "script" && strings.EqualFold(t.attrs["type"], "module")
	}); u != nil {
		return u, true
	}
	if u := pick(func(t tag) (string, bool) { return t.attrs["src"], t.name == "script" }); u != nil {
		return u, false
	}
	return pick(func(t tag) (string, bool) {
		return t.attrs["href"], t.name == "link" && contains(strings.Fields(strings.ToLower(t.attrs["rel"])), "stylesheet")
	}), false
}

// The catalog's wire shapes, the parts the checks read. Pointers mark what
// must be present: absent and empty are different answers.
type catalogItem struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	Title     string      `json:"title"`
	PosterURL string      `json:"poster_url"`
	Cast      []castEntry `json:"cast"`
}

type castEntry struct {
	PersonID     string `json:"person_id"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	Order        *int   `json:"order"`
	EpisodeCount *int   `json:"episode_count"`
}

type personSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	HasProfile *bool  `json:"has_profile"`
	ProfileURL string `json:"profile_url"`
}

type personDetail struct {
	personSummary
	Items *[]catalogItem `json:"items"`
}

// chino runs the web client's API, page by page: a list, a title, its poster,
// a person found by name, their page, their portrait, a playlist.
func (s *session) chino(ctx context.Context) []Result {
	var out []Result
	item, r := s.items(ctx)
	out = append(out, r)
	detail, r := s.itemDetail(ctx, item)
	out = append(out, r)
	out = append(out, s.poster(ctx, item))
	hit, credited, r := s.peopleSearch(ctx, detail)
	out = append(out, r)
	person, r := s.person(ctx, hit, credited)
	out = append(out, r)
	out = append(out, s.portrait(ctx, person))
	return append(out, s.playback(ctx))
}

// items lists the catalog: movies first, series when there are none.
func (s *session) items(ctx context.Context) (*catalogItem, Result) {
	const name = "chino-api: items"
	kinds := []struct{ query, one, many string }{
		{"limit=5", "movie", "movies"},
		{"type=series&limit=5", "series", "series"},
	}
	var seen []string
	for _, k := range kinds {
		path := itemsPath + "?" + k.query
		a, err := s.get(ctx, path, "application/json", true, 4<<20)
		if err != nil || a.status != http.StatusOK {
			return nil, apiFailure(name, path, a, err)
		}
		var list struct {
			Items *[]catalogItem `json:"items"`
		}
		if err := json.Unmarshal(a.body, &list); err != nil || list.Items == nil {
			return nil, Result{Name: name, Status: Fail, Detail: path + " answered something that is not the item list (" + a.ctype + ")",
				Fix: "the route for /api must reach chino-api"}
		}
		for i, it := range *list.Items {
			if strings.TrimSpace(it.ID) == "" || strings.TrimSpace(it.Title) == "" {
				return nil, Result{Name: name, Status: Fail, Detail: fmt.Sprintf("item %d of %s has no id or no title", i+1, path),
					Fix: "every client keys a title by its id and shows its title — the catalog serves records without them"}
			}
		}
		if n := len(*list.Items); n > 0 {
			seen = append(seen, fmt.Sprintf("%s on the first page", count(n, k.one, k.many)))
			first := (*list.Items)[0]
			return &first, Result{Name: name, Status: OK, Detail: strings.Join(seen, "; ")}
		}
		seen = append(seen, "no "+k.many)
	}
	return nil, Result{Name: name, Status: OK, Detail: "an empty catalog: " + strings.Join(seen, ", ") + " — the checks that need a title are skipped"}
}

// roleToken is a credit's role as the web client reads it: an open
// vocabulary of lower-case tokens (actor, director, …).
var roleToken = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// castProblem names the first malformed credit, "" when all are well-formed:
// a person to link to, a name to show, a role to label it with.
func castProblem(cast []castEntry) string {
	bad, first := 0, ""
	for i, c := range cast {
		why := ""
		switch {
		case strings.TrimSpace(c.PersonID) == "":
			why = "has no person_id"
		case strings.TrimSpace(c.Name) == "":
			why = "has no name"
		case !roleToken.MatchString(c.Role):
			why = fmt.Sprintf("has the role %q, not a lower-case token", c.Role)
		case c.Order != nil && *c.Order < 0:
			why = "has a negative order"
		case c.EpisodeCount != nil && *c.EpisodeCount < 0:
			why = "has a negative episode_count"
		}
		if why != "" {
			bad++
			if first == "" {
				first = fmt.Sprintf("credit %d %s", i+1, why)
			}
		}
	}
	if bad == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d credits are malformed — %s", bad, len(cast), first)
}

// itemDetail opens the first title, with its credits.
func (s *session) itemDetail(ctx context.Context, it *catalogItem) (*catalogItem, Result) {
	const name = "chino-api: item detail"
	if it == nil {
		return nil, Result{Name: name, Status: Skip, Detail: "no title to open — the catalog is empty"}
	}
	path := itemsPath + "/" + url.PathEscape(it.ID) + "?include=people"
	a, err := s.get(ctx, path, "application/json", true, 4<<20)
	switch {
	case err == nil && a.status == http.StatusNotFound:
		return nil, Result{Name: name, Status: Fail, Detail: fmt.Sprintf("the list names %q, and its detail answers 404", it.Title),
			Fix: "the list and the detail read different catalogs — or the title was deleted between the two reads"}
	case err != nil || a.status != http.StatusOK:
		return nil, apiFailure(name, path, a, err)
	}
	var d catalogItem
	if err := json.Unmarshal(a.body, &d); err != nil || d.ID != it.ID {
		return nil, Result{Name: name, Status: Fail, Detail: fmt.Sprintf("the detail of %q is not that title's record", it.Title),
			Fix: "chino-api must answer /api/v1/items/{id} with the item it names"}
	}
	if p := castProblem(d.Cast); p != "" {
		return &d, Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%q: %s", d.Title, p),
			Fix: "the web client links a credit to the person's page by person_id and labels it by role — the catalog's credits, or chino-api's projection of them, are incomplete"}
	}
	if len(d.Cast) == 0 {
		return &d, Result{Name: name, Status: OK, Detail: fmt.Sprintf("%q opens, and credits nobody", d.Title)}
	}
	return &d, Result{Name: name, Status: OK, Detail: fmt.Sprintf("%q opens with %s, each with a person, a name and a role",
		d.Title, count(len(d.Cast), "credit", "credits"))}
}

// image fetches an image a record links to, with the token.
func (s *session) image(ctx context.Context, ref string) (*answer, error) {
	return s.get(ctx, ref, "image/avif,image/webp,image/*;q=0.8", true, 256<<10)
}

// poster loads the first title's poster, when it has one.
func (s *session) poster(ctx context.Context, it *catalogItem) Result {
	const name = "chino-api: poster"
	if it == nil {
		return Result{Name: name, Status: Skip, Detail: "no title, so no poster"}
	}
	if strings.TrimSpace(it.PosterURL) == "" {
		return Result{Name: name, Status: Skip, Detail: fmt.Sprintf("%q names no poster_url", it.Title)}
	}
	a, err := s.image(ctx, it.PosterURL)
	switch {
	case errors.Is(err, errOffInstance):
		return Result{Name: name, Status: Skip, Detail: fmt.Sprintf("%q's poster_url points away from the instance, and the token is not sent there", it.Title)}
	case err == nil && a.status == http.StatusNotFound:
		return Result{Name: name, Status: Skip, Detail: fmt.Sprintf("%q has no poster", it.Title)}
	case err != nil || a.status != http.StatusOK:
		return apiFailure(name, it.PosterURL, a, err)
	case !isImage(a.ctype):
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%q's poster_url answered %s, not an image", it.Title, mediaType(a.ctype)),
			Fix: "the artwork route reaches something other than the artwork"}
	}
	return Result{Name: name, Status: OK, Detail: fmt.Sprintf("%q: %s, %s", it.Title, mediaType(a.ctype), bytesOf(a.size))}
}

// peopleSearch searches by the name of the title's first credit, so the hit
// can be held against the credit — or, with no credit to take a name from,
// for a single letter: the search still has to answer.
func (s *session) peopleSearch(ctx context.Context, d *catalogItem) (*personSummary, *castEntry, Result) {
	const name = "chino-api: people search"
	q := "a"
	var credited *castEntry
	if d != nil {
		for i := range d.Cast {
			if d.Cast[i].PersonID != "" && strings.TrimSpace(d.Cast[i].Name) != "" {
				credited, q = &d.Cast[i], d.Cast[i].Name
				break
			}
		}
	}
	path := peoplePath + "?q=" + url.QueryEscape(q) + "&limit=5"
	a, err := s.get(ctx, path, "application/json", true, 1<<20)
	if err != nil || a.status != http.StatusOK {
		return nil, credited, apiFailure(name, path, a, err)
	}
	var res struct {
		People *[]personSummary `json:"people"`
		Total  *int             `json:"total"`
	}
	if err := json.Unmarshal(a.body, &res); err != nil || res.People == nil {
		return nil, credited, Result{Name: name, Status: Fail, Detail: path + " answered something that is not a list of people (" + a.ctype + ")",
			Fix: "the route for /api must reach chino-api"}
	}
	people := *res.People
	if res.Total != nil && *res.Total != len(people) {
		return nil, credited, Result{Name: name, Status: Fail, Detail: fmt.Sprintf("total says %d, and the list holds %d", *res.Total, len(people)),
			Fix: "chino-api answers total as the length of the list it sends"}
	}
	for i, p := range people {
		if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" {
			return nil, credited, Result{Name: name, Status: Fail, Detail: fmt.Sprintf("person %d of the answer has no id or no name", i+1),
				Fix: "the web client links a person by id and shows their name"}
		}
	}
	found := count(len(people), "person", "people")
	if credited == nil {
		var hit *personSummary
		for i := range people {
			if hit == nil || (people[i].HasProfile != nil && *people[i].HasProfile && (hit.HasProfile == nil || !*hit.HasProfile)) {
				hit = &people[i]
			}
		}
		return hit, nil, Result{Name: name, Status: OK, Detail: fmt.Sprintf("a search for %q answers %s", q, found)}
	}
	for i := range people {
		if people[i].ID == credited.PersonID {
			return &people[i], credited, Result{Name: name, Status: OK,
				Detail: fmt.Sprintf("%q, credited on %q, is found by name (%s)", q, d.Title, found)}
		}
	}
	return nil, credited, Result{Name: name, Status: Warn,
		Detail: fmt.Sprintf("%q is credited on %q, and a search for the name does not find them (%s)", q, d.Title, found),
		Fix:    "the search and the credits read different data — the catalog's name index lags behind its credits"}
}

// person opens the person found — or the credited one, when the search did
// not find them.
func (s *session) person(ctx context.Context, hit *personSummary, credited *castEntry) (*personDetail, Result) {
	const name = "chino-api: person"
	id, who := "", ""
	switch {
	case hit != nil:
		id, who = hit.ID, hit.Name
	case credited != nil:
		id, who = credited.PersonID, credited.Name
	default:
		return nil, Result{Name: name, Status: Skip, Detail: "no person to open — the catalog names none"}
	}
	path := peoplePath + "/" + url.PathEscape(id)
	a, err := s.get(ctx, path, "application/json", true, 4<<20)
	switch {
	case err == nil && a.status == http.StatusNotFound:
		return nil, Result{Name: name, Status: Fail, Detail: fmt.Sprintf("the catalog names %q, and their page answers 404", who),
			Fix: "the person's id in the credits or the search is not one the person route knows"}
	case err != nil || a.status != http.StatusOK:
		return nil, apiFailure(name, path, a, err)
	}
	var p personDetail
	if err := json.Unmarshal(a.body, &p); err != nil || p.ID != id || strings.TrimSpace(p.Name) == "" || p.Items == nil {
		return nil, Result{Name: name, Status: Fail, Detail: fmt.Sprintf("the page of %q is not that person's record (an id, a name, a filmography)", who),
			Fix: "chino-api must answer /api/v1/people/{id} with the person it names"}
	}
	return &p, Result{Name: name, Status: OK, Detail: fmt.Sprintf("%q, credited on %s", p.Name, count(len(*p.Items), "title", "titles"))}
}

// portrait loads the person's portrait, when their record says there is one.
func (s *session) portrait(ctx context.Context, p *personDetail) Result {
	const name = "chino-api: portrait"
	switch {
	case p == nil:
		return Result{Name: name, Status: Skip, Detail: "no person, so no portrait"}
	case p.HasProfile == nil:
		return Result{Name: name, Status: Skip, Detail: fmt.Sprintf("the record of %q says nothing about a portrait (no has_profile) — this chino-api predates it", p.Name)}
	case !*p.HasProfile:
		return Result{Name: name, Status: Skip, Detail: fmt.Sprintf("%q has no portrait", p.Name)}
	case strings.TrimSpace(p.ProfileURL) == "":
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%q has a portrait (has_profile), and no profile_url to load it from", p.Name),
			Fix: "chino-api sets profile_url whenever has_profile is true"}
	}
	a, err := s.image(ctx, p.ProfileURL)
	switch {
	case errors.Is(err, errOffInstance):
		return Result{Name: name, Status: Skip, Detail: fmt.Sprintf("the profile_url of %q points away from the instance, and the token is not sent there", p.Name)}
	case err == nil && a.status == http.StatusNotFound:
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%q has a portrait (has_profile), and its profile_url answers 404", p.Name),
			Fix: "the catalog says it holds the portrait and cannot serve it — the person's artwork is missing from the catalog manager's store"}
	case err != nil || a.status != http.StatusOK:
		return apiFailure(name, p.ProfileURL, a, err)
	case !isImage(a.ctype):
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("the profile_url of %q answered %s, not an image", p.Name, mediaType(a.ctype)),
			Fix: "the portrait route reaches something other than the artwork"}
	}
	return Result{Name: name, Status: OK, Detail: fmt.Sprintf("%q: %s, %s", p.Name, mediaType(a.ctype), bytesOf(a.size))}
}

// playback reads the master playlist of one packaged title. Only a packaged
// one: a title that is not packaged is transcoded on demand, which a check
// must not set off.
func (s *session) playback(ctx context.Context) Result {
	const name = "chino-api: playback"
	a, err := s.get(ctx, packagedPath, "application/json", true, 8<<20)
	if err != nil || a.status != http.StatusOK {
		return apiFailure(name, packagedPath, a, err)
	}
	var list struct {
		IDs *[]string `json:"ids"`
	}
	if err := json.Unmarshal(a.body, &list); err != nil || list.IDs == nil {
		return Result{Name: name, Status: Fail, Detail: packagedPath + " answered something that is not the list of packaged titles (" + a.ctype + ")",
			Fix: "chino-api forwards it to chino-stream, which must answer {\"ids\": […]}"}
	}
	if len(*list.IDs) == 0 {
		return Result{Name: name, Status: Skip, Detail: "no packaged title to play"}
	}
	// The list is served stale while it is refreshed, so a package removed a
	// moment ago can still be on it: one title that has gone is the list's
	// lag, said as a warning; every title tried gone is the packages missing.
	gone := 0
	var last *answer
	for _, id := range *list.IDs {
		if gone == maxGone {
			break
		}
		path := itemsPath + "/" + url.PathEscape(id) + "/play/master.m3u8?caps=" + allCaps
		m, err := s.get(ctx, path, "application/vnd.apple.mpegurl,*/*", true, 256<<10)
		switch {
		case err == nil && m.status == http.StatusNotFound:
			gone, last = gone+1, m
			continue
		case err != nil || m.status != http.StatusOK:
			return apiFailure(name, itemsPath+"/"+id+"/play/master.m3u8", m, err)
		}
		body := strings.TrimSpace(strings.TrimPrefix(string(m.body), "\ufeff"))
		if !strings.HasPrefix(body, "#EXTM3U") {
			return Result{Name: name, Status: Fail, Detail: "the master playlist of a packaged title is not a playlist: " + excerpt(m),
				Fix: "chino-stream serves the package's master.m3u8 — the package on disk is damaged, or the route reaches something else"}
		}
		answered := fmt.Sprintf("the master playlist of a packaged title answers, with %s",
			count(strings.Count(body, "#EXT-X-STREAM-INF"), "variant", "variants"))
		if gone > 0 {
			return Result{Name: name, Status: Warn,
				Detail: fmt.Sprintf("%s — and %s listed as packaged answered 404 first", answered, count(gone, "title", "titles")),
				Fix:    "chino-stream's list of packages lags behind the packages on disk; a run a minute later should not see it"}
		}
		return Result{Name: name, Status: OK, Detail: answered}
	}
	return Result{Name: name, Status: Fail,
		Detail: fmt.Sprintf("%s listed as packaged answered 404 for the master playlist: %s", count(gone, "title", "titles"), excerpt(last)),
		Fix:    "chino-stream lists packages it cannot serve — the packages on disk are gone or incomplete, or the packages volume is not mounted where it serves from"}
}

// maxGone is how many packaged titles a playback check tries before it
// concludes the packages themselves are missing.
const maxGone = 3

// catalogConsole asks the catalog console's API the one GraphQL question
// every server answers. An account may be refused — the console is for
// administrators — and a refusal that comes from the service itself proves it
// is up; a gateway's page, a 5xx or a hang does not.
func (s *session) catalogConsole(ctx context.Context) Result {
	const name = "katalog-manager: graphql"
	a, err := s.post(ctx, consoleAPI, `{"query":"{ __typename }"}`)
	switch {
	case err != nil:
		return Result{Name: name, Status: Fail, Detail: consoleAPI + ": no answer — " + err.Error(),
			Fix: "the catalog console cannot load anything: katalog-manager, or the route for /api/manage, is down"}
	case a.status == http.StatusUnauthorized || a.status == http.StatusForbidden:
		if !isHTML(a.ctype) && len(strings.TrimSpace(string(a.body))) > 0 {
			return Result{Name: name, Status: OK, Detail: fmt.Sprintf("reachable, and refused for this account (HTTP %d: %s)", a.status, excerpt(a))}
		}
		return Result{Name: name, Status: Warn, Detail: fmt.Sprintf("refused with HTTP %d by something that does not answer like the service: %s", a.status, excerpt(a)),
			Fix: "a gateway in front of /api/manage may be answering instead of katalog-manager"}
	case a.status != http.StatusOK:
		return apiFailure(name, consoleAPI, a, nil)
	}
	var gql struct {
		Data   map[string]any `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(a.body, &gql); err != nil || (gql.Data == nil && len(gql.Errors) == 0) {
		return Result{Name: name, Status: Fail, Detail: consoleAPI + " answered something that is not GraphQL (" + mediaType(a.ctype) + ")",
			Fix: "the route for /api/manage reaches something other than katalog-manager"}
	}
	if gql.Data == nil {
		return Result{Name: name, Status: Warn, Detail: "answered with GraphQL errors: " + clip(gql.Errors[0].Message, 120)}
	}
	return Result{Name: name, Status: OK, Detail: "answers GraphQL at " + consoleAPI}
}

// count is "1 credit" or "3 credits".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// bytesOf names a size for a person.
func bytesOf(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
