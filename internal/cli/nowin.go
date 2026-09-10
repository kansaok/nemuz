//go:build !unix

package cli

import (
	"os"
)

// readSecretLine falls back to a plain read on platforms without termios
// (windows). The value still works; it is simply echoed by the terminal.
func readSecretLine(io *IO, f *os.File, question string) (string, error) {
	return io.Prompt(question)
}
