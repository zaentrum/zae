package addon

import (
	"os"
	"syscall"
)

// enableEchoInput is the console mode bit that echoes typed characters.
const enableEchoInput = 0x0004

var setConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

// terminal reports whether f is a console: only a console has a console mode.
func terminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}

// withoutEcho turns off echo on the console f and returns what turns it back on.
func withoutEcho(f *os.File) (func(), error) {
	h := syscall.Handle(f.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return nil, err
	}
	if ok, _, err := setConsoleMode.Call(uintptr(h), uintptr(mode&^enableEchoInput)); ok == 0 {
		return nil, err
	}
	return func() { _, _, _ = setConsoleMode.Call(uintptr(h), uintptr(mode)) }, nil
}
