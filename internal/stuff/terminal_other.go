//go:build !linux

package stuff

import "io"

// TTY inspection is platform-specific. Unknown readers are left alone rather
// than risking rejection of a legal embedded/scripted CLI invocation.
func stdinIsNonTTY(io.Reader) bool { return false }
