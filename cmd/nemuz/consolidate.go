package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/consolidate"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/kansaok/nemuz/internal/review"
	"github.com/kansaok/nemuz/internal/skill"
	"github.com/spf13/cobra"
)

func consolidateCmd() *cobra.Command {
	var (
		providerName string
		model        string
		baseURL      string
		maxPairs     int
	)

	c := &cobra.Command{
		Use:   "consolidate",
		Short: "Find skills that teach the same thing, and propose merging them",
		Long: "The curator re-verifies, retires and expires skills without a model —\n" +
			"whether two skills are the same idea written twice is a judgement\n" +
			"about meaning, not something a mechanical rule can decide, so this is\n" +
			"its own explicit step.\n\n" +
			"A cheap local pass finds candidate pairs by how much their\n" +
			"descriptions overlap, among active, agent-written, unpinned skills\n" +
			"only. The model is asked about a bounded number of the closest\n" +
			"pairs; most comparisons should end in \"keep separate,\" which costs\n" +
			"nothing further.\n\n" +
			"A merge is not trusted for having been proposed: the result starts\n" +
			"in quarantine like any skill the agent writes for itself, carrying\n" +
			"over both sources' eval scenarios so it can be certified without\n" +
			"writing new ones. The sources are archived, not deleted, with a\n" +
			"reason naming what replaced them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			if err := paths.EnsureDirs(); err != nil {
				return err
			}
			settings := config.LoadSettingsQuiet(paths.Config)
			applyStringDefault(cmd, "provider", &providerName, "provider", settings)
			applyStringDefault(cmd, "model", &model, "model", settings)
			applyStringDefault(cmd, "base-url", &baseURL, "base-url", settings)

			p, err := provider.Open(provider.Spec{Provider: providerName, Model: model, BaseURL: baseURL})
			if err != nil {
				return err
			}
			if model == "" {
				if preset, ok := provider.Lookup(providerName); ok {
					model = preset.DefaultModel
				}
			}
			skills, err := skill.Open(paths.Skills)
			if err != nil {
				return err
			}
			bs, err := blob.Open(paths.Blobs)
			if err != nil {
				return err
			}
			ledger, err := review.OpenLedger(filepath.Join(paths.Root, "consolidated.txt"))
			if err != nil {
				return err
			}

			c := &consolidate.Consolidator{
				Provider:   p,
				Model:      model,
				Skills:     skills,
				JournalDir: paths.Journal,
				Blobs:      bs,
				MaxPairs:   maxPairs,
				Ledger:     ledger,
			}
			report, err := c.Run(cmd.Context())
			if err != nil {
				return err
			}
			printConsolidationReport(cmd.OutOrStdout(), report)
			return nil
		},
	}

	c.Flags().StringVarP(&providerName, "provider", "p", "anthropic", "provider to call; see `nemuz providers`")
	c.Flags().StringVarP(&model, "model", "m", "", "model id (defaults to the provider's own default)")
	c.Flags().StringVar(&baseURL, "base-url", "", "override the provider endpoint")
	c.Flags().IntVar(&maxPairs, "max-pairs", consolidate.DefaultMaxPairs, "most candidate pairs to ask about in one run")
	return c
}

func printConsolidationReport(out io.Writer, report consolidate.Report) {
	if len(report.Outcomes) == 0 {
		fmt.Fprintln(out, "no candidate pairs found — nothing overlapping enough to ask about")
		return
	}
	fmt.Fprintf(out, "%d candidate pair(s) considered, %d merged\n\n", len(report.Outcomes), report.Merged())

	for _, o := range report.Outcomes {
		switch {
		case o.Skipped:
			fmt.Fprintf(out, "  skip  %s + %s — asked recently\n", o.A, o.B)
		case o.Decision == consolidate.DecisionMerged:
			fmt.Fprintf(out, "merged  %s + %s → %s\n", o.A, o.B, o.NewSkill)
			fmt.Fprintf(out, "        needs certifying: nemuz skill certify %s\n", o.NewSkill)
		case o.Decision == consolidate.DecisionKeptSeparate:
			fmt.Fprintf(out, "  keep  %s + %s — %s\n", o.A, o.B, o.Reason)
		default:
			fmt.Fprintf(out, "  fail  %s + %s — %s\n", o.A, o.B, o.Reason)
		}
		if o.TurnID != "" {
			fmt.Fprintf(out, "        reasoning: nemuz journal show %s\n", o.TurnID)
		}
	}
}
