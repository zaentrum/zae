package addon

import (
	"errors"
	"fmt"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
)

// changedError: someone else wrote the addon after zae did, so the plan or
// the rollout zae waits for is no longer the one its write asked for.
type changedError struct {
	name       string
	wrote, now int64
}

func (e *changedError) Error() string {
	return fmt.Sprintf("failed: %s changed while zae waited — zae's write made generation %d, the addon is at %d now; someone else is changing it, so zae stops here (zae addon status %s shows what it is now)",
		e.name, e.wrote, e.now, e.name)
}

// planFor accepts the status the operator wrote for generation gen — the one
// zae's own write produced. Every write that changes what the operator plans
// bumps the generation, secret inputs included: portal-api never changes a
// values Secret, it creates a new one and points valuesFrom at it. So
// "observed ≥ gen" is exactly "a plan for this write". A portal that answers
// writes without a generation gets the weaker test: suspended and in a
// planning phase.
func planFor(name string, gen int64) func(*Addon) (bool, error) {
	return func(a *Addon) (bool, error) {
		if gen == 0 {
			return a.Suspended && (a.Phase == PhasePlanned || a.Phase == PhasePlanFailed || a.Phase == PhaseFailed), nil
		}
		if a.Generation > gen {
			return false, &changedError{name: name, wrote: gen, now: a.Generation}
		}
		return a.ObservedGeneration >= gen, nil
	}
}

// readyFor accepts the install zae just made (generation gen) once it is Ready
// and the portal has registered it — only then do its app, tiles, slot rows and
// CLI commands exist.
func readyFor(name string, gen int64) func(*Addon) (bool, error) {
	return func(a *Addon) (bool, error) {
		if gen > 0 && a.Generation > gen {
			return false, &changedError{name: name, wrote: gen, now: a.Generation}
		}
		return a.Phase == PhaseReady && a.Registered && !a.Suspended && a.ObservedGeneration >= gen, nil
	}
}

// poll reads the addon until check accepts it, check fails, or timeout passes,
// and returns the last document read; done is false on timeout. changed is
// called when the phase or a component's readiness moves. While waiting, no
// answer or a portal failing for a moment is asked again — a restarting
// portal-api is not a verdict — and so is a 404 within grace of the write that
// created the addon. An API that cannot manage chart addons at all ends the
// wait at once. The error is an *apiError, a *changedError or errInterrupted.
func (c *client) poll(s *session, name string, timeout, grace time.Duration, check func(*Addon) (bool, error), changed func(*Addon)) (*Addon, bool, error) {
	start := time.Now()
	var last *Addon
	var lastErr error
	seen := ""
	for {
		a, err := c.get(s.ctx, name)
		ae, isAPI := asAPIError(err)
		switch {
		case err == nil:
			last, lastErr = a, nil
			if key := progressKey(a); changed != nil && key != seen {
				seen = key
				changed(a)
			}
			ok, cerr := check(a)
			if cerr != nil {
				return a, false, cerr
			}
			if ok {
				return a, true, nil
			}
		case s.interrupted():
			return last, false, errInterrupted
		case isAPI && (ae.transient || (ae.notFound && !ae.noAPI && time.Since(start) < grace)):
			lastErr = err
		default:
			return last, false, err
		}
		if time.Since(start) >= timeout {
			if last == nil && lastErr != nil {
				return nil, false, lastErr
			}
			return last, false, nil
		}
		select {
		case <-time.After(pollInterval):
		case <-s.ctx.Done():
			return last, false, errInterrupted
		}
	}
}

func progressKey(a *Addon) string {
	return fmt.Sprintf("%s|%t|%d|%t|%s|%s", a.Phase, a.Suspended, a.ObservedGeneration, a.Registered, a.RegistrationError, componentsLine(a.Components))
}

// waitReady follows an install until the addon is Ready and registered for
// generation gen, printing progress as it moves.
func waitReady(s *session, c *client, name string, gen int64, timeout time.Duration) int {
	fmt.Fprintf(stdout, "waiting for %s to become Ready and registered (timeout %s)\n", name, timeout)
	a, done, err := c.poll(s, name, timeout, 0, readyFor(name, gen), func(a *Addon) {
		if a.Suspended || a.ObservedGeneration < gen {
			return // still the plan the install has not reached yet
		}
		line := componentsLine(a.Components)
		if a.Phase == PhaseReady && !a.Registered {
			line = "registering in the portal"
			if a.RegistrationError != "" {
				line += ": " + a.RegistrationError
			}
		}
		fmt.Fprintf(stdout, "  %-12s%s\n", phaseOr(a.Phase), line)
	})
	switch {
	case errors.Is(err, errInterrupted):
		fmt.Fprintf(stdout, "stopped waiting — %s is installed and keeps rolling out; zae addon status %s --url %s follows it\n", name, name, c.base)
		return s.exitCode()
	case err != nil:
		return fail(err)
	case !done && a != nil && a.Phase == PhaseReady && !a.Registered:
		why := "the portal has not registered it yet"
		if a.RegistrationError != "" {
			why = "the portal could not register it: " + a.RegistrationError
		}
		errf("failed: %s is Ready but not registered after %s — %s", name, timeout, why)
		return exitcode.Failed
	case !done:
		errf("failed: %s is not Ready after %s (%s) — zae addon status %s --url %s shows why", name, timeout, lastSeen(a), name, c.base)
		return exitcode.Failed
	}
	fmt.Fprintf(stdout, "%s is Ready and registered\n", name)
	return exitcode.OK
}
