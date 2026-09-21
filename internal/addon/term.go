package addon

import (
	"os"

	"github.com/zaentrum/zae/internal/term"
)

// The terminal itself lives in internal/term, so that every command group
// which asks a person something recognises a terminal the same way.

func terminal(f *os.File) bool { return term.Is(f) }

func withoutEcho(f *os.File) (func(), error) { return term.WithoutEcho(f) }
