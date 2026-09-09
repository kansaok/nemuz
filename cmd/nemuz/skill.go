package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/skill"
	"github.com/spf13/cobra"
)

func skillCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "skill",
		Short: "Inspect and govern what the agent has learned",
		Long: "Skills the agent writes for itself start in quarantine and stay there\n" +
			"until they pass their own recorded scenarios. Because turns are\n" +
			"journaled, running those scenarios costs no model calls.\n\n" +
			"Nothing here deletes a skill. Archiving is the strongest removal, and\n" +
			"it can be undone.",
	}
	c.AddCommand(skillListCmd(), skillShowCmd(), skillEvalCmd(true), skillEvalCmd(false),
		skillArchiveCmd(), skillRestoreCmd(), skillPinCmd(true), skillPinCmd(false), curateCmd(), scenarioCmd(), consolidateCmd())
	return c
}

func openSkillStore() (*skill.Store, error) {
	paths, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	return skill.Open(paths.Skills)
}

func skillListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List every skill and its state",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openSkillStore()
			if err != nil {
				return err
			}
			skills, err := store.List()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(skills) == 0 {
				fmt.Fprintf(out, "No skills yet in %s\n", store.Root())
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SKILL\tSTATE\tBY\tUSES\tEVALS\tDESCRIPTION")
			var quarantined int
			for _, sk := range skills {
				scenarios, _ := store.Scenarios(sk.Name)
				marker := ""
				if sk.Pinned {
					marker = " (pinned)"
				}
				if sk.State == skill.StateQuarantine {
					quarantined++
				}
				fmt.Fprintf(tw, "%s\t%s%s\t%s\t%d\t%d\t%s\n",
					sk.Name, sk.State, marker, sk.CreatedBy, sk.UseCount, len(scenarios), sk.Description)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if quarantined > 0 {
				fmt.Fprintf(out, "\n%d skill(s) in quarantine. Run `nemuz skill certify <name>` to test and promote one.\n", quarantined)
			}
			return nil
		},
	}
}

func skillShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print a skill, its scenarios, and its evidence",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openSkillStore()
			if err != nil {
				return err
			}
			sk, err := store.Load(args[0])
			if err != nil {
				return err
			}
			scenarios, err := store.Scenarios(sk.Name)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s — %s\n\n", sk.Name, sk.Description)
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "state\t%s\n", sk.State)
			fmt.Fprintf(tw, "written by\t%s\n", sk.CreatedBy)
			fmt.Fprintf(tw, "created\t%s\n", sk.CreatedAt.Format(time.RFC3339))
			fmt.Fprintf(tw, "used\t%d times\n", sk.UseCount)
			if sk.PromotedBy != "" {
				fmt.Fprintf(tw, "promoted by\t%s\n", sk.PromotedBy)
			}
			if sk.Reason != "" {
				label := "archived because"
				if sk.State == skill.StateQuarantine {
					label = "sent back because"
				}
				fmt.Fprintf(tw, "%s\t%s\n", label, sk.Reason)
			}
			if sk.Pinned {
				fmt.Fprintf(tw, "pinned\tyes — exempt from automatic curation\n")
			}
			if err := tw.Flush(); err != nil {
				return err
			}

			fmt.Fprintf(out, "\nscenarios (%d)\n", len(scenarios))
			for _, sc := range scenarios {
				fmt.Fprintf(out, "  %s — %s\n", sc.Name, sc.Description)
				fmt.Fprintf(out, "    %s\n", describeExpectations(sc.Expect))
			}
			fmt.Fprintf(out, "\ninstructions\n%s\n", indent(sk.Body, "  "))
			return nil
		},
	}
}

func describeExpectations(e skill.Expectations) string {
	var parts []string
	if len(e.MustCall) > 0 {
		parts = append(parts, "must call "+strings.Join(e.MustCall, ", "))
	}
	if len(e.MustNotCall) > 0 {
		parts = append(parts, "must not call "+strings.Join(e.MustNotCall, ", "))
	}
	if len(e.MustContain) > 0 {
		parts = append(parts, "must mention "+strings.Join(e.MustContain, ", "))
	}
	if len(e.MustNotContain) > 0 {
		parts = append(parts, "must not mention "+strings.Join(e.MustNotContain, ", "))
	}
	if e.MaxSteps > 0 {
		parts = append(parts, fmt.Sprintf("at most %d steps", e.MaxSteps))
	}
	if e.MustSucceed {
		parts = append(parts, "must not error")
	}
	if len(parts) == 0 {
		return "no expectations — this scenario asserts nothing"
	}
	return strings.Join(parts, " · ")
}

func indent(text, prefix string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// skillEvalCmd builds either `eval` (report only) or `certify` (report, then
// promote if it passed). Keeping them one implementation means the report an
// operator reads is exactly the one promotion acts on.
func skillEvalCmd(promote bool) *cobra.Command {
	var workspace string
	var minScenarios int
	var pluginCmds []string
	var allowNet []string
	var allowExec []string
	var sandboxMode string

	use, short := "eval <name>", "Run a skill's scenarios and report, without promoting it"
	if promote {
		use, short = "certify <name>", "Run a skill's scenarios and promote it if they all pass"
	}

	c := &cobra.Command{
		Use:   use,
		Short: short,
		Long: "Scenarios replay recorded turns, so this makes no model calls and\n" +
			"needs no network. A skill that cannot prove itself stays in quarantine.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if paths, perr := config.Resolve(); perr == nil {
				settings := config.LoadSettingsQuiet(paths.Config)
				applyStringDefault(cmd, "sandbox", &sandboxMode, "sandbox", settings)
				applyListDefault(cmd, "allow-exec", &allowExec, "allow-exec", settings)
				applyListDefault(cmd, "allow-net", &allowNet, "allow-net", settings)
			}

			// Scenarios must run against the toolset the skill will actually
			// have. Certifying against a smaller one proves nothing: the tools
			// the skill depends on simply fail, which the gate now catches.
			gate, cleanup, err := buildGate(cmd.Context(), gateOptions{
				workspace:    workspace,
				minScenarios: minScenarios,
				pluginCmds:   pluginCmds,
				allowNet:     allowNet,
				allowExec:    allowExec,
				sandbox:      sandboxMode,
			})
			if err != nil {
				return err
			}
			defer cleanup()

			name := args[0]
			var report skill.Report
			var certifyErr error
			if promote {
				report, certifyErr = gate.Certify(cmd.Context(), name)
			} else {
				report, certifyErr = gate.Evaluate(cmd.Context(), name)
			}

			printReport(cmd.OutOrStdout(), name, report, promote)
			if len(report.Results) == 0 {
				paths, perr := config.Resolve()
				if perr == nil {
					turns, _ := journal.List(paths.Journal)
					if n := len(turns); n > 5 {
						turns = turns[n-5:]
					}
					fmt.Fprint(cmd.OutOrStdout(), suggestScenario(name, turns))
				}
			}
			if !promote {
				// `eval` reports; it does not fail the shell for a skill that
				// simply is not ready yet.
				if report.Passed() {
					return nil
				}
				return nil
			}
			return certifyErr
		},
	}
	c.Flags().StringVarP(&workspace, "workspace", "w", ".", "workspace the scenarios run against")
	c.Flags().IntVar(&minScenarios, "min-scenarios", 0, "scenarios a skill must carry to be promoted")
	c.Flags().StringArrayVar(&pluginCmds, "plugin", nil, "plugin the skill depends on; repeatable")
	c.Flags().StringArrayVar(&allowNet, "allow-net", nil, "network destination a plugin may reach; repeatable")
	c.Flags().StringArrayVar(&allowExec, "allow-exec", nil, "program a plugin may run; repeatable")
	c.Flags().StringVar(&sandboxMode, "sandbox", string(SandboxAuto), "confine the built-in tools: on, auto, or off")
	return c
}

// gateOptions is everything a certification run needs beyond the skill itself.
type gateOptions struct {
	workspace    string
	minScenarios int
	pluginCmds   []string
	allowNet     []string
	allowExec    []string
	sandbox      string
}

// buildGate assembles a gate and returns a cleanup for the plugins it started.
func buildGate(ctx context.Context, opts gateOptions) (*skill.Gate, func(), error) {
	noop := func() {}

	paths, err := config.Resolve()
	if err != nil {
		return nil, noop, err
	}
	if err := paths.EnsureDirs(); err != nil {
		return nil, noop, err
	}
	store, err := skill.Open(paths.Skills)
	if err != nil {
		return nil, noop, err
	}
	bs, err := blob.Open(paths.Blobs)
	if err != nil {
		return nil, noop, err
	}
	ts, err := buildToolset(ctx, toolsetOptions{
		Workspace:  opts.workspace,
		Sandbox:    SandboxMode(opts.sandbox),
		PluginCmds: opts.pluginCmds,
		AllowNet:   opts.allowNet,
		AllowExec:  opts.allowExec,
	})
	if err != nil {
		return nil, noop, err
	}

	runner := &agent.ScenarioRunner{
		Tools:      ts.Registry,
		Blobs:      bs,
		JournalDir: filepath.Join(paths.Root, "eval-journals"),
		BaseSystem: defaultSystemPrompt,
	}
	return &skill.Gate{Store: store, Run: runner.Run, MinScenarios: opts.minScenarios}, ts.Close, nil
}

func printReport(out io.Writer, name string, report skill.Report, promoted bool) {
	fmt.Fprintf(out, "%s — %s\n\n", name, report.Summary())
	for _, res := range report.Results {
		status := "  ok  "
		if !res.Passed {
			status = " FAIL "
		}
		fmt.Fprintf(out, "%s %s\n", status, res.Scenario)
		for _, f := range res.Failures {
			fmt.Fprintf(out, "         %s\n", f)
		}
		if res.TurnID != "" {
			fmt.Fprintf(out, "         turn %s — inspect with: nemuz journal show %s\n", res.TurnID, res.TurnID)
		}
	}
	if promoted && report.Passed() {
		fmt.Fprintf(out, "\npromoted to active, evidence %s\n", report.ID)
	}
}

func skillArchiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archive <name> <reason>",
		Short: "Retire a skill, recoverably",
		Long:  "Archiving is reversible. `nemuz skill restore` brings it back — into quarantine, so it must prove itself again.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openSkillStore()
			if err != nil {
				return err
			}
			if err := store.Archive(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "archived %s — restore with: nemuz skill restore %s\n", args[0], args[0])
			return nil
		},
	}
}

func skillRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <name>",
		Short: "Bring an archived skill back into quarantine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openSkillStore()
			if err != nil {
				return err
			}
			if err := store.Restore(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restored %s to quarantine — certify it with: nemuz skill certify %s\n", args[0], args[0])
			return nil
		},
	}
}

func skillPinCmd(pin bool) *cobra.Command {
	use, short := "unpin <name>", "Allow automatic curation to touch a skill again"
	if pin {
		use, short = "pin <name>", "Exempt a skill from automatic curation"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openSkillStore()
			if err != nil {
				return err
			}
			if err := store.Pin(args[0], pin); err != nil {
				return err
			}
			state := "unpinned"
			if pin {
				state = "pinned"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", state, args[0])
			return nil
		},
	}
}

// activeSkillPrompt renders the active skills for a turn's system prompt.
func activeSkillPrompt(base string) (string, []string, error) {
	store, err := openSkillStore()
	if err != nil {
		return base, nil, err
	}
	active, err := store.Active()
	if err != nil {
		return base, nil, err
	}
	if len(active) == 0 {
		return base, nil, nil
	}

	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n# Learned skills\n\n")
	names := make([]string, 0, len(active))
	for _, sk := range active {
		b.WriteString(sk.Prompt())
		b.WriteString("\n")
		names = append(names, sk.Name)
	}
	return b.String(), names, nil
}
