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

// poll reads the console until check accepts it or timeout passes, printing
// the state only when it changes. A portal failing for a moment is asked
// again — a restarting portal-api is not a verdict about the platform —
// while anything else ends the wait at once.
//
// delay waits one interval before the first reading. A rollout that has just
// been asked for has not started yet, and the pods still running are the ones
// from before it: reading immediately would accept them.
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
// answering for a reason waiting cannot fix.
func follow(ctx context.Context, c *client, waitingFor, settledMsg string, timeout time.Duration, delay bool, check settle) int {
	fmt.Fprintf(stdout, "waiting for %s (timeout %s)\n", waitingFor, timeout)
	done, state, err := c.poll(ctx, timeout, delay, check)
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

// platformSettled waits for the operator to report the version that was asked
// for, and for every workload it manages to be ready. target is empty when
// there is no version to wait for — nothing was pinned, or `latest` was, which
// is not a version the platform can report reaching.
func platformSettled(target string) settle {
	return func(c *Console) (bool, string) {
		op := c.Operator
		managed := c.managed()
		var pending []string
		for _, w := range managed {
			if !w.settled() {
				pending = append(pending, w.state())
			}
		}
		if !c.offered() {
			return false, c.noConsole("the instance")
		}
		running := strings.TrimSpace(op.CurrentVersion)
		state := fmt.Sprintf("%-12s %s · %d/%d ready", phaseOr(op.Phase), dash(running), len(managed)-len(pending), len(managed))
		// Everything that is still outstanding, not just the first of them:
		// a wait that runs out has to explain itself, and it can only say
		// what its progress line already said.
		var why []string
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

// workloadSettled waits for one workload. want is the replica count a scale
// asked for; -1 for a restart, which asks for no change in count.
func workloadSettled(name string, want int) settle {
	return func(c *Console) (bool, string) {
		w := c.find(name)
		switch {
		case !c.offered():
			return false, c.noConsole("the instance")
		case w == nil:
			return false, name + " is no longer among the instance's workloads"
		case want >= 0 && w.DesiredReplicas != want:
			return false, fmt.Sprintf("%s — the platform still asks for %d", w.state(), w.DesiredReplicas)
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
