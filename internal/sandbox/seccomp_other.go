//go:build !linux

package sandbox

import "errors"

// ErrSeccompUnsupported means this platform has no seccomp.
var ErrSeccompUnsupported = errors.New("sandbox: seccomp is a Linux facility")

// RestrictSyscalls always fails off Linux, so a caller cannot mistake an
// unfiltered process for a filtered one.
func RestrictSyscalls() error { return ErrSeccompUnsupported }
