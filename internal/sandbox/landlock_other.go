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

// LandlockABI always fails off Linux. Sandboxing on macOS and Windows will need
// their own backends; until those exist, callers must decide whether to run
// tools unconfined or refuse.
func LandlockABI() (int, error) { return 0, ErrUnsupported }

// Available reports whether tool sandboxing can be enforced here.
func Available() bool { return false }

// Restrict always fails off Linux, so a caller cannot mistake an unconfined
// process for a confined one.
func Restrict(Rules) error { return ErrUnsupported }
