package debug

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
	"github.com/zaentrum/zae/internal/redact"
)

// maxEvents is how many events the portal's tap keeps.
const maxEvents = 500

// Topology is GET /api/portal/debug/kafka/topology, as much of it as zae
// reads: whether the portal has a bus to read at all, and its topics.
type Topology struct {
	Available bool     `json:"available"`
	Brokers   []string `json:"brokers"`
	Topics    []struct {
		Topic string `json:"topic"`
	} `json:"topics"`
	Note string `json:"note"`
}

func (t *Topology) topics() []string {
	out := make([]string, 0, len(t.Topics))
	for _, x := range t.Topics {
		out = append(out, x.Topic)
	}
	return out
}

// Event is one message the portal's tap read off the bus. Payload is the
// message as the portal keeps it: compacted, redacted and capped.
type Event struct {
	Seq       uint64    `json:"seq"`
	Topic     string    `json:"topic"`
	Partition int       `json:"partition"`
	Offset    int64     `json:"offset"`
	Key       string    `json:"key"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	ItemID    string    `json:"itemId"`
	Payload   string    `json:"payload"`
	Size      int       `json:"size"`
}

// events prints what the portal's event tap has read from the bus.
func events(args []string) int {
	s := newSession()
	defer s.close()

	fs := flagSet("events")
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	topic := fs.String("topic", "", "only this topic, by its full name")
	limit := fs.Int("limit", 0, "the newest N events, 1 to 500 (default: every one the tap keeps)")
	payload := fs.Bool("payload", false, "print each event's payload under it")
	asJSON := fs.Bool("json", false, "print the portal's answer as JSON, newest first, redacted again")
	pos, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 0 {
		return usageErr("zae debug events --url https://… takes no arguments — a topic is --topic NAME")
	}
	base, err := instance.Base(*rawURL)
	if err != nil {
		return usageErr("%v", err)
	}
	if setFlags(fs)["limit"] && (*limit < 1 || *limit > maxEvents) {
		return usageErr("--limit is 1 to %d — the tap keeps no more", maxEvents)
	}
	if *payload && *asJSON {
		return usageErr("--json carries every payload already; --payload is for the table")
	}

	c := newClient(base, 30*time.Second)
	topo, err := topology(s.ctx, c)
	if err != nil {
		return failOr(s, err)
	}
	if !topo.Available {
		// The events list answers [] either way; only the topology tells an
		// empty bus from no bus at all.
		errf("not offered: %s has no event bus to read: %s", base, dash(topo.Note))
		return exitcode.NotOffered
	}
	if t := strings.TrimSpace(*topic); t != "" && !contains(topo.topics(), t) {
		errf("not offered: the bus of %s has no topic %q — its topics: %s", base, t, dash(strings.Join(topo.topics(), ", ")))
		return exitcode.NotOffered
	}

	q := url.Values{}
	if t := strings.TrimSpace(*topic); t != "" {
		q.Set("topic", t)
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	path := eventsPath
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.get(s.ctx, "read the event tap", path, logLimit)
	if err != nil {
		return failOr(s, err)
	}
	if *asJSON {
		out, err := redact.JSON(raw, "")
		if err != nil {
			errf("undetermined: read the event tap: %s answered with something that is not JSON (%v)", base, err)
			return exitcode.Undetermined
		}
		fmt.Fprintln(stdout, string(out))
		return exitcode.OK
	}
	var evs []Event
	if err := json.Unmarshal(raw, &evs); err != nil {
		errf("undetermined: read the event tap: %s answered with JSON that is not a list of events (%v)", base, err)
		return exitcode.Undetermined
	}
	if len(evs) == 0 {
		where := "the bus"
		if *topic != "" {
			where = *topic
		}
		fmt.Fprintf(stdout, "no events — the tap has read nothing from %s since portal-api started; it reads what passes from then on, never a topic's history\n", where)
		return exitcode.OK
	}
	renderEvents(evs, *payload)
	return exitcode.OK
}

// topology reads whether there is a bus, and its topics.
func topology(ctx context.Context, c *client) (*Topology, error) {
	raw, err := c.get(ctx, "read the bus topology", topologyPath, logLimit)
	if err != nil {
		return nil, err
	}
	var t Topology
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, &apiError{code: exitcode.Undetermined,
			msg: fmt.Sprintf("undetermined: read the bus topology: %s answered with JSON that is not the event console's (%v) — does the address reach portal-api?", c.base, err)}
	}
	return &t, nil
}

// renderEvents prints the events oldest first, as a terminal reads a log: the
// newest line next to the prompt. The portal sends them newest first. A
// payload goes under its row, outside the columns — it is as wide as it is,
// and the columns stay aligned across the rows between.
func renderEvents(evs []Event, payload bool) {
	rows := [][]string{{"TIME", "TOPIC", "TYPE", "ITEM", "AT"}}
	payloads := []string{""}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		at := "-"
		if !e.Time.IsZero() {
			at = e.Time.UTC().Format("2006-01-02T15:04:05Z")
		}
		rows = append(rows, []string{at, dash(printable(e.Topic)), dash(printable(redact.Secrets(e.Type))),
			dash(printable(redact.Secrets(e.ItemID))), fmt.Sprintf("p%d·%d", e.Partition, e.Offset)})
		payloads = append(payloads, printable(redact.Secrets(e.Payload)))
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, cell := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}
	for n, r := range rows {
		var b strings.Builder
		for i, cell := range r {
			b.WriteString(cell)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)+2))
			}
		}
		fmt.Fprintln(stdout, b.String())
		if payload && n > 0 {
			fmt.Fprintf(stdout, "    %s\n", dash(payloads[n]))
		}
	}
}
