package platform

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
)

// settle is a wait's verdict on one reading of the console: whether what was
// asked for has happened, and what the situation is either way. One string
// serves both jobs — it is the progress line while the wait runs, and the
// explanation when it runs out — so a timeout can never be less informative
// than the progress was.
type settle func(*Console) (done bool, state string)

// readinessOnly is what zae says when the instance cannot tell it what a write
// produced. It is printed once, before such a wait, because the difference
// matters: this wait can be satisfied by pods that were already ready.
const readinessOnly = "note: this instance reports no rollout generation, so this wait is a readiness gate — " +
	"it can be satisfied by the pods that were already running. Its portal-api predates the exact wait."

// poll reads the console until check accepts it or timeout passes, printing
// the state only when it changes. A portal failing for a moment is asked
// again — a restarting portal-api is not a verdict about the platform —
// while anything else ends the wait at once.
//
// delay waits one interval before the first reading. It is the old workaround
// for a rollout that has not started yet, kept for the one case that still
// needs it: an instance that reports no generations, where the counters are
// all zae has.
func (c *client) poll(ctx context.Context, timeout time.Duration, delay bool, check settle) (done bool, state string, err error) {
	start := time.Now()
	state, seen := "no answer yet", ""
	if delay && !sleep(ctx, pollInterval) {
		return false, state, ctx.Err()
	}
	for {
		cons, _, cerr := c.console(ctx)
		if cerr == nil {
			var ok bool
			ok, state = check(cons)
			if state != seen {
				seen = state
				fmt.Fprintf(stdout, "  %s\n", state)
			}
			if ok {
				return true, state, nil
			}
		} else if ae, isAPI := asAPIError(cerr); isAPI && ae.transient {
			state = ae.msg
		} else {
			return false, state, cerr
		}
		if time.Since(start) >= timeout {
			return false, state, nil
		}
		if !sleep(ctx, pollInterval) {
			return false, state, ctx.Err()
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// follow runs one wait and maps its outcome onto the exit codes: 0 when it
// settled, 1 when it did not, and the call's own code when the portal stopped
// answering for a reason waiting cannot fix. exact says the instance told zae
// what the write produced; when it did not, zae says so and falls back to the
// readiness gate it had before.
func follow(ctx context.Context, c *client, waitingFor, settledMsg string, timeout time.Duration, exact bool, check settle) int {
	if !exact {
		fmt.Fprintln(stdout, readinessOnly)
	}
	fmt.Fprintf(stdout, "waiting for %s (timeout %s)\n", waitingFor, timeout)
	done, state, err := c.poll(ctx, timeout, !exact, check)
	switch {
	case err != nil:
		return fail(err)
	case !done:
		errf("failed: still waiting for %s after %s — %s", waitingFor, timeout, state)
		return exitcode.Failed
	}
	fmt.Fprintln(stdout, settledMsg)
	return exitcode.OK
}

// platformSettled waits for the operator to have reconciled the write zae
// made — generation gen of its resource — to report the version that was asked
// for, and for every workload it manages to be rolled out and ready.
//
// target is empty when there is no version to wait for: nothing was pinned, or
// `latest` was, which is not a version the platform can report reaching.
func platformSettled(target string, gen int64) settle {
	return func(c *Console) (bool, string) {
		if !c.offered() {
			return false, c.noConsole("the instance")
		}
		op := c.Operator
		managed := c.managed()
		var pending []string
		for _, w := range managed {
			if !w.settled() {
				pending = append(pending, w.state())
			}
		}
		running := strings.TrimSpace(op.CurrentVersion)
		state := fmt.Sprintf("%-12s %s · %d/%d ready", phaseOr(op.Phase), dash(running), len(managed)-len(pending), len(managed))
		// Everything that is still outstanding, not just the first of them:
		// a wait that runs out has to explain itself, and it can only say
		// what its progress line already said.
		var why []string
		if gen > 0 && op.ObservedGeneration > 0 && op.ObservedGeneration < gen {
			why = append(why, fmt.Sprintf("the operator has not reconciled this change yet (at %d, waiting for %d)", op.ObservedGeneration, gen))
		}
		if target != "" && running != target {
			why = append(why, "waiting for "+target)
		}
		if len(pending) > 0 {
			why = append(why, strings.Join(pending, ", "))
		}
		if c.Error != "" {
			why = append(why, "the portal could not list the workloads: "+c.Error)
		}
		if len(why) == 0 {
			return true, state
		}
		return false, state + " — " + strings.Join(why, "; ")
	}
}

// rollout is what a write to one workload produced, and what the workload
// looked like before it — together, the gate a wait closes on.
type rollout struct {
	// generation the write produced; 0 when the instance did not say.
	generation int64
	// stamp is the rollout-restart stamp the write wrote, and before the one
	// it replaced. stamp is empty when this write is not a restart, when the
	// instance does not report the stamp, or when the write set the one that
	// was already there — two restarts in one second change nothing, exactly
	// as kubectl's do, and a wait must not sit there expecting otherwise.
	stamp, before string
	// exact: the instance reports rollout generations, so the wait follows
	// this write rather than whatever happens to be ready.
	exact bool
}

// rolloutOf reads a write's answer against the workload as it was before.
func rolloutOf(w write, before *Workload) rollout {
	r := rollout{generation: w.Generation, before: before.RestartedAt,
		exact: w.Generation > 0 && before.reportsRollout()}
	if w.RestartedAt != "" && w.RestartedAt != before.RestartedAt {
		r.stamp = w.RestartedAt
	}
	return r
}

// workloadSettled waits for one workload. want is the replica count a scale
// asked for; -1 for a restart, which asks for no change in count.
func workloadSettled(name string, want int, r rollout) settle {
	return func(c *Console) (bool, string) {
		w := c.find(name)
		switch {
		case !c.offered():
			return false, c.noConsole("the instance")
		case w == nil:
			return false, name + " is no longer among the instance's workloads"
		case want >= 0 && w.DesiredReplicas != want:
			return false, fmt.Sprintf("%s — the platform still asks for %d", w.state(), w.DesiredReplicas)
		case !w.observed(r.generation):
			// The rollout zae asked for has not started. This is the window in
			// which everything else below would say yes about the old pods.
			return false, w.state() + " — the rollout has not started yet"
		case r.stamp != "" && w.RestartedAt == r.before:
			// The stamp has not moved, so these are still the pods from before
			// the restart — however ready they say they are. A later restart by
			// somebody else moves it too, and supersedes this one: waiting for
			// THEIR rollout is right, waiting for a stamp that will never come
			// back is not.
			return false, w.state() + " — still running the rollout from before this restart"
		}
		return w.settled(), w.state()
	}
}

func phaseOr(p string) string {
	if strings.TrimSpace(p) == "" {
		return "no phase"
	}
	return p
}
