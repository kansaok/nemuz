package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/spf13/cobra"
)

// newTestSession builds a turnSession over a scripted provider and an
// unsandboxed toolset, so the chat loop's own mechanics — reading lines,
// answering each as a turn, honouring /exit — can be exercised without a
// model, a key, or a kernel that supports Landlock.
func newTestSession(t *testing.T, responses []llm.Response) *turnSession {
	t.Helper()
	t.Setenv(config.EnvHome, t.TempDir())
	workspace := t.TempDir()

	paths, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	bs, err := blob.Open(paths.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := buildToolset(context.Background(), toolsetOptions{
		Workspace: workspace,
		Sandbox:   SandboxOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ts.Close)

	return &turnSession{
		paths:      paths,
		provider:   &llm.Static{Label: "test", Responses: responses},
		model:      "test-model",
		ts:         ts,
		bs:         bs,
		baseSystem: "you are a test assistant",
		maxSteps:   4,
		doReview:   false,
		doCurate:   false,
	}
}

func TestChatLoopAnswersEachLineAsATurn(t *testing.T) {
	sess := newTestSession(t, []llm.Response{
		{Text: "hi there", StopReason: llm.StopEnd},
		{Text: "still here", StopReason: llm.StopEnd},
	})

	in := strings.NewReader("halo\napa kabar\n/exit\n")
	var out strings.Builder
	cmd := &cobra.Command{}

	if err := runChatLoop(cmd, in, &out, sess, DefaultDelegateDepth); err != nil {
		t.Fatalf("runChatLoop: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "hi there") {
		t.Errorf("first answer missing from transcript:\n%s", got)
	}
	if !strings.Contains(got, "still here") {
		t.Errorf("second answer missing from transcript:\n%s", got)
	}
}

func TestChatLoopStopsOnQuit(t *testing.T) {
	sess := newTestSession(t, nil) // no scripted responses: a call would fail the test

	in := strings.NewReader("/quit\n")
	var out strings.Builder
	cmd := &cobra.Command{}

	if err := runChatLoop(cmd, in, &out, sess, DefaultDelegateDepth); err != nil {
		t.Fatalf("runChatLoop: %v", err)
	}
}

func TestChatLoopStopsOnEOFWithNoTrailingCommand(t *testing.T) {
	sess := newTestSession(t, []llm.Response{{Text: "ok", StopReason: llm.StopEnd}})

	in := strings.NewReader("halo\n") // no trailing /exit — input just ends
	var out strings.Builder
	cmd := &cobra.Command{}

	if err := runChatLoop(cmd, in, &out, sess, DefaultDelegateDepth); err != nil {
		t.Fatalf("runChatLoop: %v", err)
	}
	if !strings.Contains(out.String(), "ok") {
		t.Errorf("the one line before EOF should still have been answered:\n%s", out.String())
	}
}

func TestChatLoopSkipsBlankLines(t *testing.T) {
	// A blank line must not be sent to the model as an empty turn — the
	// script below has only one response, so a second Complete call (from a
	// wrongly-forwarded blank line) would exhaust it and fail the test.
	sess := newTestSession(t, []llm.Response{{Text: "answered", StopReason: llm.StopEnd}})

	in := strings.NewReader("\n   \nhalo\n/exit\n")
	var out strings.Builder
	cmd := &cobra.Command{}

	if err := runChatLoop(cmd, in, &out, sess, DefaultDelegateDepth); err != nil {
		t.Fatalf("runChatLoop: %v", err)
	}
	if !strings.Contains(out.String(), "answered") {
		t.Errorf("the real line should have been answered:\n%s", out.String())
	}
}
