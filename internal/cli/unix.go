//go:build unix

package cli

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// readSecretLine reads one line with the terminal's echo turned off, so a
// token typed into `nemuz config` does not scroll past on the screen. The
// terminal stays line-buffered and only ECHO changes, then gets restored even
// if the read fails.
func readSecretLine(io *IO, f *os.File, question string) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return io.Prompt(question)
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return io.Prompt(question)
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, old)

	fmt.Fprint(io.Out, question+" ")
	line, err := io.reader().ReadString('\n')
	fmt.Fprintln(io.Out)
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(strings.TrimRight(line, "\r\n")), nil
}
