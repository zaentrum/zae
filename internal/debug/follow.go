package debug

import (
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
)

// followed is one container a follow reads: the newest line it has printed,
// and when it last read it.
//
// The portal has no stream to subscribe to — the console's "live" button
// reads the log again every few seconds, and so does this. Each read asks for
// the window since the last one, widened by followMargin, and prints only the
// lines newer than the newest it printed: the cluster stamps every line, to
// the nanosecond, so a line read twice is recognised by its stamp and not by
// guessing at overlaps.
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
			// A container read before is read from where that read began; one
			// never read — a new pod, or one that was not up yet — from when
			// the follow began: everything it wrote since is new here.
			from := start
			if f.read {
				from = f.readAt
			}
			q := query{tail: maxTail, since: seconds(round.Sub(from) + followMargin)}
			es, code, stop := read(f, q, round)
			if stop {
				return code
			}
			batch = append(batch, es...)
		}
		w.print(merge(batch))
	}
}
