package addon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
)

// maxValuesBytes bounds --values: an install's inputs, not a data file.
const maxValuesBytes = 1 << 20

// maxNameLen: the addon name is the Helm release name and prefixes the
// objects the platform creates for it (zaentrum-addon-<name>-generated), so
// it stays well inside a DNS label.
const maxNameLen = 40

// reservedKey is the top-level values key the operator sets with platform
// facts. Anything given under it would be overridden, so zae refuses it.
const reservedKey = "zaentrum"

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// A dotted values path: the same spelling the portal uses for secret keys,
	// which become Secret keys and so allow nothing else. Keys outside it go
	// through --values.
	pathRe   = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)
	digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// archiveVersion is the "-1.2.0" (or "-v0.3.1-rc.1") a packaged chart's
	// file name ends with — the same pattern portal-api strips.
	archiveVersion = regexp.MustCompile(`-v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?$`)
)

func validName(s string) bool { return len(s) <= maxNameLen && dnsLabel.MatchString(s) }

// chartRef is a chart reference in the shape the resource takes: an oci://
// repository and its tag apart, or one https link to an archive.
type chartRef struct {
	ref     string
	version string // the tag, for oci://; empty for an https archive
	oci     bool
}

func (c chartRef) String() string {
	if c.oci && c.version != "" {
		return c.ref + " " + c.version
	}
	return c.ref
}

// parseChart reads a chart reference. An oci:// reference needs a version —
// from version, or as its own :tag, which is split off because the resource
// keeps the tag apart. An https link is one archive, so version does not
// apply to it.
func parseChart(raw, version string) (chartRef, error) {
	switch {
	case strings.HasPrefix(raw, "oci://"):
		rest := strings.TrimPrefix(raw, "oci://")
		slash := strings.LastIndex(rest, "/")
		if slash <= 0 || slash == len(rest)-1 {
			return chartRef{}, fmt.Errorf("%q is not an OCI chart reference like oci://registry.example.org/charts/example", raw)
		}
		c := chartRef{ref: raw, version: version, oci: true}
		last := rest[slash+1:]
		if strings.Contains(last, "@") {
			return chartRef{}, fmt.Errorf("%q pins a digest in the reference — give the version as the tag, and pin the chart archive with --digest sha256:…", raw)
		}
		if i := strings.LastIndex(last, ":"); i >= 0 {
			tag := last[i+1:]
			if version != "" && version != tag {
				return chartRef{}, fmt.Errorf("%q names tag %s but --version says %s", raw, tag, version)
			}
			c.ref = "oci://" + rest[:slash+1] + last[:i]
			c.version = tag
		}
		if c.version == "" {
			return chartRef{}, fmt.Errorf("an oci:// chart needs its version: --version 1.2.0, or %s:1.2.0", raw)
		}
		return c, nil
	case strings.HasPrefix(raw, "https://"):
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || strings.Trim(u.Path, "/") == "" {
			return chartRef{}, fmt.Errorf("%q is not a link to a chart archive like https://example.org/charts/example-1.2.0.tgz", raw)
		}
		return chartRef{ref: raw}, nil
	case strings.HasPrefix(raw, "http://"):
		return chartRef{}, fmt.Errorf("%q: chart archives are fetched over https only", raw)
	default:
		return chartRef{}, fmt.Errorf("%q is not a chart reference: use oci://registry/path/chart or an https:// link to a chart archive", raw)
	}
}

// defaultName derives the addon name from a chart reference exactly as
// portal-api does, so settings and zae name the same chart the same way: the
// last path segment, without a tag or digest, without a .tar.gz or .tgz
// extension, without a trailing -<version> (pre-release and build included),
// lower-cased. oci://ghcr.io/example/charts/example:1.2.0 and
// https://example.org/charts/Example-1.2.0.tgz are both "example". It returns
// "" when the reference has no path; the caller then asks for --name. zae
// always sends the name it derived, so the name it checks is the name used.
func defaultName(ref string) string {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ""
	}
	last := path.Base(strings.TrimRight(u.Path, "/"))
	if last == "." || last == "/" {
		return ""
	}
	if i := strings.IndexAny(last, ":@"); i >= 0 {
		last = last[:i]
	}
	for _, ext := range []string{".tar.gz", ".tgz"} {
		if strings.HasSuffix(strings.ToLower(last), ext) {
			last = last[:len(last)-len(ext)]
			break
		}
	}
	return strings.ToLower(archiveVersion.ReplaceAllString(last, ""))
}

// readValues loads --values: one JSON object, from a file or from stdin ("-").
func readValues(src string) (map[string]any, error) {
	var r io.Reader
	name := src
	if src == "-" {
		r, name = stdin, "stdin"
	} else {
		f, err := os.Open(src)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxValuesBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", name, err)
	}
	if len(b) > maxValuesBytes {
		return nil, fmt.Errorf("%s is larger than 1 MiB — values are an install's inputs", name)
	}
	return parseValues(b, name)
}

// parseValues accepts exactly one JSON object. YAML is refused rather than
// guessed at: zae has no YAML parser, and a hand-rolled one would disagree
// with Helm's about what "yes", "on" or 0755 mean — the same file would then
// install differently through zae than through helm. JSON is also valid YAML,
// so a JSON values file works everywhere a chart's values do.
func parseValues(b []byte, name string) (map[string]any, error) {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%s is empty — values are one JSON object", name)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		if trimmed[0] != '{' && trimmed[0] != '[' {
			return nil, fmt.Errorf("%s is not JSON — zae reads values as JSON only; convert YAML first, e.g. yq -o=json values.yaml", name)
		}
		return nil, fmt.Errorf("%s is not valid JSON: %v", name, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s holds more than one JSON value — values are one JSON object", name)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(`%s is JSON but not an object — values look like {"worker":{"replicas":2}}`, name)
	}
	if _, ok := m[reservedKey]; ok {
		return nil, fmt.Errorf("%s sets %q — values under it are set by the platform; remove the key", name, reservedKey)
	}
	return m, nil
}

// assignment splits a --set or --set-secret argument into path and value.
// Messages name the path only: the value may be a secret.
func assignment(flagName, arg string) (path, value string, err error) {
	path, value, ok := strings.Cut(arg, "=")
	if !ok {
		return "", "", fmt.Errorf("--%s wants path=value", flagName)
	}
	if !pathRe.MatchString(path) {
		return "", "", fmt.Errorf("--%s: %q is not a dotted path of letters, digits, _ and - (other keys go in --values)", flagName, path)
	}
	if path == reservedKey || strings.HasPrefix(path, reservedKey+".") {
		return "", "", fmt.Errorf("--%s %s: values under %q are set by the platform", flagName, path, reservedKey)
	}
	return path, value, nil
}

// setValue types a --set value: JSON when the whole value parses as JSON —
// numbers, true, false, null, "quoted strings", arrays, objects — and the text
// itself as a string otherwise. Quoting forces a string: --set 'tag="1.10"'.
func setValue(s string) any {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return s
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return s
	}
	return v
}

// setPath sets a dotted path in values, creating objects on the way.
func setPath(values map[string]any, path string, v any) error {
	parts := strings.Split(path, ".")
	m := values
	for i, p := range parts[:len(parts)-1] {
		next, ok := m[p]
		if !ok || next == nil {
			child := map[string]any{}
			m[p] = child
			m = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("cannot set %s: %s is already a value, not an object", path, strings.Join(parts[:i+1], "."))
		}
		m = child
	}
	m[parts[len(parts)-1]] = v
	return nil
}

// lookup reads a dotted path from values.
func lookup(values map[string]any, path string) (any, bool) {
	var cur any = values
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}
