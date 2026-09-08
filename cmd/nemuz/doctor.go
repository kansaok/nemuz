package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"text/tabwriter"

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
	if abi, err := sandbox.LandlockABI(); err == nil {
		checks = append(checks, check{"sandbox", "ok", fmt.Sprintf("Landlock ABI v%d — tool confinement enforced by the kernel", abi)})
	} else {
		checks = append(checks, check{"sandbox", "warn", fmt.Sprintf("%v — tools would run unconfined", err)})
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
