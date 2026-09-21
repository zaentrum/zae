//go:build !linux && !darwin && !windows

// Package term answers the two questions every command that asks a person
// something has to answer: is there a person on the other end of stdin, and
// can this terminal be told to stop echoing while a secret is typed.
package term

import (
	"errors"
	"os"
)

// Is falls back to recognising a character device that is not /dev/null,
// where zae has no terminal ioctl to ask.
func Is(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}

// WithoutEcho is not available here; a secret input comes from a file instead.
func WithoutEcho(*os.File) (func(), error) {
	return nil, errors.New("zae cannot turn off terminal echo on this system")
}
