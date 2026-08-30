//go:build linux

package stuff

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

// stdinIsNonTTY deliberately only makes a decision for an *os.File. This
// keeps embedded callers and tests using arbitrary readers from being treated
// as redirected stdin when the terminal cannot be inspected.
func stdinIsNonTTY(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)))
	return errno != 0
}
