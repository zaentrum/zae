package addon

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
)

// input is one install input a chart's values.schema.json declares.
type input struct {
	path     string
	secret   bool   // "writeOnly": true — masked, stored only in the values Secret
	generate string // "x-zaentrum-generate": the operator generates it when not given
	required bool
	def      json.RawMessage // the schema default, nil when none
}

// schemaNode is the part of JSON Schema that describes inputs.
type schemaNode struct {
	Properties map[string]*schemaNode `json:"properties"`
	Required   []string               `json:"required"`
	WriteOnly  bool                   `json:"writeOnly"`
	Generate   string                 `json:"x-zaentrum-generate"`
	Default    json.RawMessage        `json:"default"`
}

// schemaInputs lists the inputs of a values schema, by dotted path. Objects
// with properties are walked; everything else is an input. The reserved
// platform key is not an input.
func schemaInputs(raw schemaDoc) ([]input, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return nil, nil
	}
	var root schemaNode
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		return nil, err
	}
	var out []input
	var walk func(n *schemaNode, prefix string)
	walk = func(n *schemaNode, prefix string) {
		keys := make([]string, 0, len(n.Properties))
		for k := range n.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := n.Properties[k]
			if child == nil || (prefix == "" && k == reservedKey) {
				continue
			}
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			if len(child.Properties) > 0 && !child.WriteOnly && child.Generate == "" {
				walk(child, path)
				continue
			}
			out = append(out, input{path: path, secret: child.WriteOnly, generate: child.Generate,
				required: slices.Contains(n.Required, k), def: child.Default})
		}
	}
	walk(&root, "")
	return out, nil
}

// phaseOr names a phase, or says there is none yet.
func phaseOr(p string) string {
	if p == "" {
		return "no status yet"
	}
	return p
}

// renderPlan prints what the operator would apply: the chart, the workloads
// with their images and ports, the objects, what changes against what runs,
// the inputs (secret ones only as set or missing), and everything that blocks
// an install. In a status the phase is already printed above the plan.
func renderPlan(w io.Writer, a *Addon, inStatus bool) {
	p := a.Plan
	if p == nil {
		fmt.Fprintf(w, "%s — %s, no plan", a.Name, phaseOr(a.Phase))
		if a.Message != "" {
			fmt.Fprintf(w, ": %s", a.Message)
		}
		fmt.Fprintln(w)
		return
	}
	header := strings.TrimSpace(p.Chart.Name + " " + p.Chart.Version)
	if header == "" {
		header = "unnamed chart"
	}
	if inStatus {
		fmt.Fprintf(w, "\ncurrent plan — chart %s\n", header)
	} else {
		fmt.Fprintf(w, "\nplan for %s — chart %s — %s\n", a.Name, header, phaseOr(a.Phase))
		if a.Message != "" && a.Phase != PhasePlanned {
			fmt.Fprintf(w, "  %s\n", a.Message)
		}
	}
	if p.Chart.Description != "" {
		label(w, "description", p.Chart.Description)
	}
	if p.Chart.AppVersion != "" {
		label(w, "app version", p.Chart.AppVersion)
	}
	if p.Chart.Digest != "" {
		label(w, "digest", p.Chart.Digest)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(p.Workloads) > 0 {
		fmt.Fprintln(w, "  workloads")
		for _, wl := range p.Workloads {
			ports := make([]string, 0, len(wl.Ports))
			for _, raw := range wl.Ports {
				ports = append(ports, portString(raw))
			}
			images := wl.Images
			if len(images) == 0 {
				images = []string{"(no image)"}
			}
			for i, img := range images {
				name := wl.Kind + "/" + wl.Name
				if i > 0 {
					name = ""
				}
				if i == 0 && len(ports) > 0 {
					fmt.Fprintf(tw, "    %s\t%s\tports %s\n", name, img, strings.Join(ports, ", "))
				} else {
					fmt.Fprintf(tw, "    %s\t%s\n", name, img)
				}
			}
		}
		tw.Flush()
	}
	if len(p.Objects) > 0 {
		label(w, "objects", fmt.Sprintf("%d (%s)", len(p.Objects), kindCounts(p.Objects)))
	}
	if !p.Changes.empty() {
		fmt.Fprintln(w, "  changes against what runs")
		for _, s := range p.Changes.Added {
			fmt.Fprintf(w, "    + %s\n", s)
		}
		for _, s := range p.Changes.Removed {
			fmt.Fprintf(w, "    - %s\n", s)
		}
		for _, s := range p.Changes.Images {
			fmt.Fprintf(w, "    ~ %s\n", s)
		}
	}
	missing := renderInputs(w, a)
	if len(p.Violations) > 0 {
		fmt.Fprintln(w, "  refused by the guardrails — install is blocked")
		for _, v := range p.Violations {
			fmt.Fprintf(w, "    ✗ %s\n", v)
		}
	}
	if len(p.ValuesErrors) > 0 {
		fmt.Fprintln(w, "  values errors — install is blocked")
		for _, v := range p.ValuesErrors {
			fmt.Fprintf(w, "    ✗ %s\n", v)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(w, "  still to give: %s\n", strings.Join(missing, "  "))
	}
	fmt.Fprintln(w)
}

// renderInputs prints the schema's inputs and their state. A secret input is
// only ever "set" or not; a given value is shown, since it is not secret by
// the chart's own declaration. When the plan has values errors, it returns
// the required inputs nobody gave, each with the flag that gives it.
func renderInputs(w io.Writer, a *Addon) (missing []string) {
	inputs, err := schemaInputs(a.Plan.ValuesSchema)
	if err != nil {
		label(w, "inputs", fmt.Sprintf("values.schema.json does not parse: %v", err))
		return nil
	}
	if len(inputs) == 0 {
		return nil
	}
	failed := len(a.Plan.ValuesErrors) > 0
	values := a.valuesMap()
	var plain []string
	fmt.Fprintln(w, "  inputs")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, in := range inputs {
		kind := "value"
		if in.secret {
			kind = "secret"
		}
		given, inValues := lookup(values, in.path)
		inSecrets := slices.Contains(a.SecretKeys, in.path)
		var state string
		switch {
		case inSecrets:
			state = "set"
		case inValues && in.secret:
			state = "set"
			plain = append(plain, in.path)
		case inValues:
			state = compact(given)
		case in.generate != "":
			state = "generated by the operator"
		case len(in.def) > 0:
			state = "default " + compactRaw(in.def)
		case in.required && failed:
			state = "missing"
			hint := "--set " + in.path + "=…"
			if in.secret {
				hint = "--set-secret " + in.path
			}
			missing = append(missing, hint)
		case in.required:
			state = "from the chart's values"
		default:
			state = "not set"
		}
		if in.required {
			kind += ", required"
		}
		fmt.Fprintf(tw, "    %s\t%s\t%s\n", in.path, kind, state)
	}
	tw.Flush()
	for _, p := range plain {
		fmt.Fprintf(w, "  ! %s is a secret input given as a plain value: it is readable in the addon resource. Give it with --set-secret instead.\n", p)
	}
	return missing
}

// renderStatus prints one addon: its phase, what runs, its components and its
// current plan.
func renderStatus(w io.Writer, a *Addon) {
	state := phaseOr(a.Phase)
	if a.Suspended {
		state += " · suspended: plans only, applies nothing"
	}
	fmt.Fprintf(w, "%s — %s\n", a.Name, state)
	if a.Message != "" {
		fmt.Fprintf(w, "  %s\n", a.Message)
	}
	if a.Chart != nil && a.Chart.Ref != "" {
		label(w, "chart", chartLine(a.Chart))
	}
	if a.installed() {
		label(w, "running", chartLine(a.LastAppliedChart))
	} else {
		label(w, "running", "nothing yet — never installed")
	}
	switch {
	case a.Registered:
		label(w, "registered", "yes")
	case a.RegistrationError != "":
		label(w, "registered", "no — "+a.RegistrationError)
	case a.Phase == PhaseReady:
		label(w, "registered", "not yet")
	default:
		label(w, "registered", "no — the portal registers it once it is Ready")
	}
	if len(a.Components) > 0 {
		fmt.Fprintln(w, "  components")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, c := range a.Components {
			if c.Reason != "" {
				fmt.Fprintf(tw, "    %s\t%s\t%d/%d\t%s\n", c.Name, c.Kind, c.Ready, c.Desired, c.Reason)
			} else {
				fmt.Fprintf(tw, "    %s\t%s\t%d/%d\n", c.Name, c.Kind, c.Ready, c.Desired)
			}
		}
		tw.Flush()
	}
	if a.Plan != nil {
		renderPlan(w, a, true)
	}
}

// renderList prints every installed addon. Chart addons show their chart, the
// version asked for next to the one running, and their phase; addons installed
// from an address show the address and the manifest's version.
func renderList(w io.Writer, rows []Listed) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no addons installed")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSOURCE\tVERSION\tPHASE\tREADY")
	for _, r := range rows {
		name := r.Key
		if name == "" {
			name = r.Name
		}
		source, version, phase := r.ProxyURL, dash(r.Version), "installed"
		if r.Chart != nil && r.Chart.Ref != "" {
			source, phase = r.Chart.Ref, phaseOr(r.Phase)
			version = listVersion(r.Chart)
			if !r.Registered && r.Phase == PhaseReady {
				phase += ", not registered"
			}
		}
		if r.Suspended {
			phase += " (suspended)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, dash(source), version, phase, readyCount(r.Components))
	}
	tw.Flush()
}

// listVersion is the version a chart addon asks for, with the one running
// when that differs.
func listVersion(c *listChart) string {
	asked := dash(c.Version)
	switch {
	case c.LastApplied == nil || (c.LastApplied.Ref == "" && c.LastApplied.Version == ""):
		return asked + " (not running)"
	case c.LastApplied.Ref != c.Ref:
		return asked + " (runs " + chartLine(c.LastApplied) + ")"
	case c.LastApplied.Version != c.Version:
		return asked + " (runs " + dash(c.LastApplied.Version) + ")"
	}
	return asked
}

// componentsLine is one progress line: each component's ready/desired.
func componentsLine(cs []Component) string {
	if len(cs) == 0 {
		return "no components reported yet"
	}
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		s := fmt.Sprintf("%s %d/%d", c.Name, c.Ready, c.Desired)
		if c.Reason != "" {
			s += " (" + c.Reason + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func readyCount(cs []listComponent) string {
	if len(cs) == 0 {
		return "-"
	}
	ready := 0
	for _, c := range cs {
		if c.Ready != nil && c.Desired != nil && *c.Desired > 0 && *c.Ready >= *c.Desired {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(cs))
}

func chartLine(c *Chart) string {
	if c == nil {
		return "-"
	}
	s := strings.TrimSpace(c.Ref + " " + c.Version)
	if c.Digest != "" {
		s += " (" + c.Digest + ")"
	}
	return s
}

func kindCounts(objs []Object) string {
	counts := map[string]int{}
	for _, o := range objs {
		counts[o.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// portString renders a port however the plan spells it: 8080, "8080/TCP",
// or {"name":"http","containerPort":8080,"protocol":"TCP"}.
func portString(raw json.RawMessage) string {
	var n json.Number
	if json.Unmarshal(raw, &n) == nil && n != "" {
		return n.String()
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Name          string      `json:"name"`
		ContainerPort json.Number `json:"containerPort"`
		Port          json.Number `json:"port"`
		Protocol      string      `json:"protocol"`
	}
	if json.Unmarshal(raw, &o) == nil {
		p := o.ContainerPort.String()
		if p == "" {
			p = o.Port.String()
		}
		if p != "" {
			if o.Protocol != "" {
				p += "/" + o.Protocol
			}
			if o.Name != "" {
				p = o.Name + " " + p
			}
			return p
		}
	}
	return string(raw)
}

// compact renders a given value on one line, shortened.
func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return compactRaw(b)
}

func compactRaw(b []byte) string {
	s := string(b)
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}

// label prints one "  name        value" row; every labelled row lines up.
func label(w io.Writer, name, value string) { fmt.Fprintf(w, "  %-12s%s\n", name, value) }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
