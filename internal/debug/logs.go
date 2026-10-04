package debug

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
	"github.com/zaentrum/zae/internal/redact"
)

// Pod is one row of GET /api/portal/debug/pods: a pod of the platform's
// namespace, its phase, the names of its containers — and what runs it.
type Pod struct {
	Pod        string   `json:"pod"`
	Phase      string   `json:"phase"`
	Containers []string `json:"containers"`
	// Workload is what runs the pod, read from its owner references: a
	// Deployment (behind its ReplicaSet), a StatefulSet, a DaemonSet, a Job —
	// WorkloadKind says which — and "" for a pod nothing owns. A pointer,
	// because a portal-api older than the field sends no key at all, and the
	// pod's name is then all zae has to go on.
	Workload     *string `json:"workload,omitempty"`
	WorkloadKind string  `json:"workloadKind,omitempty"`
}

// of reports whether the pod is one of the workload name's: the one the portal
// says runs it, or — from a portal-api that does not say — the one its name
// says, by the names Kubernetes gives a workload's pods.
func (p Pod) of(name string) bool {
	if p.Workload != nil {
		return *p.Workload == name
	}
	rest, ok := strings.CutPrefix(p.Pod, name+"-")
	return ok && generated(rest)
}

// workload is the name to read the pod's workload by: the one the portal
// names, the pod's own when nothing owns it, and from an older portal-api the
// one its name says.
func (p Pod) workload() string {
	switch {
	case p.Workload == nil:
		return workloadOf(p.Pod)
	case *p.Workload == "":
		return p.Pod
	}
	return *p.Workload
}

// source is one container whose log is read.
type source struct{ pod, container string }

func (s source) String() string { return s.pod + "/" + s.container }

// maxTail is the most lines the portal returns per container.
const maxTail = 5000

// followMargin widens every window a follow reads, so a line written while
// the previous read was on its way is read again rather than lost. The lines
// read twice are told apart by their timestamps.
const followMargin = 5 * time.Second

// logLimit bounds one container's answer; the portal caps it at 2 MiB.
const logLimit = 8 << 20

// nameRe is a Kubernetes object name, the shape the portal accepts for a pod
// or a container.
var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// hashChars is the alphabet Kubernetes writes the generated parts of a pod's
// name in — a ReplicaSet's template hash, a generated suffix: no vowels, no 0,
// 1 or 3, so that no generated part spells a word.
const hashChars = "bcdfghjklmnpqrstvwxz2456789"

// logs prints a workload's container logs, or follows them.
func logs(args []string) int {
	s := newSession()
	defer s.close()

	fs := flagSet("logs")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	container := fs.String("container", "", "only this container of each pod (default: every one)")
	tail := fs.Int("tail", 0, "lines per container, 1 to 5000 (default: the portal's, 500)")
	since := fs.Duration("since", 0, "only lines newer than this, e.g. 10m or 2h")
	follow := fs.Bool("follow", false, "keep reading new lines until Ctrl-C, through rollouts")
	asJSON := fs.Bool("json", false, "one JSON object per line: pod, container, time and line")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		return usageErr("zae debug logs <workload|pod> --url https://… takes one workload or pod name")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	name := pos[0]
	if len(name) > 253 || !nameRe.MatchString(name) {
		return usageErr("%q is not a workload or pod name (lowercase letters, digits, - and .)", name)
	}
	if *container != "" && (len(*container) > 63 || !nameRe.MatchString(*container)) {
		return usageErr("--container %q is not a container name", *container)
	}
	set := setFlags(fs)
	if set["tail"] && (*tail < 1 || *tail > maxTail) {
		return usageErr("--tail is 1 to %d lines per container — the most the portal returns", maxTail)
	}
	if set["since"] && *since <= 0 {
		return usageErr("--since is how far back to read, e.g. --since 10m")
	}

	c := newClient(base, 30*time.Second)
	pods, err := listPods(s.ctx, c)
	if err != nil {
		return failOr(s, err)
	}
	if len(pods) == 0 {
		// In a cluster the list always holds portal-api itself; empty is the
		// portal's answer outside one, where it has no pods to read.
		errf("not offered: %s lists no pods — its portal-api is not running in a cluster, so there are no container logs to read", base)
		return exitcode.NotOffered
	}
	srcs, exact := match(name, *container, pods)
	if len(srcs) == 0 {
		return noSuchSource(base, name, *container, pods)
	}
	w := &writer{json: *asJSON, prefix: len(srcs) > 1 || (*follow && !exact)}
	first := query{tail: *tail, since: seconds(*since)}
	if *follow {
		return followLogs(s, c, name, *container, srcs, first, w)
	}

	var all []entry
	failed := exitcode.OK
	read, gone := 0, 0
	for _, src := range srcs {
		es, err := readLog(s.ctx, c, src, first)
		if s.interrupted() {
			return s.exitCode()
		}
		switch {
		case err != nil && isGone(err) && exact:
			return gonePod(base, src, pods)
		case err != nil && isGone(err):
			// One pod of the workload went between the listing and the read —
			// a rollout replacing it, most likely. Its lines went with it; it
			// is said, and the rest are printed.
			gone++
			note("%s is gone — it stopped after zae listed the pods of %s", src, name)
			continue
		case err != nil:
			code := fail(err)
			if failed == exitcode.OK {
				failed = code
			}
			continue
		}
		read++
		all = append(all, es...)
	}
	w.print(merge(all))
	if gone > 0 && read == 0 && failed == exitcode.OK {
		// Nothing listed was there to read. That says nothing about whether
		// the workload runs — its pods are being replaced — so it is not "not
		// offered": zae could not find out, and asking again will.
		errf("undetermined: every pod of %s that zae listed on %s was gone by the time it was read — they are being replaced, most likely: run it again", name, base)
		return exitcode.Undetermined
	}
	return failed
}

// gonePod is exit 3 for a pod asked for by its own name that went after it
// was listed, naming the workload whose pods run in its place.
func gonePod(base string, src source, pods []Pod) int {
	msg := fmt.Sprintf("not offered: no pod %q runs on %s now — it was listed a moment ago, and is gone", src.pod, base)
	for _, p := range pods {
		if p.Pod == src.pod {
			if wl := p.workload(); wl != p.Pod {
				msg += fmt.Sprintf(" — zae debug logs %s --url %s reads the pods of %s", wl, base, wl)
			}
			break
		}
	}
	errf("%s", msg)
	return exitcode.NotOffered
}

// failOr is fail, unless a signal ended the session.
func failOr(s *session, err error) int {
	if s.interrupted() {
		return s.exitCode()
	}
	return fail(err)
}

// listPods reads the namespace's pods.
func listPods(ctx context.Context, c *client) ([]Pod, error) {
	raw, err := c.get(ctx, "list pods", podsPath, logLimit)
	if err != nil {
		return nil, err
	}
	var pods []Pod
	if err := json.Unmarshal(raw, &pods); err != nil {
		return nil, &apiError{code: exitcode.Undetermined,
			msg: fmt.Sprintf("undetermined: list pods: %s answered with JSON that is not a list of pods (%v) — does the address reach portal-api?", c.base, err)}
	}
	return pods, nil
}

// match picks the containers to read: the pod named name, else every pod of a
// workload named name — and of each, every container or the one asked for.
// exact is true when name is a pod's own.
//
// The portal names the workload that runs each pod, from its owner references,
// and that is what a workload's pods are found by. A portal-api older than
// that lists pods without their owners, and then they are found by the names
// Kubernetes gives them: <name>-<template hash>-<suffix> for a Deployment's,
// <name>-<suffix> for a Job's or a DaemonSet's, <name>-<ordinal> for a
// StatefulSet's. The generated parts are written in Kubernetes' own alphabet,
// which keeps chino from claiming chino-web's pods: "web" is not a template
// hash. What a name cannot say is whether a Job named api-bcdfg was made from
// api: by name its pods read as the Deployment api's — by owner they are not.
func match(name, container string, pods []Pod) (srcs []source, exact bool) {
	var chosen []Pod
	for _, p := range pods {
		if p.Pod == name {
			chosen, exact = []Pod{p}, true
			break
		}
	}
	if !exact {
		for _, p := range pods {
			if p.of(name) {
				chosen = append(chosen, p)
			}
		}
	}
	for _, p := range chosen {
		for _, c := range p.Containers {
			if container == "" || c == container {
				srcs = append(srcs, source{pod: p.Pod, container: c})
			}
		}
	}
	return srcs, exact
}

// generated reports whether rest is a suffix Kubernetes gives the pods of a
// workload: <hash>-<suffix>, <suffix>, or an ordinal.
func generated(rest string) bool {
	if ordinal(rest) || (len(rest) == 5 && hashLike(rest)) {
		return true
	}
	h, sfx, ok := strings.Cut(rest, "-")
	return ok && len(h) >= 1 && len(h) <= 10 && hashLike(h) && len(sfx) == 5 && hashLike(sfx)
}

func hashLike(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune(hashChars, r) {
			return false
		}
	}
	return s != ""
}

func ordinal(s string) bool {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// workloadOf is the workload a pod's name says it belongs to, or the pod's
// own name when it carries no generated part — the guess an older portal-api
// leaves zae with.
func workloadOf(pod string) string {
	for i := len(pod) - 1; i > 0; i-- {
		if pod[i] == '-' && generated(pod[i+1:]) {
			// The longest generated tail wins: <name>-<hash>-<suffix>, then
			// <name>-<suffix>.
			if j := strings.LastIndex(pod[:i], "-"); j > 0 && generated(pod[j+1:]) {
				return pod[:j]
			}
			return pod[:i]
		}
	}
	return pod
}

// noSuchSource is exit 3: the instance answered, and runs nothing to read by
// that name. It says what it does run.
func noSuchSource(base, name, container string, pods []Pod) int {
	if container != "" {
		if chosen, _ := match(name, "", pods); len(chosen) > 0 {
			var have []string
			for _, s := range chosen {
				if !contains(have, s.container) {
					have = append(have, s.container)
				}
			}
			they := "its containers are"
			if len(have) == 1 {
				they = "its container is"
			}
			errf("not offered: no container %q in the pods of %s on %s — %s %s", container, name, base, they, strings.Join(have, ", "))
			return exitcode.NotOffered
		}
	}
	var names []string
	for _, p := range pods {
		if n := p.workload(); !contains(names, n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	errf("not offered: %s runs no workload named %q, and no pod by that name — it runs %s (a pod's own name works too)",
		base, name, strings.Join(names, ", "))
	return exitcode.NotOffered
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// query is what one read asks for: lines per container, and how far back —
// a window of seconds before now, or the lines from a moment on. The portal
// takes one of since and sinceTime, never both.
type query struct {
	tail, since int
	sinceTime   time.Time
}

// seconds rounds a duration up to whole seconds, which is what the portal
// takes; zero stays zero.
func seconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

func logPath(src source, q query) string {
	v := url.Values{"pod": {src.pod}, "container": {src.container}}
	if q.tail > 0 {
		v.Set("tail", strconv.Itoa(q.tail))
	}
	switch {
	case !q.sinceTime.IsZero():
		v.Set("sinceTime", q.sinceTime.UTC().Format(time.RFC3339Nano))
	case q.since > 0:
		v.Set("since", strconv.Itoa(q.since))
	}
	return logsPath + "?" + v.Encode()
}

// readLog reads one container's log. Every container is named, so a pod with
// several never leaves the choice to the cluster.
func readLog(ctx context.Context, c *client, src source, q query) ([]entry, error) {
	raw, err := c.get(ctx, "logs of "+src.String(), logPath(src, q), logLimit)
	if err != nil {
		return nil, refusedRead(c.base, src, err)
	}
	return parseLog(src, raw), nil
}

// refusedRead words a log read the portal answered with its own 404 or 400,
// which each say something different about the one container asked for:
//
//	404  the namespace runs no pod by that name: it went after zae listed it
//	400  the apiserver refused the read as asked, in its words — a container
//	     the pod does not run (it was replaced under the same name), or one
//	     that is still waiting to start
//
// A portal-api older than the distinction answered 500 for both, which stays
// what it was: a read that failed.
func refusedRead(base string, src source, err error) error {
	ae, ok := asAPIError(err)
	if !ok {
		return err
	}
	switch {
	case ae.status == http.StatusNotFound && ae.said != routerNotFound:
		return &apiError{code: exitcode.NotOffered, status: ae.status, said: ae.said, gone: true,
			msg: fmt.Sprintf("not offered: no pod %q runs on %s now — it was listed a moment ago, and is gone", src.pod, base)}
	case ae.status == http.StatusBadRequest && strings.Contains(ae.said, "is not valid for pod"):
		return &apiError{code: exitcode.NotOffered, status: ae.status, said: ae.said, noContainer: true,
			msg: fmt.Sprintf("not offered: pod %s runs no container %q now: %s", src.pod, src.container, ae.said)}
	case ae.status == http.StatusBadRequest:
		return &apiError{code: exitcode.Failed, status: ae.status, said: ae.said,
			msg: fmt.Sprintf("failed: %s cannot be read now: %s", src, ae.said)}
	}
	return err
}

// isGone reports whether a read found its pod gone.
func isGone(err error) bool {
	ae, ok := asAPIError(err)
	return ok && ae.gone
}

// entry is one log line. The portal asks the cluster for timestamps, so every
// line starts with one; it is what a merge orders by and what a follow
// recognises a line it already printed by.
type entry struct {
	src  source
	at   time.Time
	line string // the line as the portal sent it, timestamp first
	text string // the line without its timestamp
}

// parseLog splits a log into lines. A line whose timestamp cannot be read
// takes the one before it, so it stays where it was in a merge.
func parseLog(src source, raw []byte) []entry {
	text := strings.TrimRight(string(raw), "\n")
	if text == "" {
		return nil
	}
	var out []entry
	var last time.Time
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		e := entry{src: src, at: last, line: line, text: line}
		if ts, rest, ok := strings.Cut(line, " "); ok {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				e.at, e.text, last = t, rest, t
			}
		}
		out = append(out, e)
	}
	return out
}

// merge orders the lines of several containers by time. Each container's own
// lines are in order already, and a stable sort keeps them so.
func merge(es []entry) []entry {
	sort.SliceStable(es, func(i, j int) bool { return es[i].at.Before(es[j].at) })
	return es
}

// writer prints lines: as the portal sent them, with [pod/container] in front
// when more than one container is read, or as one JSON object each.
type writer struct {
	json   bool
	prefix bool
}

// logLine is one line as --json prints it.
type logLine struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Time      string `json:"time,omitempty"`
	Line      string `json:"line"`
}

func (w *writer) print(es []entry) {
	for _, e := range es {
		if w.json {
			l := logLine{Pod: e.src.pod, Container: e.src.container, Line: redact.Secrets(e.text)}
			if !e.at.IsZero() {
				l.Time = e.at.UTC().Format(time.RFC3339Nano)
			}
			b, _ := redact.Encode(l, "")
			fmt.Fprintln(stdout, string(b))
			continue
		}
		line := redact.Secrets(e.line)
		if w.prefix {
			line = "[" + e.src.String() + "] " + line
		}
		fmt.Fprintln(stdout, line)
	}
}
