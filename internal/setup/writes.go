package setup

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zaentrum/zae/internal/exitcode"
)

// completion is GET and POST /api/portal/setup/complete: the record alone.
type completion struct {
	Completed *Completion `json:"completed"`
}

// scan starts a scan of the library — unless one is under way already, which
// the console's button waits out too. It does not ask: a scan reads the
// library into the catalog and changes nothing else, and the console does not
// ask either.
func scan(args []string) int {
	fs := flagSet("scan")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return usageErr("zae setup scan --url https://… takes no arguments")
	}
	base, code := baseOf(*rawURL)
	if code != exitcode.OK {
		return code
	}
	ctx := context.Background()
	c := newClient(base)
	d, _, err := read(ctx, c)
	if err != nil {
		return fail(err)
	}
	l := d.Library
	at := now()
	switch {
	case l.State == stateUnknown && strings.HasPrefix(l.Note, noCatalog):
		errf("not offered: %s has no catalog manager to scan with: %s", base, noteOr(l.Note))
		return exitcode.NotOffered
	case scanRunning(l.Scan, at):
		fmt.Fprintf(stdout, "%s: %s — nothing more started; follow it with zae setup --url %s\n", base, scanText(l, at), base)
		return exitcode.OK
	}
	var after Doc
	if err := c.do(ctx, "start a scan", http.MethodPost, scanPath, nil, &after); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "started a scan on %s — follow it with zae setup --url %s\n", base, base)
	head, more := libraryStep(after.Library, base, now())
	if after.Library.State != stateUnknown && len(more) > 0 {
		more = more[:1] // the scan, not the rest of the step
	}
	row(stdout, "library", head, more...)
	return exitcode.OK
}

// done marks setup done, so the launchpad stops showing the checklist. While
// steps are open it says which and asks first, as the console does; with
// every step done there is nothing to ask.
func done(args []string) int {
	fs := flagSet("done")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	yes := fs.Bool("yes", false, "mark it done without asking, steps open or not")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return usageErr("zae setup done --url https://… takes no arguments")
	}
	base, code := baseOf(*rawURL)
	if code != exitcode.OK {
		return code
	}
	// Up front, as every command that may ask: whether this one will depends
	// on what the checklist says, and a script should not.
	if code := requireTTY(*yes, "marking setup done"); code != exitcode.OK {
		return code
	}
	ctx := context.Background()
	c := newClient(base)
	d, _, err := read(ctx, c)
	if err != nil {
		return fail(err)
	}
	at := now()
	if d.Completed != nil {
		fmt.Fprintf(stdout, "setup on %s is marked done already, %s — nothing changed\n", base, completedText(d.Completed, at))
		return exitcode.OK
	}
	if open := openSteps(d); len(open) > 0 {
		done, total := progress(d)
		fmt.Fprintf(stdout, "%s — first-run setup · %d of %d steps done\n", base, done, total)
		for _, name := range open {
			head, _ := stepHead(d, name, base, at)
			row(stdout, name, head)
		}
		fmt.Fprintln(stdout, "marking setup done takes the checklist off the launchpad; zae setup reopen shows it again")
		if !*yes && !confirm(fmt.Sprintf("mark setup done on %s with %s open?", base, count(len(open), "step", "steps"))) {
			fmt.Fprintln(stdout, "nothing changed")
			return exitcode.Failed
		}
	}
	var r completion
	if err := c.do(ctx, "mark setup done", http.MethodPost, completePath, nil, &r); err != nil {
		return fail(err)
	}
	how := ""
	if r.Completed != nil {
		how = ", " + completedText(r.Completed, now())
	}
	fmt.Fprintf(stdout, "marked setup done on %s%s — the launchpad no longer shows the checklist; zae setup reopen --url %s shows it again\n", base, how, base)
	return exitcode.OK
}

// stepHead is one counted step's line of the checklist.
func stepHead(d *Doc, name, base string, at time.Time) (string, []string) {
	switch name {
	case "metadata":
		return metadataStep(d.Metadata, base, at)
	case "library":
		return libraryStep(d.Library, base, at)
	case "processing":
		return processingStep(d.Processing, base)
	}
	return devicesStep(d.Devices)
}

// reopen takes the record that setup was marked done away, so the launchpad
// shows the checklist to admins again. It does not ask: it changes what the
// launchpad shows and nothing else, and the console does not ask either.
func reopen(args []string) int {
	fs := flagSet("reopen")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return usageErr("zae setup reopen --url https://… takes no arguments")
	}
	base, code := baseOf(*rawURL)
	if code != exitcode.OK {
		return code
	}
	ctx := context.Background()
	c := newClient(base)
	var cur completion
	if err := c.do(ctx, "read whether setup is done", http.MethodGet, completePath, nil, &cur); err != nil {
		return fail(err)
	}
	if cur.Completed == nil {
		fmt.Fprintf(stdout, "setup on %s is open already — the launchpad shows the checklist to admins; nothing changed\n", base)
		return exitcode.OK
	}
	if err := c.do(ctx, "reopen setup", http.MethodDelete, completePath, nil, nil); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "reopened setup on %s — it was marked done %s; the launchpad shows the checklist to admins again\n",
		base, completedText(cur.Completed, now()))
	return exitcode.OK
}
