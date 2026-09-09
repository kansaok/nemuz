package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/sandbox"
	"github.com/spf13/cobra"
)

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine can run nemuz safely",
		Long: "Reports what nemuz found on this machine. The sandbox line matters\n" +
			"most: without kernel enforcement, tools run unconfined.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.OutOrStdout())
		},
	}
}

// check is one line of doctor output.
type check struct {
	name   string
	status string // ok, warn, or fail
	detail string
}

func runDoctor(out io.Writer) error {
	var checks []check

	checks = append(checks, check{"platform", "ok", fmt.Sprintf("%s/%s, %s", runtime.GOOS, runtime.GOARCH, runtime.Version())})

	// Sandboxing is the one check whose failure changes what nemuz will do,
	// so it reports the ABI version rather than a bare yes.
	abi, abiErr := sandbox.LandlockABI()
	if abiErr == nil {
		checks = append(checks, check{"kernel", "ok", fmt.Sprintf("Landlock ABI v%d available", abi)})
	} else {
		checks = append(checks, check{"kernel", "warn", fmt.Sprintf("%v — tools will run unconfined", abiErr)})
	}

	// Support is not the same as enforcement. This actually spawns the tool
	// worker and has it try to read a file outside its workspace, because a
	// build that computes the right grants and never applies them would pass
	// every check above while confining nothing.
	if abiErr == nil {
		if err := probeConfinement(); err != nil {
			checks = append(checks, check{"sandbox", "fail", err.Error()})
		} else {
			checks = append(checks, check{"sandbox", "ok", "verified — the tool worker cannot read outside its workspace"})
		}

		// A separate layer, checked separately: Landlock says nothing about
		// which syscalls are callable at all, so its own success proves
		// nothing about whether the seccomp filter installed correctly.
		if err := probeSeccomp(); err != nil {
			checks = append(checks, check{"seccomp", "warn", err.Error()})
		} else {
			checks = append(checks, check{"seccomp", "ok", "verified — ptrace and similar syscalls are refused by the kernel"})
		}
	}

	paths, err := config.Resolve()
	if err != nil {
		checks = append(checks, check{"state dir", "fail", err.Error()})
	} else {
		st, statErr := os.Stat(paths.Root)
		switch {
		case os.IsNotExist(statErr):
			checks = append(checks, check{"state dir", "warn", paths.Root + " — not created yet"})
		case statErr != nil:
			checks = append(checks, check{"state dir", "fail", statErr.Error()})
		case st.Mode().Perm() != 0o700:
			checks = append(checks, check{"state dir", "warn", fmt.Sprintf("%s is mode %o, want 0700 — it holds credentials", paths.Root, st.Mode().Perm())})
		default:
			checks = append(checks, check{"state dir", "ok", paths.Root})
		}
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	worst := "ok"
	for _, c := range checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", symbol(c.status), c.name, c.detail)
		if c.status == "fail" || (c.status == "warn" && worst == "ok") {
			worst = c.status
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if worst == "fail" {
		return fmt.Errorf("doctor found a blocking problem")
	}
	return nil
}

func symbol(status string) string {
	switch status {
	case "ok":
		return "  ok  "
	case "warn":
		return " warn "
	default:
		return " FAIL "
	}
}

// probeSeccomp spawns the sandboxed tool worker and confirms the kernel
// refuses ptrace — a syscall with no legitimate use in a tool call, and one
// that succeeds by default, which is what makes its refusal meaningful.
//
// This is reported as a warning rather than a failure when it does not hold:
// seccomp is defense in depth layered under Landlock, and an old kernel
// without CONFIG_SECCOMP_FILTER should not read as "the sandbox is broken" the
// way a Landlock leak would.
func probeSeccomp() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not locate nemuz: %w", err)
	}
	dir, err := os.MkdirTemp("", "nemuz-probe-*")
	if err != nil {
		return fmt.Errorf("could not create a probe workspace: %w", err)
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "tool-worker", "--workspace", dir, "--self-check-syscall")
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("the seccomp probe did not run: %w", err)
	}

	var result selfCheckResult
	if err := json.Unmarshal(out, &result); err != nil {
		return fmt.Errorf("the seccomp probe returned %q", strings.TrimSpace(string(out)))
	}
	if !result.Denied {
		return fmt.Errorf("ptrace was not refused (%s) — this kernel may lack CONFIG_SECCOMP_FILTER", result.Error)
	}
	return nil
}

// probeConfinement spawns the sandboxed tool worker and confirms the kernel
// refuses it a file outside its workspace.
func probeConfinement() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not locate nemuz: %w", err)
	}
	dir, err := os.MkdirTemp("", "nemuz-probe-*")
	if err != nil {
		return fmt.Errorf("could not create a probe workspace: %w", err)
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// /etc/hostname is world-readable and present everywhere Landlock is, so a
	// refusal can only come from the sandbox.
	const outside = "/etc/hostname"
	cmd := exec.CommandContext(ctx, self, "tool-worker", "--workspace", dir, "--self-check", outside)
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("the confinement probe did not run: %w", err)
	}

	var result selfCheckResult
	if err := json.Unmarshal(out, &result); err != nil {
		return fmt.Errorf("the confinement probe returned %q", strings.TrimSpace(string(out)))
	}
	if !result.Denied {
		if result.Error != "" {
			return fmt.Errorf("the worker was not confined: reading %s failed with %q, which is not a permission denial", outside, result.Error)
		}
		return fmt.Errorf("NOT CONFINED — the worker read %s despite the sandbox", outside)
	}
	return nil
}
