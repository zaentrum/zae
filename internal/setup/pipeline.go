package setup

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/zaentrum/zae/internal/exitcode"
)

// pipeline switches the media pipeline — analyzer, katalog-ingest, packager,
// transcoder — on the operator's resource: PATCH /api/portal/operator
// {"pipeline": true|false}, the operator console's write. It is a change to
// the platform, so like every other one it says what changes — what starts or
// stops, and what the transcoder needs — and asks first.
func pipeline(args []string) int {
	fs := flagSet("pipeline")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	yes := fs.Bool("yes", false, "switch it without asking")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 || (pos[0] != "on" && pos[0] != "off") {
		return usageErr("zae setup pipeline on|off --url https://… takes on or off")
	}
	on, word := pos[0] == "on", pos[0]
	base, code := baseOf(*rawURL)
	if code != exitcode.OK {
		return code
	}
	if code := requireTTY(*yes, "switching the media pipeline "+word); code != exitcode.OK {
		return code
	}
	ctx := context.Background()
	c := newClient(base)
	d, _, err := read(ctx, c)
	if err != nil {
		return fail(err)
	}
	p := d.Processing
	switch {
	case !p.Switchable || p.Pipeline == nil:
		errf("not offered: %s has no operator's resource, which is where the media pipeline is switched — %s", base, noOperator(p))
		return exitcode.NotOffered
	case *p.Pipeline == on:
		fmt.Fprintf(stdout, "the media pipeline of %s is %s already — nothing changed\n", base, word)
		head, _ := processingStep(p, base)
		row(stdout, "processing", head)
		return exitcode.OK
	}
	fmt.Fprintf(stdout, "%s — the media pipeline\n", base)
	row(stdout, "pipeline", onOff(!on)+" → "+word)
	if on {
		row(stdout, "starts", "analyzer, katalog-ingest, packager and transcoder — the operator rolls them out, and each title is analyzed, transcoded and packaged for adaptive streaming once it is in the catalog")
		row(stdout, "gpu", gpuNeed(p))
	} else {
		row(stdout, "stops", "analyzer, katalog-ingest, packager and transcoder")
		row(stdout, "keeps", "what is packaged stays playable; new titles stream as they are")
	}
	if !*yes && !confirm(fmt.Sprintf("turn %s the media pipeline of %s?", word, base)) {
		fmt.Fprintln(stdout, "nothing changed")
		return exitcode.Failed
	}
	var done struct {
		Generation int64 `json:"generation"`
	}
	if err := c.do(ctx, "switch the media pipeline "+word, http.MethodPatch, operatorPath, map[string]bool{"pipeline": on}, &done); err != nil {
		return pipelineRefused(base, err)
	}
	what := "starts its workers"
	if !on {
		what = "stops its workers"
	}
	fmt.Fprintf(stdout, "turned the media pipeline of %s %s — the operator %s; follow it with zae setup --url %s\n", base, word, what, base)
	return exitcode.OK
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// noOperator says why there is no resource to switch the pipeline on: the
// portal's own note when it gave one.
func noOperator(p Processing) string {
	if strings.TrimSpace(p.Note) != "" {
		return printable(strings.TrimSpace(p.Note))
	}
	return "the portal reads no operator's resource here; the pipeline is switched through whatever deploys the platform"
}

// gpuNeed says what the transcoder needs, and whether a node offers it.
func gpuNeed(p Processing) string {
	switch {
	case p.GPUNodes == nil:
		return "the transcoder needs an NVIDIA GPU (nvidia.com/gpu); without a node that offers one it waits, and titles go no further than analysis"
	case *p.GPUNodes == 0:
		return "the transcoder needs an NVIDIA GPU, and no node offers one: it waits, and titles go no further than analysis"
	}
	return "the transcoder runs on a node that offers an NVIDIA GPU — " + strings.TrimPrefix(gpuLine(p), "GPU: ")
}

// pipelineRefused maps what a refused switch answered. A portal-api older than
// the switch refuses the field as one it does not know, and one without an
// operator's resource says it has none to configure — both definitive.
func pipelineRefused(base string, err error) int {
	if ae, ok := asAPIError(err); ok && ae.status == http.StatusBadRequest {
		switch {
		case strings.Contains(ae.said, `unknown field "pipeline"`):
			errf(`not offered: %s cannot switch the media pipeline — its portal-api predates the switch (PATCH %s {"pipeline": …})`, base, operatorPath)
			return exitcode.NotOffered
		case strings.Contains(ae.said, "no operator instance"):
			errf("not offered: %s has no operator's resource, which is where the media pipeline is switched: %s", base, ae.said)
			return exitcode.NotOffered
		}
	}
	return fail(err)
}
