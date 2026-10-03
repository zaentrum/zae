package debug

import (
	"context"
	"encoding/json"
	"fmt"
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
// namespace, its phase, and the names of its containers.
type Pod struct {
	Pod        string   `json:"pod"`
	Phase      string   `json:"phase"`
	Containers []string `json:"containers"`
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
	for _, src := range srcs {
		es, err := readLog(s.ctx, c, src, first)
		if s.interrupted() {
			return s.exitCode()
		}
		if err != nil {
			code := fail(err)
			if failed == exitcode.OK {
				failed = code
			}
			continue
		}
		all = append(all, es...)
	}
	w.print(merge(all))
	return failed
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
// The portal lists pods without their owners, so a workload's pods are found
// by the names Kubernetes gives them: <name>-<template hash>-<suffix> for a
// Deployment's, <name>-<suffix> for a Job's or a DaemonSet's, <name>-<ordinal>
// for a StatefulSet's. The generated parts are written in Kubernetes' own
// alphabet, which keeps chino from claiming chino-web's pods: "web" is not a
// template hash.
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
			if rest, ok := strings.CutPrefix(p.Pod, name+"-"); ok && generated(rest) {
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
// own name when it carries no generated part.
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
		if n := workloadOf(p.Pod); !contains(names, n) {
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

// query is what one read asks for: lines per container, and how far back.
type query struct{ tail, since int }

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
	if q.since > 0 {
		v.Set("since", strconv.Itoa(q.since))
	}
	return logsPath + "?" + v.Encode()
}

// readLog reads one container's log. Every container is named, so a pod with
// several never leaves the choice to the cluster.
func readLog(ctx context.Context, c *client, src source, q query) ([]entry, error) {
	raw, err := c.get(ctx, "logs of "+src.String(), logPath(src, q), logLimit)
	if err != nil {
		return nil, err
	}
	return parseLog(src, raw), nil
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
