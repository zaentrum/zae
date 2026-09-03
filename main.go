// Command zae is the zaentrum CLI.
//
// The design splits the binary in two, and the split is the point:
//
//   - a STATIC core — the commands that must work when the platform cannot
//     speak for itself: preflight and outside-in diagnosis (doctor), and soon
//     login. These ship in the binary because a service can only extend the
//     CLI once it is running, and the moments you need doctor most are the
//     moments nothing is.
//   - a DISCOVERED surface — every service and addon will declare commands,
//     checks and topics in a capability descriptor; the instance aggregates
//     them and zae renders them at runtime. Installing an addon extends the
//     CLI; removing it leaves no trace. Descriptors are data, never code: the
//     worst a descriptor can do is describe an HTTP call zae then makes with
//     the user's own token against the platform's own APIs.
//
// zae deliberately uses only the standard library. Three commands do not need
// a framework, and the discovered surface will not either — it renders from
// data.
package main

import (
	"fmt"
	"os"

	"github.com/zaentrum/zae/internal/doctor"
)

// version is stamped by the release build (-ldflags "-X main.version=v…").
var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `zae — the zaentrum CLI

Usage:
  zae doctor --url https://your-instance.example   outside-in health of an instance
  zae discover --url https://…                     show the instance's capability surface
  zae version

The command surface grows at runtime: services and addons on the instance you
point zae at register their own commands and checks. 'zae discover' shows what
this instance offers; nothing addon-specific is compiled into this binary.
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
	case "doctor":
		os.Exit(doctor.Run(os.Args[2:], version))
	case "discover":
		os.Exit(doctor.Discover(os.Args[2:]))
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "zae: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}
