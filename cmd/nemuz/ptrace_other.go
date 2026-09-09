//go:build !linux

package main

import "errors"

// attemptPtrace has no meaning off Linux, where neither seccomp nor this
// probe apply.
func attemptPtrace() error {
	return errors.New("seccomp self-check is only meaningful on linux")
}
