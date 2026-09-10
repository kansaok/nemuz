// Package cli is the handful of interactive terminal building blocks `nemuz
// config` is built on — a prompt, a numbered picker, and a no-echo secret
// read. Everything takes an explicit io.Reader/io.Writer pair so the wizard
// can be driven by a script or a test the same way it runs for a human.
package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"
)

// IO is the terminal stream pair. Injected, never global, so tests can feed a
// script and capture what would have been printed. The buffered reader is
// carried with the pair so lines keep coming from the same stream.
type IO struct {
	In  io.Reader
	Out io.Writer
	rd  *bufio.Reader
}

// Terminal is an IO bound to the process's real stdin/stdout.
func Terminal() IO { return IO{In: os.Stdin, Out: os.Stdout} }

func (io *IO) reader() *bufio.Reader {
	if io.rd == nil {
		io.rd = bufio.NewReader(io.In)
	}
	return io.rd
}

// Interactive reports whether In is a terminal. The wizard runs only when it
// is; a piped stdin instead falls back to the non-interactive behavior, so a
// script never blocks waiting for a human.
func (io *IO) Interactive() bool {
	f, ok := io.In.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

// Prompt prints question and reads one trimmed line. An empty answer is kept —
// callers decide what empty means — and end-of-input is reported as io.EOF.
func (io *IO) Prompt(question string) (string, error) {
	fmt.Fprint(io.Out, question+" ")
	line, err := io.reader().ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(strings.TrimRight(line, "\r\n")), nil
}

// Pick prints question, then the numbered options, and reads a choice number.
// The returned index is into options. An option's extra lines (after a
// newline) are printed indented under its numbered first line.
func (io *IO) Pick(question string, options []string) (int, error) {
	fmt.Fprintln(io.Out, question)
	for i, opt := range options {
		lines := strings.Split(opt, "\n")
		fmt.Fprintf(io.Out, "  %2d. %s\n", i+1, lines[0])
		for _, extra := range lines[1:] {
			fmt.Fprintf(io.Out, "      %s\n", extra)
		}
	}
	for {
		choice, err := io.Prompt("choose (1-" + strconv.Itoa(len(options)) + "):")
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(choice)
		if err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		fmt.Fprintf(io.Out, "  a number between 1 and %d, please\n", len(options))
	}
}

// ReadSecret is Prompt for a value that should not appear on screen. On a real
// terminal the terminal's echo is turned off for the line; on a pipe it falls
// back to a plain read, which is fine for scripts and tests.
func (io *IO) ReadSecret(question string) (string, error) {
	f, ok := io.In.(*os.File)
	if ok && isatty.IsTerminal(f.Fd()) {
		return readSecretLine(io, f, question)
	}
	return io.Prompt(question)
}
