package debug

import (
	"net/http"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
)

// followed is one container a follow reads: the newest line it has printed,
// and when it last read it.
//
// The portal has no stream to subscribe to — the console's "live" button
// reads the log again every few seconds, and so does this. Each read asks for
// the lines from the newest one printed on (sinceTime, that line's own stamp),
// and prints only the lines newer than it: the cluster stamps every line, to
// the nanosecond, so a line read twice is recognised by its stamp and not by
// guessing at overlaps. And lines ARE read twice: the cluster reads sinceTime
// to the second, so the whole second of the newest line comes back — on the
// demo, a sinceTime in the middle of a second answered all seventeen lines of
// it, eight of them from before the time asked for.
//
// A container with no line printed yet is read by window instead — the
// seconds since the last read, widened by followMargin, measured on this
// machine's clock — since a time from this clock means nothing to the
// cluster's. So is every container against a portal-api that ignores
// sinceTime or refuses it: the window is how a follow read before there was
// sinceTime, and it still works there.
type followed struct {
	src source
	// last is the newest timestamp printed; atLast the lines printed at
	// exactly that time, since a second read of the window returns them again.
	last   time.Time
	atLast map[string]bool
	// read is true once a read has succeeded; readAt is when that read began,
	// by this machine's clock — the start of the next window.
	read   bool
	readAt time.Time
	// trouble is the last failure said about this container, so a failure that
	// repeats every round is said once.
	trouble string
	// gone: the portal answered that the pod is gone. It is said once, and the
	// listing that drops the pod does not say it again; it is read on all the
	// same while it is listed, since a StatefulSet's pod comes back under its
	// own name.
	gone bool
}

// fresh keeps the lines not printed yet.
func (f *followed) fresh(es []entry) []entry {
	var out []entry
	for _, e := range es {
		switch {
		case e.at.After(f.last):
		case !e.at.IsZero() && e.at.Equal(f.last) && !f.atLast[e.line]:
		default:
			continue
		}
		out = append(out, e)
	}
	return out
}

// remember records what was printed.
func (f *followed) remember(es []entry) {
	for _, e := range es {
		if e.at.After(f.last) {
			f.last, f.atLast = e.at, map[string]bool{}
		}
		if e.at.Equal(f.last) {
			f.atLast[e.line] = true
		}
	}
}

// note prints a follow's remark about what it is reading, on stderr: the
// lines on stdout stay the log.
func note(format string, a ...any) { errf("note: "+format, a...) }

// ignoredSinceTime reports whether a read answered lines from before the
// second it asked for: the cluster reads sinceTime to the second, so the lines
// of that second come back — and nothing older does, unless the portal did not
// read the parameter at all.
func ignoredSinceTime(es []entry, since time.Time) bool {
	floor := since.Truncate(time.Second)
	for _, e := range es {
		if !e.at.IsZero() && e.at.Before(floor) {
			return true
		}
	}
	return false
}

// refusesSinceTime reports whether a read was refused for its sinceTime.
func refusesSinceTime(err error) bool {
	ae, ok := asAPIError(err)
	return ok && ae.status == http.StatusBadRequest && strings.Contains(ae.said, "sinceTime")
}

// stopsAFollow reports whether a failure ends a follow: a refused bearer, or
// an instance with nothing to read, does not get better by asking again. A pod
// that went, or a container a pod no longer runs, is a rollout under way —
// what the follow is there to take up.
func stopsAFollow(err error) bool {
	ae, ok := asAPIError(err)
	if !ok || ae.gone || ae.noContainer {
		return false
	}
	return ae.code == exitcode.Forbidden || ae.code == exitcode.NotOffered || ae.code == exitcode.Usage
}

// followLogs prints the first read the flags ask for, then reads every
// pollInterval until Ctrl-C. Each round lists the pods again: a pod a rollout
// retired is said to be gone, and the pods that replace it are read from
// their first line on.
func followLogs(s *session, c *client, name, container string, srcs []source, first query, w *writer) int {
	start := now()
	tracked := map[source]*followed{}
	var order []source
	track := func(src source) {
		tracked[src] = &followed{src: src, atLast: map[string]bool{}}
		order = append(order, src)
	}
	for _, src := range srcs {
		track(src)
	}
	// bySinceTime: this portal-api reads the lines from a moment on. Assumed
	// until a read shows otherwise — one that ignores the parameter answers
	// lines older than it, one that refuses it says so — and then every read
	// is by window, as it was before there was sinceTime.
	bySinceTime := true
	byWindow := func(why string) {
		if bySinceTime {
			bySinceTime = false
			note("%s %s — reading the window since each read instead", c.base, why)
		}
	}

	// read reads one container and returns the lines it has not printed, or
	// a failure that ends the follow.
	read := func(f *followed, q query, at time.Time) ([]entry, int, bool) {
		es, err := readLog(s.ctx, c, f.src, q)
		switch {
		case s.interrupted():
			return nil, s.exitCode(), true
		case err != nil && isGone(err):
			if !f.gone {
				f.gone = true
				note("%s is gone — still following %s", f.src, name)
			}
			return nil, exitcode.OK, false
		case err != nil && !q.sinceTime.IsZero() && refusesSinceTime(err):
			// The next round reads this container by window, from the read
			// before: nothing is lost by the one that was refused.
			ae, _ := asAPIError(err)
			byWindow("refuses sinceTime (" + ae.said + ")")
			return nil, exitcode.OK, false
		case err != nil && stopsAFollow(err):
			return nil, fail(err), true
		case err != nil:
			if ae, ok := asAPIError(err); ok && ae.msg != f.trouble {
				f.trouble = ae.msg
				note("%s — still following, trying again", ae.msg)
			}
			return nil, exitcode.OK, false
		}
		if f.trouble != "" || f.gone {
			f.trouble, f.gone = "", false
			note("reading %s again", f.src)
		}
		if !q.sinceTime.IsZero() && ignoredSinceTime(es, q.sinceTime) {
			// What came back is still told apart by its stamps, so this round
			// prints right all the same; the next ones ask for less.
			byWindow("does not read sinceTime")
		}
		fresh := es
		if f.read {
			fresh = f.fresh(es)
			if len(es) >= maxTail && len(fresh) == len(es) {
				note("%s wrote more than %d lines between two reads — some of them were not read", f.src, maxTail)
			}
		}
		f.read, f.readAt = true, at
		f.remember(fresh)
		return fresh, exitcode.OK, false
	}

	var batch []entry
	for _, src := range order {
		es, code, stop := read(tracked[src], first, now())
		if stop {
			return code
		}
		batch = append(batch, es...)
	}
	w.print(merge(batch))

	listTrouble, idle := "", false
	for {
		if !s.sleep(pollInterval) {
			return s.exitCode()
		}
		round := now()
		pods, err := listPods(s.ctx, c)
		switch {
		case s.interrupted():
			return s.exitCode()
		case err != nil && stopsAFollow(err):
			return fail(err)
		case err != nil:
			if ae, ok := asAPIError(err); ok && ae.msg != listTrouble {
				listTrouble = ae.msg
				note("%s — still following the pods it listed before", ae.msg)
			}
		default:
			listTrouble = ""
			current, _ := match(name, container, pods)
			present := map[source]bool{}
			for _, src := range current {
				present[src] = true
			}
			kept := order[:0]
			for _, src := range order {
				if present[src] {
					kept = append(kept, src)
					continue
				}
				if !tracked[src].gone {
					note("%s is gone", src)
				}
				delete(tracked, src)
			}
			order = kept
			for _, src := range current {
				if tracked[src] == nil {
					note("following %s too", src)
					track(src)
				}
			}
			switch {
			case len(order) == 0 && !idle:
				idle = true
				note("no pod of %s runs now — waiting for one", name)
			case len(order) > 0:
				idle = false
			}
		}

		batch = batch[:0]
		for _, src := range order {
			f := tracked[src]
			// A container with a line printed is read from that line's stamp on:
			// the cluster's own clock, so nothing in between is lost to this
			// machine's. One with none — or against a portal-api that does not
			// read sinceTime — by window: a container read before from where
			// that read began; one never read — a new pod, or one that was not up
			// yet — from when the follow began: everything it wrote since is new
			// here.
			q := query{tail: maxTail, sinceTime: f.last}
			if !bySinceTime || !f.read || f.last.IsZero() {
				from := start
				if f.read {
					from = f.readAt
				}
				q = query{tail: maxTail, since: seconds(round.Sub(from) + followMargin)}
			}
			es, code, stop := read(f, q, round)
			if stop {
				return code
			}
			batch = append(batch, es...)
		}
		w.print(merge(batch))
	}
}
