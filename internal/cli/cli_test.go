package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func script(answers string) *IO {
	return &IO{In: strings.NewReader(answers), Out: &bytes.Buffer{}}
}

func TestPromptReadsTrimmedLine(t *testing.T) {
	io := script("  hello world  \n")
	got, err := io.Prompt("say:")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(io.Out.(*bytes.Buffer).String(), "say:") {
		t.Error("the question was not printed")
	}
}

func TestPromptsReadOneSustainedStream(t *testing.T) {
	// Multiple prompts and picks must keep reading from the same buffered
	// stream — a full script can arrive before any prompt starts.
	io := script("one\ntwo\n")
	a, err := io.Prompt("a:")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.Prompt("b:")
	if err != nil {
		t.Fatal(err)
	}
	if a != "one" || b != "two" {
		t.Errorf("got %q then %q", a, b)
	}
}

func TestPickReturnsIndex(t *testing.T) {
	io := script("2\n")
	got, err := io.Pick("Which?", []string{"first", "second", "third"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("index = %d, want 1", got)
	}
	out := io.Out.(*bytes.Buffer).String()
	for _, want := range []string{"1. first", "2. second", "3. third"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPickRepeatsOnGarbage(t *testing.T) {
	io := script("banana\n4\n99\n1\n")
	got, err := io.Pick("Which?", []string{"only"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("index = %d, want 0", got)
	}
	if strings.Count(io.Out.(*bytes.Buffer).String(), "a number between") < 3 {
		t.Error("invalid answers were not re-prompted")
	}
}

func TestPickEndOfInput(t *testing.T) {
	iox := &IO{In: strings.NewReader(""), Out: &bytes.Buffer{}}
	if _, err := iox.Pick("Which?", []string{"only"}); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestReadSecretOnAPipeIsAPlainRead(t *testing.T) {
	io := script("sk-secret\n")
	got, err := io.ReadSecret("key:")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-secret" {
		t.Errorf("got %q", got)
	}
}
