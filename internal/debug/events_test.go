package debug

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/instance"
)

func TestEventsOldestFirstWithPayloads(t *testing.T) {
	p, srv := newPortal(t)
	p.events = `[
	  {"seq":3,"topic":"platform.item.removed","partition":0,"offset":12,"key":"Bearer ` + leaked + `","time":"2026-10-03T12:00:03Z","type":"removed","itemId":"item-2","payload":"{\"itemId\":\"item-2\"}","size":17},
	  {"seq":2,"topic":"platform.item.added","partition":1,"offset":40,"key":"k","time":"2026-10-03T12:00:02Z","type":"added\u001b[2J","itemId":"item-1","payload":"{\"token\":\"` + leaked + `\"}","size":30},
	  {"seq":1,"topic":"platform.item.added","partition":0,"offset":7,"key":"","time":"2026-10-03T12:00:01Z","payload":"plain text","size":10}
	]`
	code, out, errs := run(t, "events", "--url", srv.URL, "--payload")
	if code != exitcode.OK {
		t.Fatalf("want 0, got %d %s", code, errs)
	}
	want := "TIME                  TOPIC                  TYPE       ITEM    AT\n" +
		"2026-10-03T12:00:01Z  platform.item.added    -          -       p0·7\n" +
		"    plain text\n" +
		"2026-10-03T12:00:02Z  platform.item.added    added?[2J  item-1  p1·40\n" +
		"    {\"token\":\"***REDACTED***\"}\n" +
		"2026-10-03T12:00:03Z  platform.item.removed  removed    item-2  p0·12\n"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("oldest first, aligned, a payload under its row, no escape sequence:\n got:\n%s\nwant prefix:\n%s", out, want)
	}
	if strings.Contains(out, leaked) || strings.Contains(out, "\x1b") {
		t.Fatalf("a credential or an escape sequence reached the terminal:\n%s", out)
	}

	// --json: the portal's document, newest first, redacted again — the key
	// too, which the portal does not redact.
	code, out, _ = run(t, "events", "--url", srv.URL, "--json")
	var evs []Event
	if code != exitcode.OK || json.Unmarshal([]byte(out), &evs) != nil || len(evs) != 3 || evs[0].Seq != 3 {
		t.Fatalf("--json: %d\n%s", code, out)
	}
	if strings.Contains(out, leaked) {
		t.Fatalf("--json carries a credential:\n%s", out)
	}
}

func TestEventsTopicsAndTheBus(t *testing.T) {
	p, srv := newPortal(t)
	code, out, errs := run(t, "events", "--url", srv.URL, "--topic", "platform.item.added", "--limit", "50")
	if code != exitcode.OK || !strings.Contains(out, "no events — the tap has read nothing from platform.item.added") {
		t.Fatalf("an empty tap: want 0 saying so, got %d\n%s\n%s", code, out, errs)
	}
	if p.called("GET "+eventsPath+"?limit=50&topic=platform.item.added") != 1 {
		t.Fatalf("--topic and --limit must reach the portal: %v", p.calls)
	}

	code, _, errs = run(t, "events", "--url", srv.URL, "--topic", "platform.item.renamed")
	if code != exitcode.NotOffered || !strings.Contains(errs, "platform.item.added, platform.item.removed") {
		t.Fatalf("an unknown topic: want 3 naming the topics, got %d %q", code, errs)
	}

	p.topology = `{"available":false,"note":"Kafka introspection is unavailable (KAFKA_BROKERS unset)"}`
	code, _, errs = run(t, "events", "--url", srv.URL)
	if code != exitcode.NotOffered || !strings.Contains(errs, "KAFKA_BROKERS unset") {
		t.Fatalf("no bus: want 3 with the portal's note, got %d %q", code, errs)
	}
	if p.called("GET "+eventsPath) != 1 {
		t.Fatalf("without a bus there is nothing to read: %v", p.calls)
	}
}

func TestEventsUsageAndRefusals(t *testing.T) {
	p, srv := newPortal(t)
	usageCases(t, p, map[string][]string{
		"a positional":         {"events", "platform.item.added", "--url", srv.URL},
		"limit zero":           {"events", "--url", srv.URL, "--limit", "0"},
		"limit beyond the tap": {"events", "--url", srv.URL, "--limit", "501"},
		"payload and json":     {"events", "--url", srv.URL, "--payload", "--json"},
		"no url":               {"events"},
	})
	p.token = "admin-bearer"
	if code, _, errs := run(t, "events", "--url", srv.URL); code != exitcode.Forbidden || !strings.Contains(errs, "supply a bearer via "+instance.TokenEnv) {
		t.Errorf("without a bearer: want 5, got %d %q", code, errs)
	}
	if code, _, errs := run(t, "events", "--url", "http://127.0.0.1:1"); code != exitcode.Undetermined || !strings.Contains(errs, "not concluding") {
		t.Errorf("unreachable: want 4, got %d %q", code, errs)
	}
}
