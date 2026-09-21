//go:build linux || darwin

// Package term answers the two questions every command that asks a person
// something has to answer: is there a person on the other end of stdin, and
// can this terminal be told to stop echoing while a secret is typed.
//
// The standard library has no terminal package, and zae takes no dependency
// for one: a terminal is recognised, and its echo switched, with the two
// ioctls every terminal answers. ioctlGetTermios and ioctlSetTermios are per
// system.
package term

import (
	"os"
	"syscall"
	"unsafe"
)

func termios(f *os.File) (*syscall.Termios, error) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlGetTermios, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return nil, errno
	}
	return &t, nil
}

func setTermios(f *os.File, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlSetTermios, uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}

// Is reports whether f is a terminal: only a terminal has termios, so
// /dev/null and pipes are not mistaken for one.
func Is(f *os.File) bool {
	_, err := termios(f)
	return err == nil
}

// WithoutEcho turns off echo on the terminal f and returns what turns it back
// on. Line editing and Ctrl-C keep working.
func WithoutEcho(f *os.File) (func(), error) {
	old, err := termios(f)
	if err != nil {
		return nil, err
	}
	t := *old
	t.Lflag &^= syscall.ECHO
	if err := setTermios(f, &t); err != nil {
		return nil, err
	}
	return func() { _ = setTermios(f, old) }, nil
}
