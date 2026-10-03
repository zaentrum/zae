package doctor

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

// page is what the doctor reads out of an identity provider's HTML: its
// forms, the message it shows when something went wrong, its title, and
// every address it links or posts to.
//
// It reads pages by what they DO, never by how they look. The bundled realm
// has a login theme of its own, another realm has another, and the next
// Keycloak release changes the stock one again; what stays is that the login
// form posts a username and a password to …/login-actions/authenticate, that a
// pending required action is reached under …/login-actions/required-action
// with the action's name as `execution`, and that a page says what went wrong
// in an alert. So a form is found by its action, a field by its type and
// name, and a required action by the address it posts to.
//
// The standard library has no HTML parser, and zae takes no dependency for
// one: a page an identity provider renders is a handful of tags, and a
// scanner that reads start tags and their attributes is enough to find them.
type page struct {
	title   string
	forms   []form
	message string
	// links is every URL the page posts or points to, as written.
	links []string
}

type form struct {
	id, action, method string
	inputs             []input
}

type input struct {
	name, typ, value string
	checked          bool
}

// loginActions is the path every Keycloak login step posts to; what follows
// it says which step it is.
const (
	authenticateAction   = "/login-actions/authenticate"
	requiredActionAction = "/login-actions/required-action"
)

// passwordField is the form's password input, nil when it has none.
func (f *form) passwordField() *input {
	for i := range f.inputs {
		if f.inputs[i].typ == "password" {
			return &f.inputs[i]
		}
	}
	return nil
}

// usernameField is the input a username goes in: the one named username, as
// Keycloak names it, else the first text or e-mail input.
func (f *form) usernameField() *input {
	for i := range f.inputs {
		if f.inputs[i].name == "username" {
			return &f.inputs[i]
		}
	}
	for i := range f.inputs {
		if t := f.inputs[i].typ; t == "text" || t == "email" || t == "" {
			return &f.inputs[i]
		}
	}
	return nil
}

// values is what a browser would send for the form as it stands: every named
// input except buttons and files, and checkboxes and radio buttons only when
// checked. The caller fills in the username and the password.
func (f *form) values() url.Values {
	v := url.Values{}
	for _, in := range f.inputs {
		if in.name == "" {
			continue
		}
		switch in.typ {
		case "submit", "button", "image", "reset", "file":
			continue
		case "checkbox", "radio":
			if !in.checked {
				continue
			}
			if in.value == "" {
				in.value = "on"
			}
		}
		v.Add(in.name, in.value)
	}
	return v
}

// postsTo reports whether the form's action path contains the given step.
func (f *form) postsTo(step string) bool { return strings.Contains(actionPath(f.action), step) }

func actionPath(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Path
	}
	return raw
}

// loginForm is the username-and-password form: it posts to the authenticate
// step and has a password field.
func (p *page) loginForm() *form {
	for i := range p.forms {
		if p.forms[i].postsTo(authenticateAction) && p.forms[i].passwordField() != nil {
			return &p.forms[i]
		}
	}
	return nil
}

// otherStep is a form for a further authentication step — one that posts to
// the authenticate step without asking for a password: a one-time code, a
// security key, a choice of credential.
func (p *page) otherStep() *form {
	for i := range p.forms {
		if p.forms[i].postsTo(authenticateAction) && p.forms[i].passwordField() == nil {
			return &p.forms[i]
		}
	}
	return nil
}

// requiredAction is the required action the page asks the account to
// complete — UPDATE_PASSWORD, VERIFY_PROFILE, … — or "" when it asks none.
func (p *page) requiredAction() string {
	for _, raw := range p.links {
		u, err := url.Parse(raw)
		if err != nil || !strings.Contains(u.Path, requiredActionAction) {
			continue
		}
		if a := u.Query().Get("execution"); a != "" {
			return a
		}
		return "a required action"
	}
	return ""
}

// parsePage reads the parts of a page the doctor acts on.
func parsePage(body []byte) *page {
	s := string(body)
	p := &page{}
	tags := scanTags(s)
	for i, t := range tags {
		switch t.name {
		case "title":
			if p.title == "" {
				p.title = innerText(s, tags, i)
			}
		case "form":
			f := form{id: t.attrs["id"], action: t.attrs["action"], method: strings.ToLower(t.attrs["method"])}
			if f.method == "" {
				f.method = "get"
			}
			for _, in := range tags[i+1:] {
				if in.name == "/form" {
					break
				}
				if in.name == "input" {
					_, checked := in.attrs["checked"]
					f.inputs = append(f.inputs, input{name: in.attrs["name"], typ: strings.ToLower(in.attrs["type"]),
						value: in.attrs["value"], checked: checked})
				}
			}
			p.forms = append(p.forms, f)
			p.links = append(p.links, f.action)
		case "a", "link":
			if h := t.attrs["href"]; h != "" {
				p.links = append(p.links, h)
			}
		}
	}
	p.message = pageMessage(s, tags)
	return p
}

// pageMessage is what the page says went wrong. Keycloak's themes put it in
// one of a few places, and a theme of its own keeps at least one of them; the
// most specific wins.
func pageMessage(s string, tags []tag) string {
	match := []func(t tag) bool{
		func(t tag) bool { return hasClass(t, "kc-feedback-text") },
		func(t tag) bool { return strings.HasPrefix(t.attrs["id"], "input-error") },
		func(t tag) bool { return t.attrs["role"] == "alert" },
		func(t tag) bool { return t.attrs["id"] == "kc-error-message" },
		func(t tag) bool { return hasClass(t, "instruction") || hasClassPrefix(t, "alert") },
	}
	for _, m := range match {
		for i, t := range tags {
			if strings.HasPrefix(t.name, "/") || !m(t) {
				continue
			}
			if text := innerText(s, tags, i); text != "" {
				return text
			}
		}
	}
	return ""
}

func hasClass(t tag, c string) bool {
	for _, x := range strings.Fields(t.attrs["class"]) {
		if x == c {
			return true
		}
	}
	return false
}

func hasClassPrefix(t tag, prefix string) bool {
	for _, x := range strings.Fields(t.attrs["class"]) {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}
	return false
}

// tag is one start or end tag: its lower-case name ("/div" for an end tag),
// its attributes with their values unescaped, and where it ends in the page.
type tag struct {
	name  string
	attrs map[string]string
	start int // offset of '<'
	end   int // offset just past '>'
}

// scanTags reads every tag of the page in order. Comments are skipped, and so
// is what a script or a style element contains, so a "<form" in a script is
// not taken for a form.
func scanTags(s string) []tag {
	var out []tag
	for i := 0; i < len(s); {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		if strings.HasPrefix(s[i:], "<!--") {
			end := strings.Index(s[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		t, ok := readTag(s, i)
		if !ok {
			i++
			continue
		}
		out = append(out, t)
		i = t.end
		if t.name == "script" || t.name == "style" {
			close := strings.Index(strings.ToLower(s[i:]), "</"+t.name)
			if close < 0 {
				break
			}
			i += close
		}
	}
	return out
}

// readTag reads the tag that starts at s[i] == '<'.
func readTag(s string, i int) (tag, bool) {
	j := i + 1
	closing := j < len(s) && s[j] == '/'
	if closing {
		j++
	}
	n := j
	for n < len(s) && isNameByte(s[n]) {
		n++
	}
	if n == j {
		return tag{}, false
	}
	t := tag{name: strings.ToLower(s[j:n]), attrs: map[string]string{}, start: i}
	if closing {
		t.name = "/" + t.name
	}
	k := n
	for k < len(s) {
		for k < len(s) && isSpace(s[k]) {
			k++
		}
		if k >= len(s) {
			return tag{}, false
		}
		if s[k] == '>' {
			t.end = k + 1
			return t, true
		}
		if s[k] == '/' {
			k++
			continue
		}
		a := k
		for k < len(s) && !isSpace(s[k]) && s[k] != '=' && s[k] != '>' && s[k] != '/' {
			k++
		}
		name := strings.ToLower(s[a:k])
		for k < len(s) && isSpace(s[k]) {
			k++
		}
		val := ""
		if k < len(s) && s[k] == '=' {
			k++
			for k < len(s) && isSpace(s[k]) {
				k++
			}
			if k < len(s) && (s[k] == '"' || s[k] == '\'') {
				q := s[k]
				end := strings.IndexByte(s[k+1:], q)
				if end < 0 {
					return tag{}, false
				}
				val = s[k+1 : k+1+end]
				k += end + 2
			} else {
				v := k
				for k < len(s) && !isSpace(s[k]) && s[k] != '>' {
					k++
				}
				val = s[v:k]
			}
		}
		if name != "" {
			if _, seen := t.attrs[name]; !seen {
				t.attrs[name] = html.UnescapeString(val)
			}
		}
	}
	return tag{}, false
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

var anyTag = regexp.MustCompile(`<[^>]*>`)

// innerText is the text inside the element tags[i] opens, as a reader sees
// it: tags dropped, entities read, whitespace collapsed, at most 200
// characters. Elements of the same name nested inside it are counted, so a
// div in a div ends at the right place.
func innerText(s string, tags []tag, i int) string {
	open := tags[i]
	depth, end := 1, len(s)
	for _, t := range tags[i+1:] {
		switch t.name {
		case open.name:
			depth++
		case "/" + open.name:
			depth--
		}
		if depth == 0 {
			end = t.start
			break
		}
	}
	text := html.UnescapeString(anyTag.ReplaceAllString(s[open.end:end], " "))
	return clip(strings.Join(strings.Fields(text), " "), 200)
}

// clip shortens s to at most n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
