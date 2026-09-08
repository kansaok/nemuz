package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/skill"
	"github.com/spf13/cobra"
)

func scenarioCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "scenario",
		Short: "Write the evidence a skill needs to leave quarantine",
		Long: "A skill the agent writes for itself is quarantined until it passes\n" +
			"scenarios. A scenario is a recorded turn plus what the skill must make\n" +
			"happen — which tools run, what the answer says, how long it takes.\n\n" +
			"Because scenarios replay recordings, running them costs no model calls.\n" +
			"That is what makes it reasonable to demand evidence every time.",
	}
	c.AddCommand(scenarioAddCmd(), scenarioListCmd(), scenarioRemoveCmd())
	return c
}

func scenarioAddCmd() *cobra.Command {
	var (
		name           string
		description    string
		turnRef        string
		prompt         string
		mustCall       []string
		mustNotCall    []string
		mustContain    []string
		mustNotContain []string
		maxSteps       int
		mustSucceed    bool
	)

	c := &cobra.Command{
		Use:   "add <skill>",
		Short: "Attach a scenario built from a recorded turn",
		Long: "Copies the recording in beside the scenario, so the two travel together\n" +
			"when the skill is shared or moved.\n\n" +
			"Pick a turn where the skill would have applied. `nemuz journal ls` and\n" +
			"`nemuz search` are the usual ways to find one.",
		Args: cobra.ExactArgs(1),
		Example: `  nemuz skill scenario add project-health-check \
    --turn 01M20SYV7B --must-call read_file --must-succeed`,
		RunE: func(cmd *cobra.Command, args []string) error {
			skillName := args[0]
			if turnRef == "" {
				return fmt.Errorf("--turn is required: a scenario is a recorded turn plus expectations")
			}

			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			store, err := skill.Open(paths.Skills)
			if err != nil {
				return err
			}
			if _, err := store.Load(skillName); err != nil {
				return err
			}

			turn, err := journal.Find(paths.Journal, turnRef)
			if err != nil {
				return err
			}

			expect := skill.Expectations{
				MustCall:       mustCall,
				MustNotCall:    mustNotCall,
				MustContain:    mustContain,
				MustNotContain: mustNotContain,
				MaxSteps:       maxSteps,
				MustSucceed:    mustSucceed,
			}
			// A scenario that asserts nothing passes always, which is worse
			// than no scenario at all: it makes the gate look satisfied.
			if isEmptyExpectation(expect) {
				return fmt.Errorf("this scenario asserts nothing, so it would pass whatever the skill does.\n" +
					"Give it at least one of --must-call, --must-not-call, --must-contain, --max-steps or --must-succeed")
			}

			sc := skill.Scenario{Name: name, Description: description, Prompt: prompt, Expect: expect}
			if err := store.AttachScenario(skillName, sc, turn.Path); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "attached scenario %q to %s, replaying turn %s\n", name, skillName, turn.ID)
			fmt.Fprintf(out, "  %s\n\n", describeExpectations(expect))
			fmt.Fprintf(out, "Try it: nemuz skill eval %s\n", skillName)
			return nil
		},
	}

	c.Flags().StringVar(&name, "name", "basic", "scenario name, used for its files")
	c.Flags().StringVar(&description, "description", "", "what this scenario checks")
	c.Flags().StringVar(&turnRef, "turn", "", "recorded turn to replay; an id or unambiguous prefix")
	c.Flags().StringVar(&prompt, "prompt", "", "override the recorded prompt")
	c.Flags().StringArrayVar(&mustCall, "must-call", nil, "tool the turn has to invoke; repeatable")
	c.Flags().StringArrayVar(&mustNotCall, "must-not-call", nil, "tool the turn must stay away from; repeatable")
	c.Flags().StringArrayVar(&mustContain, "must-contain", nil, "text the answer must include; repeatable")
	c.Flags().StringArrayVar(&mustNotContain, "must-not-contain", nil, "text the answer must avoid; repeatable")
	c.Flags().IntVar(&maxSteps, "max-steps", 0, "most model rounds the turn may take")
	c.Flags().BoolVar(&mustSucceed, "must-succeed", false, "require the turn to finish without errors")
	return c
}

func isEmptyExpectation(e skill.Expectations) bool {
	return len(e.MustCall) == 0 && len(e.MustNotCall) == 0 &&
		len(e.MustContain) == 0 && len(e.MustNotContain) == 0 &&
		e.MaxSteps == 0 && !e.MustSucceed
}

func scenarioListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls <skill>",
		Aliases: []string{"list"},
		Short:   "List a skill's scenarios",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openSkillStore()
			if err != nil {
				return err
			}
			scenarios, err := store.Scenarios(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(scenarios) == 0 {
				fmt.Fprintf(out, "%s has no scenarios, so it cannot leave quarantine.\n", args[0])
				fmt.Fprintf(out, "Add one: nemuz skill scenario add %s --turn <id> --must-call <tool>\n", args[0])
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SCENARIO\tEXPECTS")
			for _, sc := range scenarios {
				fmt.Fprintf(tw, "%s\t%s\n", sc.Name, describeExpectations(sc.Expect))
			}
			return tw.Flush()
		},
	}
}

func scenarioRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <skill> <scenario>",
		Aliases: []string{"remove"},
		Short:   "Delete a scenario and the recording it owned",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openSkillStore()
			if err != nil {
				return err
			}
			if err := store.RemoveScenario(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed scenario %q from %s\n", args[1], args[0])
			return nil
		},
	}
}

// suggestScenario is shown when a skill cannot be certified for want of one.
func suggestScenario(skillName string, recent []journal.Turn) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\nAdd one with:\n  nemuz skill scenario add %s --turn <id> --must-call <tool> --must-succeed\n", skillName))
	if len(recent) > 0 {
		b.WriteString("\nRecent turns to choose from:\n")
		for i, t := range recent {
			if i >= 5 {
				break
			}
			b.WriteString("  " + t.ID + "\n")
		}
	}
	return b.String()
}
