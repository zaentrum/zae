package doctor

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// discoveryPath is where an instance aggregates every service's and addon's
// capability descriptor. This is the seam that makes zae dynamic: the binary
// compiles in NO service names — what zae can do against an instance is
// whatever that instance declares here.
const discoveryPath = "/api/portal/cli/discovery"

// Descriptor is capability schema v1 — deliberately boring. Commands map
// declaratively to HTTP calls; checks are either run server-side and reported,
// or simple expectations zae executes. Data, never code: nothing an instance
// serves can execute in the operator's terminal.
type Descriptor struct {
	Service  string `json:"service"`
	Kind     string `json:"kind"` // platform | addon
	Version  string `json:"version"`
	Commands []struct {
		Name    string `json:"name"` // rendered as: zae <service> <name>
		Summary string `json:"summary"`
		Method  string `json:"method"`
		Path    string `json:"path"`
		Role    string `json:"role,omitempty"`
	} `json:"commands"`
	Checks []struct {
		Name string `json:"name"`
		Path string `json:"path"` // GET; returns structured check results
	} `json:"checks"`
	Topics []string `json:"topics"`
}

type discovery struct {
	CapabilityVersion int          `json:"capabilityVersion"`
	Services          []Descriptor `json:"services"`
}

// checkDiscovery reports whether the dynamic surface exists yet. Its absence
// is expected on today's platform and is reported as the roadmap item it is —
// doctor describing the dynamic system's own absence honestly.
func checkDiscovery(base fmt.Stringer) Result {
	resp, err := client().Get(strings.TrimRight(base.String(), "/") + discoveryPath)
	if err != nil {
		return Result{Name: "capability discovery", Status: Skip, Detail: "unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		var d discovery
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			return Result{Name: "capability discovery", Status: Warn, Detail: "endpoint answered but is not valid JSON"}
		}
		return Result{Name: "capability discovery", Status: OK,
			Detail: fmt.Sprintf("%d service(s) declare capabilities (schema v%d)", len(d.Services), d.CapabilityVersion)}
	case 404, 401:
		return Result{Name: "capability discovery", Status: Skip,
			Detail: "not implemented by this instance yet — zae runs with its static core only"}
	default:
		return Result{Name: "capability discovery", Status: Warn,
			Detail: fmt.Sprintf("answered %d", resp.StatusCode)}
	}
}

// Discover prints the instance's declared capability surface.
func Discover(args []string) int {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	rawURL := fs.String("url", "", "public URL of the instance (required)")
	_ = fs.Parse(args)
	if *rawURL == "" {
		fmt.Fprintln(os.Stderr, "discover: --url is required")
		return 2
	}
	resp, err := client().Get(strings.TrimRight(*rawURL, "/") + discoveryPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "discover:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Printf("this instance does not implement capability discovery yet (%d from %s)\n", resp.StatusCode, discoveryPath)
		fmt.Println("zae's static core (doctor) still works against it.")
		return 0
	}
	var d discovery
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		fmt.Fprintln(os.Stderr, "discover: invalid discovery document:", err)
		return 1
	}
	fmt.Printf("capability schema v%d · %d service(s)\n\n", d.CapabilityVersion, len(d.Services))
	for _, s := range d.Services {
		// Version is optional in the schema; do not print the gap it leaves.
		if s.Version != "" {
			fmt.Printf("%s (%s %s)\n", s.Service, s.Kind, s.Version)
		} else {
			fmt.Printf("%s (%s)\n", s.Service, s.Kind)
		}
		for _, c := range s.Commands {
			fmt.Printf("  zae %s %-18s %s\n", s.Service, c.Name, c.Summary)
		}
		for _, c := range s.Checks {
			fmt.Printf("  check: %s\n", c.Name)
		}
		if len(s.Topics) > 0 {
			fmt.Printf("  topics: %s\n", strings.Join(s.Topics, ", "))
		}
		fmt.Println()
	}
	return 0
}
