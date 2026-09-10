// Command nemuz runs the agent gateway and the tools that inspect its journal.
package main

import (
	"fmt"
	"os"

	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/spf13/cobra"
)

// Version is stamped at build time via -ldflags. The zero value marks a build
// made outside the release pipeline.
var (
	Version = "dev"
	Commit  = "none"
)

func main() {
	loadStartupConfig()

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

// loadStartupConfig exports ~/.nemuz/.env into the environment and registers
// models.providers from config.json as provider catalogue entries, so every
// command sees the same configuration before it even parses its own flags.
// This is why main runs it rather than each command: a ${NAME} in config.json
// must already be resolvable wherever the env is checked.
func loadStartupConfig() {
	paths, err := config.Resolve()
	if err != nil {
		return
	}
	if err := config.LoadDotEnv(paths.Env); err != nil {
		fmt.Fprintf(os.Stderr, "nemuz: load %s: %v\n", paths.Env, err)
	}
	settings := config.LoadSettingsQuiet(paths.Config)
	if settings == nil {
		return
	}
	if err := provider.RegisterCustom(providersFromConfig(settings.Expand().Models)); err != nil {
		fmt.Fprintf(os.Stderr, "nemuz: config: %v\n", err)
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
