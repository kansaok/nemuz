// Command nemuz runs the agent gateway and the tools that inspect its journal.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is stamped at build time via -ldflags. The zero value marks a build
// made outside the release pipeline.
var (
	Version = "dev"
	Commit  = "none"
)

func main() {
	root := &cobra.Command{
		Use:   "nemuz",
		Short: "An agent framework whose every turn can be replayed",
		Long: "nemuz records everything an agent does to an append-only journal,\n" +
			"then replays it exactly. Bugs become reproducible, regressions become\n" +
			"testable without API calls, and skills the agent teaches itself can be\n" +
			"proven before they are trusted.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(versionCmd(), doctorCmd(), journalCmd(), replayCmd(), runCmd(), chatCmd(), providersCmd(), pluginCmd(), skillCmd(), memoryCmd(), serveCmd(), acpCmd(), searchCmd(), usageCmd(), indexCmd(), toolWorkerCmd(), configCmd(), channelCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "nemuz:", err)
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "nemuz %s (%s)\n", Version, Commit)
			return nil
		},
	}
}
