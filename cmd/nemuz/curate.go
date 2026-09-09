package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/curator"
	"github.com/kansaok/nemuz/internal/skill"
	"github.com/spf13/cobra"
)

func curateCmd() *cobra.Command {
	var (
		workspace  string
		pluginCmds []string
		allowNet   []string
		allowExec  []string
		sandbox    string
		dryRun     bool
		force      bool
		pause      bool
		resume     bool
	)

	c := &cobra.Command{
		Use:   "curate",
		Short: "Re-verify, retire, and tidy the agent's own skills",
		Long: "Runs the three checks that need no model:\n\n" +
			"  · Active skills are run against their own scenarios again. A skill\n" +
			"    that passed once is not proven forever — tools change, plugins\n" +
			"    change, nemuz changes. One that no longer passes is returned to\n" +
			"    quarantine. Because scenarios replay recordings, this is free.\n" +
			"  · Skills nobody has used for a long time are archived.\n" +
			"  · Drafts that sat in quarantine without ever being certified are\n" +
			"    archived, so quarantine does not become a junk drawer.\n\n" +
			"Skills a person wrote, and pinned skills, are never touched. Nothing\n" +
			"is deleted — archiving is reversible.\n\n" +
			"Re-verification needs the same tools the skills depend on, so pass the\n" +
			"same --plugin flags you would pass to `nemuz run`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if pause || resume {
				return setCuratorPaused(cmd, pause)
			}

			if paths, perr := config.Resolve(); perr == nil {
				settings := config.LoadSettingsQuiet(paths.Config)
				applyStringDefault(cmd, "sandbox", &sandbox, "sandbox", settings)
				applyListDefault(cmd, "allow-exec", &allowExec, "allow-exec", settings)
				applyListDefault(cmd, "allow-net", &allowNet, "allow-net", settings)
			}

			gate, cleanup, err := buildGate(cmd.Context(), gateOptions{
				workspace:  workspace,
				pluginCmds: pluginCmds,
				allowNet:   allowNet,
				allowExec:  allowExec,
				sandbox:    sandbox,
			})
			if err != nil {
				return err
			}
			defer cleanup()

			c, err := newCurator(gate, dryRun, force)
			if err != nil {
				return err
			}
			report, err := c.Run(cmd.Context())
			if err != nil {
				return err
			}
			printCurationReport(cmd.OutOrStdout(), report)
			return nil
		},
	}

	c.Flags().StringVarP(&workspace, "workspace", "w", ".", "workspace the scenarios run against")
	c.Flags().StringArrayVar(&pluginCmds, "plugin", nil, "plugin the skills depend on; repeatable")
	c.Flags().StringArrayVar(&allowNet, "allow-net", nil, "network destination a plugin may reach; repeatable")
	c.Flags().StringArrayVar(&allowExec, "allow-exec", nil, "program a plugin may run; repeatable")
	c.Flags().StringVar(&sandbox, "sandbox", string(SandboxAuto), "confine the built-in tools: on, auto, or off")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "say what would change without changing it")
	c.Flags().BoolVar(&force, "force", false, "run even if the curator ran recently")
	c.Flags().BoolVar(&pause, "pause", false, "stop automatic curation")
	c.Flags().BoolVar(&resume, "resume", false, "allow automatic curation again")
	return c
}

// newCurator assembles a curator over the state directory.
func newCurator(gate *skill.Gate, dryRun, force bool) (*curator.Curator, error) {
	paths, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	skills, err := skill.Open(paths.Skills)
	if err != nil {
		return nil, err
	}
	state, err := curator.OpenState(filepath.Join(paths.Root, "curator.json"))
	if err != nil {
		return nil, err
	}

	c := &curator.Curator{Skills: skills, Gate: gate, State: state, DryRun: dryRun}
	if force {
		// Forcing bypasses the interval, not the pause: pausing is a decision
		// someone made, and a flag should not quietly overrule it.
		c.State = nil
		if paused, err := curatorPaused(state); err != nil {
			return nil, err
		} else if paused {
			return nil, fmt.Errorf("curation is paused; resume it with: nemuz skill curate --resume")
		}
	}
	return c, nil
}

func curatorPaused(state *curator.State) (bool, error) {
	_, paused, err := state.Read()
	return paused, err
}

func setCuratorPaused(cmd *cobra.Command, pause bool) error {
	paths, err := config.Resolve()
	if err != nil {
		return err
	}
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	state, err := curator.OpenState(filepath.Join(paths.Root, "curator.json"))
	if err != nil {
		return err
	}
	if err := state.SetPaused(pause); err != nil {
		return err
	}
	if pause {
		fmt.Fprintln(cmd.OutOrStdout(), "automatic curation paused — resume with: nemuz skill curate --resume")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "automatic curation resumed")
	}
	return nil
}

func printCurationReport(out io.Writer, report curator.Report) {
	fmt.Fprintf(out, "%s\n", report.Summary())
	if len(report.Actions) == 0 {
		return
	}

	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, a := range report.Actions {
		marker := "  ok  "
		switch a.Action {
		case curator.ActionArchived, curator.ActionDemoted:
			marker = " " + a.Action + " "
		case curator.ActionFailed:
			marker = " FAIL "
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", marker, a.Skill, a.Reason)
	}
	_ = tw.Flush()

	if report.DryRun && report.Changed() > 0 {
		fmt.Fprintf(out, "\nNothing was changed. Run without --dry-run to apply.\n")
	}
}

// curateIfDue runs the curator after a turn, when it is due.
//
// The curator is meant to run while the agent is idle rather than from a
// daemon, so this is where "idle" happens: the turn is finished and the answer
// is already printed. A failure here is reported and dropped, because the user
// asked for a turn, not for maintenance.
func curateIfDue(cmd *cobra.Command, ts *toolset) {
	paths, err := config.Resolve()
	if err != nil {
		return
	}
	skills, err := skill.Open(paths.Skills)
	if err != nil {
		return
	}
	blobs, err := blob.Open(paths.Blobs)
	if err != nil {
		return
	}
	state, err := curator.OpenState(filepath.Join(paths.Root, "curator.json"))
	if err != nil {
		return
	}

	runner := &agent.ScenarioRunner{
		Tools:      ts.Registry,
		Blobs:      blobs,
		JournalDir: filepath.Join(paths.Root, "eval-journals"),
		BaseSystem: defaultSystemPrompt,
	}
	c := &curator.Curator{
		Skills: skills,
		Gate:   &skill.Gate{Store: skills, Run: runner.Run},
		State:  state,
	}

	due, err := c.Due()
	if err != nil || !due {
		return
	}
	report, err := c.Run(context.Background())
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "\ncuration failed: %v\n", err)
		return
	}
	if report.Changed() == 0 {
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\ncuration: %s\n", report.Summary())
	for _, a := range report.Actions {
		if a.Action == curator.ActionKept {
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  %s %s — %s\n", a.Action, a.Skill, a.Reason)
	}
}
