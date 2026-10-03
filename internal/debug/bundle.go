package debug

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
	"github.com/zaentrum/zae/internal/redact"
)

// bundleSections are the sections the portal assembles, each on unless asked
// off, in the order the console offers them; client is zae's own.
var bundleSections = []string{"logs", "instances", "kafka", "registry", "config", "client"}

// bundleKind is what the portal's support bundle says it is.
const bundleKind = "zaentrum-support-bundle"

// bundleLimit bounds the bundle: the portal caps its logs at 24 MiB, and the
// rest is small beside them.
const bundleLimit = 64 << 20

// bundleTimeout: the portal gives itself a minute to read every container's
// log; the request waits a little longer than that.
const bundleTimeout = 2 * time.Minute

// bundle writes the portal's support bundle to a file, or to stdout.
func bundle(args []string, version string) int {
	s := newSession()
	defer s.close()

	fset := flagSet("bundle")
	rawURL := fset.String("url", "", "public URL of the instance (required)")
	out := fset.String("o", "", "the file to write — never one that exists — or - for stdout (required)")
	var without listFlag
	fset.Var(&without, "without", "sections to leave out: "+strings.Join(bundleSections, ", ")+" (repeatable, or a comma list)")
	pos, code, ok := parse(fset, args)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return usageErr("zae debug bundle -o FILE --url https://… takes no arguments")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	if *out == "" {
		return usageErr("zae debug bundle needs -o FILE, or -o - for stdout")
	}
	off := map[string]bool{}
	for _, w := range without {
		if !contains(bundleSections, w) {
			return usageErr("--without %q: the sections are %s", w, strings.Join(bundleSections, ", "))
		}
		off[w] = true
	}
	if len(off) == len(bundleSections) {
		return usageErr("--without leaves nothing to bundle")
	}
	// A bundle holds every container's recent log: it is not written over a
	// file somebody may still need, and never at a wider mode than the
	// credentials file's. Checked first, so that a usage error costs no
	// minute of assembling.
	if *out != "-" {
		if _, err := os.Lstat(*out); err == nil {
			return usageErr("%s exists — zae writes a bundle to a new file only; name another one", *out)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return usageErr("-o %s: %v", *out, err)
		}
		if fi, err := os.Stat(filepath.Dir(*out)); err != nil || !fi.IsDir() {
			return usageErr("-o %s: %s is not a directory to write it in", *out, filepath.Dir(*out))
		}
	}

	q := url.Values{}
	for _, sec := range bundleSections {
		if sec == "client" {
			continue // zae's own, never the portal's to assemble
		}
		q.Set(sec, onOff(!off[sec]))
	}
	c := newClient(base, bundleTimeout)
	fmt.Fprintf(stderr, "assembling the support bundle on %s — the portal reads every container's recent log, which can take a minute …\n", base)
	raw, err := c.get(s.ctx, "the support bundle", bundlePath+"?"+q.Encode(), bundleLimit)
	if err != nil {
		return failOr(s, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil || doc["kind"] != bundleKind {
		errf("undetermined: the support bundle: %s answered with something that is not a support bundle — does the address reach portal-api?", base)
		return exitcode.Undetermined
	}
	sections, _ := doc["sections"].(map[string]any)
	if sections == nil {
		sections = map[string]any{}
		doc["sections"] = sections
	}
	// Redacted again on this side, value by value, before the client section
	// is added: what zae says about itself holds nothing to redact.
	redact.Value(doc)
	if !off["client"] {
		sections["client"] = map[string]any{
			"kind": "zae", "version": version, "os": runtime.GOOS, "arch": runtime.GOARCH,
			"collectedAt": now().UTC().Format(time.RFC3339),
		}
	}
	body, err := redact.Encode(doc, "  ")
	if err != nil {
		errf("failed: the support bundle could not be encoded again: %v", err)
		return exitcode.Failed
	}
	body = append(body, '\n')

	if *out == "-" {
		if _, err := stdout.Write(body); err != nil {
			errf("failed: writing the bundle to stdout: %v", err)
			return exitcode.Failed
		}
	} else if err := writeNew(*out, body); err != nil {
		errf("failed: %v", err)
		return exitcode.Failed
	}
	where := *out
	if where == "-" {
		where = "stdout"
	}
	fmt.Fprintf(stderr, "wrote %s (%s): %s\n", where, size(len(body)), describeSections(sections))
	return exitcode.OK
}

func onOff(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// writeNew writes body to a file that does not exist yet, readable by its
// owner only. O_EXCL makes "never over a file that exists" hold even when one
// appears while the portal assembles.
func writeNew(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("cannot create %s: %w", path, err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	return nil
}

// describeSections names what the bundle holds, and how many containers'
// logs — the part a reader of a bug report goes to first.
func describeSections(sections map[string]any) string {
	order := map[string]int{"config": 0, "registry": 1, "kafka": 2, "operator": 3, "instances": 4, "pods": 5, "logs": 6, "client": 7}
	names := make([]string, 0, len(sections))
	for k := range sections {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		oi, iok := order[names[i]]
		oj, jok := order[names[j]]
		switch {
		case iok && jok:
			return oi < oj
		case iok != jok:
			return iok
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, k := range names {
		if k != "logs" {
			parts = append(parts, k)
			continue
		}
		logs, _ := sections[k].(map[string]any)
		n := len(logs)
		s := "logs of " + count(n, "container", "containers")
		if _, cut := logs["_note"]; cut {
			s = "logs of " + count(n-1, "container", "containers") + " (cut short: the portal's size cap was reached)"
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "no sections — the portal had nothing to put in them"
	}
	return strings.Join(parts, ", ")
}

// size names a byte count for a person.
func size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
