// Command zae is the zaentrum CLI.
//
// The design splits the binary in two, and the split is the point:
//
//   - a STATIC core — the commands that must work when the platform cannot
//     speak for itself: preflight and outside-in diagnosis (doctor), signing
//     in (login), adding addons from a chart (addon). These ship in the binary
//     because a service can only extend the CLI once it is running, and the
//     moments you need doctor most are the moments nothing is. An addon
//     cannot declare the command that installs it.
//   - a DISCOVERED surface — every service and addon will declare commands,
//     checks and topics in a capability descriptor; the instance aggregates
//     them and zae renders them at runtime. Installing an addon extends the
//     CLI; removing it leaves no trace. Descriptors are data, never code: the
//     worst a descriptor can do is describe an HTTP call zae then makes with
//     the user's own token against the platform's own APIs.
//
// zae deliberately uses only the standard library. A handful of commands do
// not need a framework, and the discovered surface will not either — it
// renders from data.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/zaentrum/zae/internal/addon"
	"github.com/zaentrum/zae/internal/auth"
	"github.com/zaentrum/zae/internal/doctor"
	"github.com/zaentrum/zae/internal/exitcode"
	"github.com/zaentrum/zae/internal/invoke"
	"github.com/zaentrum/zae/internal/platform"
)

// version is stamped by the release build (-ldflags "-X main.version=v…").
var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `zae — the zaentrum CLI

Usage:
  zae login --url https://your-instance.example    sign in (device flow, opens a browser)
  zae logout --url https://… | --all               forget a stored session
  zae whoami --url https://…                       who zae's bearer says you are
  zae doctor --url https://your-instance.example   outside-in health of an instance
  zae discover --url https://…                     show the instance's capability surface
  zae require <service>[.<command>] --url https://… assert the instance offers it (exit 0/3/4/6, silent)
  zae addon add <chart> --url https://… [flags]    plan an addon from a Helm chart, confirm, install
  zae addon list|status|upgrade|remove …           manage addons installed from charts ('zae addon help')
  zae platform status --url https://…              the platform's version, update and workloads
  zae platform update|restart|scale …              drive platform updates ('zae platform help')
  zae <service> <command> --url https://… [--arg k=v] [--query k=v] [--data JSON]
  zae version

Exit codes (stable, for scripts): 0 ran · 1 the instance returned an error ·
2 usage · 3 not offered by this instance · 4 undetermined (could not find out —
do NOT treat as removed) · 5 forbidden · 6 capability schema newer than zae.

The command surface grows at runtime: services and addons on the instance you
point zae at register their own commands and checks. 'zae discover' shows what
this instance offers; nothing addon-specific is compiled into this binary.

Credentials: 'zae login' stores a session per instance under ~/.config/zae;
ZAE_TOKEN, when set, is sent instead — for service accounts and CI.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("zae", version)
	case "login":
		// Static, like doctor: signing in is what makes everything else
		// possible, so it cannot be a command an instance declares.
		os.Exit(auth.Login(os.Args[2:]))
	case "logout":
		os.Exit(auth.Logout(os.Args[2:]))
	case "whoami":
		os.Exit(auth.Whoami(os.Args[2:]))
	case "doctor":
		os.Exit(doctor.Run(os.Args[2:], version))
	case "discover":
		os.Exit(doctor.Discover(os.Args[2:]))
	case "require":
		os.Exit(invoke.Require(os.Args[2:]))
	case "addon":
		// Static, like doctor: installing is how an addon reaches the
		// instance, so no addon can declare it. `addon` is therefore not
		// available as a discovered service name.
		os.Exit(addon.Run(os.Args[2:]))
	case "platform":
		// Static for the same reason: the platform's own version is what
		// every declared command depends on, so it cannot be declared by one.
		os.Exit(platform.Run(os.Args[2:]))
	case "help", "--help", "-h":
		usage()
	default:
		// Anything else is `zae <service> <command>`: resolved against the
		// instance, never against this binary. A miss is reported by the
		// instance's answer (exit 3/4), not as "unknown command" — that
		// wording, and a usage dump, belong to typos in the static surface.
		if len(os.Args) < 3 || strings.HasPrefix(os.Args[2], "-") {
			fmt.Fprintf(os.Stderr, "zae: usage: %q is not a built-in command; instance commands are `zae <service> <command> --url …`\n", os.Args[1])
			os.Exit(exitcode.Usage)
		}
		os.Exit(invoke.Run(os.Args[1], os.Args[2], os.Args[3:]))
	}
}
