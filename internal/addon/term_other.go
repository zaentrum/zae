//go:build !linux && !darwin && !windows

package addon

import (
	"errors"
	"os"
)

// terminal falls back to recognising a character device that is not
// /dev/null, where zae has no terminal ioctl to ask.
func terminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}

// withoutEcho is not available here; a secret input comes from a file instead.
func withoutEcho(*os.File) (func(), error) {
	return nil, errors.New("zae cannot turn off terminal echo on this system")
}
