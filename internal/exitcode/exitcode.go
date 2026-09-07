// Package exitcode is zae's exit-status contract. Scripts key off these
// numbers, not off message text, so they are stable and published (README,
// and the platform's CLI contract page).
//
// The dynamic design creates a failure mode static CLIs never had: a command
// can vanish between two runs of the same script because the INSTANCE
// changed, not the binary. So the contract must let a script tell three
// different things apart — "I typed nonsense", "this instance definitively
// does not offer that", and "I could not find out" — because the last two
// demand opposite reactions. A network blip that reads as "removed" is how a
// script deletes something it should not.
package exitcode

const (
	// OK: the command ran and the instance answered success.
	OK = 0
	// Failed: the command ran and the instance returned an error.
	Failed = 1
	// Usage: the invocation itself is malformed — unknown static command,
	// missing --url, a placeholder not supplied. Fix the script.
	Usage = 2
	// NotOffered: discovery ANSWERED and this instance does not declare that
	// service or command. Definitive: the addon is absent or the command was
	// renamed. A script may branch on this.
	NotOffered = 3
	// Undetermined: discovery could not be reached, or the instance predates
	// capability discovery. zae could not find out; a script must NOT conclude
	// the command is gone.
	Undetermined = 4
	// Forbidden: the command is declared but the caller may not run it — not
	// authenticated, or lacking the role.
	Forbidden = 5
	// ContractMismatch: the instance speaks a newer capability schema than
	// this binary understands. Upgrade zae.
	ContractMismatch = 6
)
