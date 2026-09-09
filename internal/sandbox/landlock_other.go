//go:build !linux

package sandbox

import "errors"

// ErrUnsupported means this platform has no Landlock.
var ErrUnsupported = errors.New("sandbox: landlock is a Linux facility")

// Rules is the set of directories a confined process may touch.
type Rules struct {
	Read    []string
	Write   []string
	Execute []string
}

// The path sets below exist so callers can be written once and compile
// everywhere. They are empty here rather than absent: a build that only
// compiles on Linux is a portability break that shows up first in a release,
// which is the worst place to find one.
var (
	// SystemReadDirs are directories a program reads without running anything
	// from them.
	SystemReadDirs []string
	// SystemExecDirs get read and execute when commands are allowed.
	SystemExecDirs []string
	// SystemDevices are the device files ordinary programs expect.
	SystemDevices []string
	// ResolverFiles are the files a program reads to look up a hostname.
	ResolverFiles []string
)

// ResolvedPaths follows symlinks and keeps the targets that exist. Off Linux
// nothing is confined, so there is nothing to resolve.
func ResolvedPaths([]string) []string { return nil }

// LandlockABI always fails off Linux. Sandboxing on macOS and Windows will need
// their own backends; until those exist, callers must decide whether to run
// tools unconfined or refuse.
func LandlockABI() (int, error) { return 0, ErrUnsupported }

// Available reports whether tool sandboxing can be enforced here.
func Available() bool { return false }

// Restrict always fails off Linux, so a caller cannot mistake an unconfined
// process for a confined one.
func Restrict(Rules) error { return ErrUnsupported }
