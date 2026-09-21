// Package capability is zae's client for an instance's declared surface:
// fetching the discovery document, and answering "does this instance offer
// <service> <command>?" with an outcome a script can act on.
//
// The outcomes are deliberately three-valued. "Not offered" and "could not
// find out" are different facts with opposite consequences for a script, and
// collapsing them is the one mistake this package exists to make impossible.
package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Path is the aggregation endpoint every instance serves once its portal-api
// supports capability discovery. The binary compiles in no service names —
// what zae can do against an instance is exactly what this returns.
const Path = "/api/portal/cli/discovery"

// SchemaVersion is the capability schema major this binary understands. An
// instance advertising a higher major is a contract mismatch, not a guess.
const SchemaVersion = 1

type Command struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Role    string `json:"role,omitempty"`
}

type Check struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type Descriptor struct {
	Service string `json:"service"`
	Kind    string `json:"kind"`
	Version string `json:"version"`
	// ProxyKey is the app-registry key the portal proxies this service under
	// (/api/portal/apps/<key>/…). Optional; defaults to the service name.
	ProxyKey string    `json:"proxyKey,omitempty"`
	Commands []Command `json:"commands"`
	Checks   []Check   `json:"checks"`
	Topics   []string  `json:"topics"`
}

// Auth is how a CLI signs in to this instance. zae cannot guess either half:
// a shared realm registers per-instance clients, and the issuer may live
// under a path prefix on the instance's own origin — so the instance says
// both. Absent on an instance that predates the field, or that has nothing to
// sign in to; `zae login` then needs --issuer and --client-id.
type Auth struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientId"`
}

// Usable reports whether the instance said enough to attempt a login.
func (a *Auth) Usable() bool { return a != nil && a.Issuer != "" && a.ClientID != "" }

type Document struct {
	CapabilityVersion int          `json:"capabilityVersion"`
	Auth              *Auth        `json:"auth,omitempty"`
	Services          []Descriptor `json:"services"`
}

// Fetch errors are typed so callers can map them to exit codes without
// parsing strings.

// ErrUnsupported: the instance answered, but does not serve discovery — it
// predates the feature. zae cannot know what it offers.
var ErrUnsupported = errors.New("instance does not implement capability discovery")

// ErrUnreachable: no answer at all (connect, TLS, timeout). Also "cannot
// know" — and emphatically not "removed".
var ErrUnreachable = errors.New("capability discovery unreachable")

// ErrSchema: the instance speaks a newer schema major than this binary.
var ErrSchema = errors.New("capability schema newer than this zae")

// Fetch retrieves the discovery document from a public instance URL.
func Fetch(ctx context.Context, base string) (*Document, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+Path, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 200:
	case resp.StatusCode == 404 || resp.StatusCode == 401:
		return nil, ErrUnsupported
	default:
		return nil, fmt.Errorf("%w: discovery answered %d", ErrUnreachable, resp.StatusCode)
	}
	var d Document
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("%w: invalid discovery document: %v", ErrUnreachable, err)
	}
	if d.CapabilityVersion > SchemaVersion {
		return &d, fmt.Errorf("%w: instance speaks v%d, this zae speaks v%d", ErrSchema, d.CapabilityVersion, SchemaVersion)
	}
	return &d, nil
}

// Lookup is the answer to "does this instance offer <service> <command>?".
type Lookup struct {
	Service *Descriptor // nil when the service is not declared
	Command *Command    // nil when the service is declared but not the command
}

// Find resolves a service/command pair against the document. Both nil means
// the service is absent; Service set and Command nil means the command is.
// Matching is exact — a CLI that fuzzy-matches "wantd" to "wanted" would run
// a command the script did not name.
func (d *Document) Find(service, command string) Lookup {
	for i := range d.Services {
		s := &d.Services[i]
		if s.Service != service {
			continue
		}
		for j := range s.Commands {
			if s.Commands[j].Name == command {
				return Lookup{Service: s, Command: &s.Commands[j]}
			}
		}
		return Lookup{Service: s}
	}
	return Lookup{}
}

// ServiceNames lists what the instance declares, sorted, for error messages
// that say what IS there rather than only what is not.
func (d *Document) ServiceNames() []string {
	out := make([]string, 0, len(d.Services))
	for _, s := range d.Services {
		out = append(out, s.Service)
	}
	sort.Strings(out)
	return out
}

// CommandNames lists a service's declared commands in declaration order —
// the order the service chose is usually the order that reads well.
func (s *Descriptor) CommandNames() []string {
	out := make([]string, 0, len(s.Commands))
	for _, c := range s.Commands {
		out = append(out, c.Name)
	}
	return out
}
