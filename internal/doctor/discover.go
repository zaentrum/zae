package doctor

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/zaentrum/zae/internal/capability"
)

// checkDiscovery reports whether the dynamic surface exists. Its absence on an
// older instance is reported as the roadmap item it is — doctor describing the
// dynamic system's own absence honestly.
func checkDiscovery(base fmt.Stringer) Result {
	doc, err := capability.Fetch(context.Background(), base.String())
	switch {
	case err == nil:
		return Result{Name: "capability discovery", Status: OK,
			Detail: fmt.Sprintf("%d service(s) declare capabilities (schema v%d)", len(doc.Services), doc.CapabilityVersion)}
	case errors.Is(err, capability.ErrSchema):
		return Result{Name: "capability discovery", Status: Warn, Detail: err.Error(), Fix: "upgrade zae"}
	case errors.Is(err, capability.ErrUnsupported):
		return Result{Name: "capability discovery", Status: Skip,
			Detail: "not implemented by this instance yet — zae runs with its static core only"}
	default:
		return Result{Name: "capability discovery", Status: Skip, Detail: err.Error()}
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
	doc, err := capability.Fetch(context.Background(), *rawURL)
	if errors.Is(err, capability.ErrUnsupported) {
		fmt.Println("this instance does not implement capability discovery yet")
		fmt.Println("zae's static core (doctor) still works against it.")
		return 0
	}
	if err != nil && doc == nil {
		fmt.Fprintln(os.Stderr, "discover:", err)
		return 1
	}
	if err != nil { // schema newer than us: show what we can, say so
		fmt.Fprintln(os.Stderr, "discover: warning:", err)
	}
	fmt.Printf("capability schema v%d · %d service(s)\n\n", doc.CapabilityVersion, len(doc.Services))
	for _, s := range doc.Services {
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
