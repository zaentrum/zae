package setup

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

// switched is the one PATCH the portal was sent, decoded.
func (p *fakePortal) switched(t *testing.T) map[string]any {
	t.Helper()
	bodies := p.bodies["PATCH "+operatorPath]
	if len(bodies) != 1 {
		t.Fatalf("want one PATCH %s, got %d: %v", operatorPath, len(bodies), p.calls)
	}
	var b map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &b); err != nil {
		t.Fatalf("the PATCH is not JSON: %v %q", err, bodies[0])
	}
	return b
}

// Switching the pipeline is a change to the platform: what starts, what the
// transcoder needs — then the question — and only then the write, carrying the
// switch and nothing else.
func TestPipelineOnSaysWhatStartsAndAsksFirst(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	code, out, errs := runWith(t, "y\n", true, "pipeline", "on", "--url", srv.URL)
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d\n%s\n%s", code, out, errs)
	}
	if b := p.switched(t); len(b) != 1 || b["pipeline"] != true {
		t.Fatalf(`the PATCH is {"pipeline": true} alone: %v`, b)
	}
	for _, s := range []string{
		srv.URL + " — the media pipeline",
		"  pipeline     off → on",
		"  starts       analyzer, katalog-ingest, packager and transcoder — the operator rolls them out",
		"  gpu          the transcoder needs an NVIDIA GPU (nvidia.com/gpu); without a node that offers one it waits, and titles go no further than analysis",
		"turn on the media pipeline of " + srv.URL + "? [y/N] ",
		"turned the media pipeline of " + srv.URL + " on — the operator starts its workers; follow it with zae setup --url " + srv.URL,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("pipeline on lacks %q:\n%s", s, out)
		}
	}
	if strings.Index(out, "  starts ") > strings.Index(out, "[y/N]") {
		t.Errorf("what changes is printed before the question:\n%s", out)
	}
	// The checklist then reads it as starting.
	if _, out, _ := run(t, "--url", srv.URL); !strings.Contains(out, "  processing   starting — the media pipeline is starting: analyzer, katalog-ingest, packager and transcoder are not ready yet") {
		t.Errorf("the checklist after the switch:\n%s", out)
	}
}

// A no switches nothing; without a terminal, zae will not ask — --yes is
// needed, and its absence fails before anything is read or written.
func TestPipelineWillNotSwitchUnasked(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	code, out, errs := runWith(t, "n\n", true, "pipeline", "on", "--url", srv.URL)
	if code != exitcode.Failed || !strings.Contains(out, "nothing changed") || p.called("PATCH "+operatorPath) != 0 {
		t.Fatalf("declined: want 1 and no write, got %d %v\n%s\n%s", code, p.calls, out, errs)
	}
	calls := len(p.calls)
	code, _, errs = runWith(t, "y\n", false, "pipeline", "on", "--url", srv.URL)
	if code != exitcode.Usage || !strings.Contains(errs, "stdin is not a terminal, so zae will not ask before switching the media pipeline on — add --yes") || len(p.calls) != calls {
		t.Fatalf("no terminal, no --yes: want 2 before any call, got %d %q %v", code, errs, p.calls[calls:])
	}
	code, out, _ = run(t, "pipeline", "on", "--url", srv.URL, "--yes")
	if code != exitcode.OK || strings.Contains(out, "[y/N]") || p.called("PATCH "+operatorPath) != 1 {
		t.Fatalf("--yes: want 0, no question and one write, got %d %v\n%s", code, p.calls, out)
	}
}

// Off: what stops, and what stays playable.
func TestPipelineOff(t *testing.T) {
	p, srv := newPortal(t, setUp())
	code, out, errs := runWith(t, "yes\n", true, "pipeline", "off", "--url", srv.URL)
	if code != exitcode.OK || p.switched(t)["pipeline"] != false {
		t.Fatalf("want 0 and the switch off, got %d %v\n%s\n%s", code, p.bodies, out, errs)
	}
	for _, s := range []string{
		"  pipeline     on → off",
		"  stops        analyzer, katalog-ingest, packager and transcoder",
		"  keeps        what is packaged stays playable; new titles stream as they are",
		"turn off the media pipeline of " + srv.URL + "? [y/N] ",
		"turned the media pipeline of " + srv.URL + " off — the operator stops its workers",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("pipeline off lacks %q:\n%s", s, out)
		}
	}
}

// What the GPU line says depends on what the portal can see of the nodes.
func TestPipelineOnSaysWhetherANodeOffersTheGPU(t *testing.T) {
	for nodes, want := range map[int]string{
		0: "  gpu          the transcoder needs an NVIDIA GPU, and no node offers one: it waits, and titles go no further than analysis",
		1: "  gpu          the transcoder runs on a node that offers an NVIDIA GPU — 1 node offers one",
		3: "  gpu          the transcoder runs on a node that offers an NVIDIA GPU — 3 nodes offer one",
	} {
		d := freshBox()
		d.Processing.GPUNodes = ptr(nodes)
		_, srv := newPortal(t, d)
		if _, out, _ := runWith(t, "n\n", true, "pipeline", "on", "--url", srv.URL); !strings.Contains(out, want) {
			t.Errorf("%d GPU nodes: want %q:\n%s", nodes, want, out)
		}
	}
}

// Switched already: nothing is written, and that is no failure.
func TestPipelineAlreadySwitched(t *testing.T) {
	p, srv := newPortal(t, setUp())
	code, out, _ := runWith(t, "y\n", true, "pipeline", "on", "--url", srv.URL)
	if code != exitcode.OK || p.called("PATCH "+operatorPath) != 0 || strings.Contains(out, "[y/N]") ||
		!strings.Contains(out, "the media pipeline of "+srv.URL+" is on already — nothing changed") ||
		!strings.Contains(out, "  processing   done — the media pipeline runs") {
		t.Fatalf("on already: want 0, no question and no write, got %d %v\n%s", code, p.calls, out)
	}
	p2, srv2 := newPortal(t, freshBox())
	if code, out, _ := run(t, "pipeline", "off", "--url", srv2.URL, "--yes"); code != exitcode.OK || p2.called("PATCH "+operatorPath) != 0 || !strings.Contains(out, "is off already") {
		t.Fatalf("off already: want 0 and no write, got %d %v\n%s", code, p2.calls, out)
	}
}

// Where there is nothing to switch — no operator's resource, an instance that
// manages no workloads, a portal-api that predates the switch — it is not
// offered, in the portal's words.
func TestPipelineWhereItCannotBeSwitched(t *testing.T) {
	d := freshBox()
	d.Processing = Processing{Step: Step{State: stateUnknown, Note: "instance management is unavailable (not running in a cluster)"}, Workers: []Worker{}}
	p, srv := newPortal(t, d)
	code, out, errs := run(t, "pipeline", "on", "--url", srv.URL, "--yes")
	if code != exitcode.NotOffered || p.called("PATCH "+operatorPath) != 0 ||
		!strings.Contains(errs, "has no operator's resource, which is where the media pipeline is switched — instance management is unavailable") {
		t.Errorf("no operator's resource: want 3 without a write, got %d %v\n%s\n%s", code, p.calls, out, errs)
	}

	p2, srv2 := newPortal(t, freshBox())
	p2.legacyOperator = true
	if code, _, errs := run(t, "pipeline", "on", "--url", srv2.URL, "--yes"); code != exitcode.NotOffered || !strings.Contains(errs, "its portal-api predates the switch") {
		t.Errorf("a portal-api older than the switch: want 3, got %d %q", code, errs)
	}
	p3, srv3 := newPortal(t, freshBox())
	p3.outside = true
	if code, _, errs := run(t, "pipeline", "on", "--url", srv3.URL, "--yes"); code != exitcode.NotOffered || !strings.Contains(errs, "does not manage its own workloads") {
		t.Errorf("an instance that manages no workloads: want 3, got %d %q", code, errs)
	}
	p4, srv4 := newPortal(t, freshBox())
	p4.old = true
	if code, _, errs := run(t, "pipeline", "on", "--url", srv4.URL, "--yes"); code != exitcode.NotOffered || p4.called("PATCH "+operatorPath) != 0 {
		t.Errorf("a portal-api without the checklist: want 3 without a write, got %d %q", code, errs)
	}
	p5, srv5 := newPortal(t, freshBox())
	p5.token = "the-admin-bearer"
	t.Setenv(instance.TokenEnv, "")
	if code, _, errs := run(t, "pipeline", "on", "--url", srv5.URL, "--yes"); code != exitcode.Forbidden {
		t.Errorf("without the admin role: want 5, got %d %q", code, errs)
	}
}

func TestPipelineUsage(t *testing.T) {
	p, srv := newPortal(t, freshBox())
	usageCases(t, p, map[string][]string{
		"no switch":      {"pipeline", "--url", srv.URL, "--yes"},
		"not a switch":   {"pipeline", "maybe", "--url", srv.URL, "--yes"},
		"two switches":   {"pipeline", "on", "off", "--url", srv.URL, "--yes"},
		"a switch, true": {"pipeline", "true", "--url", srv.URL, "--yes"},
		"no url":         {"pipeline", "on", "--yes"},
	})
}
