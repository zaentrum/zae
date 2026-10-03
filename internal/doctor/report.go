package doctor

import (
	"bytes"
	"encoding/json"
	"os"
)

// The report is a run's outcome as data, for whatever runs the doctor on the
// platform's behalf. The operator's verification Job passes
// --report /dev/termination-log, and the kubelet keeps at most 4096 bytes of
// that file — the rest is cut, wherever it falls, which would leave JSON that
// does not parse. So the report is built to fit, giving up the least useful
// text first:
//
//  1. the detail of every check that passed;
//  2. then less and less of the details of skips and warnings, and of
//     failures last and least;
//  3. then whole lines: the ones that passed, then the skips, then the
//     warnings.
//
// A failure is never dropped while anything else is left, and the counts are
// always the run's own, so a reader can tell how much was left out.
const reportLimit = 4096

// reportVersion is the shape's version, "v" in the document.
const reportVersion = 1

type reportDoc struct {
	V       int           `json:"v"`
	Zae     string        `json:"zae"`
	URL     string        `json:"url"`
	Passed  int           `json:"passed"`
	Failed  int           `json:"failed"`
	Warned  int           `json:"warned"`
	Skipped int           `json:"skipped"`
	Checks  []reportCheck `json:"checks"`
}

// reportCheck is one check: its name, ok|warn|fail|skip, and what it found —
// omitted when nothing is left of it.
type reportCheck struct {
	N string `json:"n"`
	S string `json:"s"`
	D string `json:"d,omitempty"`
}

// word is the status as the report spells it.
func (s Status) word() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return "skip"
	}
}

// reportDetail is what a line says: its detail, and for a line that did not
// pass, the fix — the part a reader of a failure needs most.
func reportDetail(r Result) string {
	if r.Fix != "" && r.Status != OK {
		if r.Detail == "" {
			return "fix: " + r.Fix
		}
		return r.Detail + " · fix: " + r.Fix
	}
	return r.Detail
}

// budget is one round of giving things up: the most a detail may keep, per
// status (0 drops it), and which statuses lose their lines altogether.
type budget struct {
	ok, skip, warn, fail       int
	dropOK, dropSkip, dropWarn bool
}

var budgets = []budget{
	{ok: 300, skip: 300, warn: 300, fail: 500},
	{ok: 0, skip: 300, warn: 300, fail: 500},
	{ok: 0, skip: 120, warn: 200, fail: 400},
	{ok: 0, skip: 60, warn: 120, fail: 300},
	{ok: 0, skip: 0, warn: 60, fail: 200},
	{ok: 0, skip: 0, warn: 60, fail: 200, dropOK: true},
	{ok: 0, skip: 0, warn: 40, fail: 120, dropOK: true, dropSkip: true},
	{ok: 0, skip: 0, warn: 0, fail: 80, dropOK: true, dropSkip: true},
	{ok: 0, skip: 0, warn: 0, fail: 60, dropOK: true, dropSkip: true, dropWarn: true},
	{ok: 0, skip: 0, warn: 0, fail: 30, dropOK: true, dropSkip: true, dropWarn: true},
	{ok: 0, skip: 0, warn: 0, fail: 0, dropOK: true, dropSkip: true, dropWarn: true},
}

// buildReport is the report for a run, at most reportLimit bytes with its
// trailing newline.
func buildReport(version, base string, results []Result) []byte {
	doc := reportDoc{V: reportVersion, Zae: version, URL: base, Checks: []reportCheck{}}
	for _, r := range results {
		switch r.Status {
		case OK:
			doc.Passed++
		case Warn:
			doc.Warned++
		case Fail:
			doc.Failed++
		default:
			doc.Skipped++
		}
	}
	var out []byte
	for _, b := range budgets {
		doc.Checks = doc.Checks[:0]
		for _, r := range results {
			keep, limit := true, 0
			switch r.Status {
			case OK:
				keep, limit = !b.dropOK, b.ok
			case Warn:
				keep, limit = !b.dropWarn, b.warn
			case Fail:
				limit = b.fail
			default:
				keep, limit = !b.dropSkip, b.skip
			}
			if !keep {
				continue
			}
			d := ""
			if limit > 0 {
				d = clip(reportDetail(r), limit)
			}
			doc.Checks = append(doc.Checks, reportCheck{N: r.Name, S: r.Status.word(), D: d})
		}
		if out = encode(doc); len(out) <= reportLimit {
			return out
		}
	}
	// More failures than fit even by name: keep as many as do. The counts
	// still say how many there were. (Doctor runs some thirty checks; this is
	// the guard, not the plan.)
	for len(doc.Checks) > 0 {
		doc.Checks = doc.Checks[:len(doc.Checks)-1]
		if out = encode(doc); len(out) <= reportLimit {
			return out
		}
	}
	return out
}

// encode is the document on one line, without HTML escaping: "<" spelled as
// < costs six bytes of a budget that has none to spare.
func encode(doc reportDoc) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(doc)
	return buf.Bytes()
}

// writeReport writes the report to path. It truncates what is there: a
// termination message is one document, not a log.
func writeReport(path, version, base string, results []Result) error {
	return os.WriteFile(path, buildReport(version, base, results), 0o644)
}
