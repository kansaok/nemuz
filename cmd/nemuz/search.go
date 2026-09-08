package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/index"
	"github.com/spf13/cobra"
)

// openIndex prepares the search index over the journal.
func openIndex() (*index.Index, config.Paths, error) {
	paths, err := config.Resolve()
	if err != nil {
		return nil, paths, err
	}
	if err := paths.EnsureDirs(); err != nil {
		return nil, paths, err
	}
	ix, err := index.Open(paths.DB)
	return ix, paths, err
}

func searchCmd() *cobra.Command {
	var limit int
	var full bool
	var withReviews bool

	c := &cobra.Command{
		Use:   "search <words>",
		Short: "Search everything the agent has ever been asked or answered",
		Long: "Matches turns containing all of the given words, in either the question\n" +
			"or the answer. Words are taken literally, so an ordinary question with\n" +
			"punctuation works rather than failing to parse.\n\n" +
			"The index is built from the journal and holds nothing the journal does\n" +
			"not. If it is ever wrong, `nemuz index rebuild` fixes it.",
		Args:    cobra.MinimumNArgs(1),
		Example: `  nemuz search deploy staging`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ix, _, err := openIndex()
			if err != nil {
				return err
			}
			defer ix.Close()

			query := strings.Join(args, " ")
			search := ix.Search
			if withReviews {
				search = ix.SearchAll
			}
			hits, err := search(query, limit)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if len(hits) == 0 {
				total, _ := ix.Count()
				if total == 0 {
					fmt.Fprintln(out, "Nothing indexed yet. Run a turn, or `nemuz index rebuild`.")
					return nil
				}
				fmt.Fprintf(out, "No turns match %q. %d indexed.\n", query, total)
				return nil
			}

			for i, h := range hits {
				if i > 0 {
					fmt.Fprintln(out)
				}
				printHit(out, h, full)
			}
			return nil
		},
	}
	c.Flags().IntVarP(&limit, "limit", "n", index.DefaultSearchLimit, "how many results to show")
	c.Flags().BoolVar(&full, "full", false, "print the whole question and answer instead of a snippet")
	c.Flags().BoolVar(&withReviews, "reviews", false, "also match the agent's own background reviews")
	return c
}

func printHit(out io.Writer, h index.Hit, full bool) {
	t := h.Turn
	marker := ""
	if t.Errored {
		marker = " · failed"
	}
	if t.Role != "" {
		marker += " · " + t.Role
	}
	fmt.Fprintf(out, "%s  %s  %s%s\n", t.ID, t.StartedAt.Local().Format("2006-01-02 15:04"), t.Model, marker)

	if full {
		fmt.Fprintf(out, "  Q: %s\n", indent(t.Prompt, "     ")[5:])
		if t.Answer != "" {
			fmt.Fprintf(out, "  A: %s\n", indent(t.Answer, "     ")[5:])
		}
	} else {
		fmt.Fprintf(out, "  %s\n", strings.Join(strings.Fields(h.Snippet), " "))
	}
	fmt.Fprintf(out, "  replay: nemuz replay %s\n", t.ID)
}

func usageCmd() *cobra.Command {
	var days int

	c := &cobra.Command{
		Use:   "usage",
		Short: "Report what the agent has consumed",
		Long: "Tokens by model and tool, counted from the journal.\n\n" +
			"Tokens only, not money. Turning tokens into a bill needs a price list,\n" +
			"and nemuz does not ship one: prices change without notice, and a\n" +
			"confident figure computed from a stale table is worse than no figure.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ix, _, err := openIndex()
			if err != nil {
				return err
			}
			defer ix.Close()

			var since time.Time
			if days > 0 {
				since = time.Now().AddDate(0, 0, -days)
			}
			report, err := ix.Usage(since)
			if err != nil {
				return err
			}
			printUsage(cmd.OutOrStdout(), report, days)
			return nil
		},
	}
	c.Flags().IntVar(&days, "days", 0, "only count the last N days (0 means everything)")
	return c
}

func printUsage(out io.Writer, r index.UsageReport, days int) {
	period := "all time"
	if days > 0 {
		period = fmt.Sprintf("last %d days", days)
	}
	if r.Turns == 0 {
		fmt.Fprintf(out, "No turns recorded in %s.\n", period)
		return
	}

	in, outTok, cached := r.Totals()
	fmt.Fprintf(out, "%s · %d turns", period, r.Turns)
	if r.Reviews > 0 {
		// Reviews are nemuz spending tokens on its own behalf, which is worth
		// separating when reading a bill.
		fmt.Fprintf(out, " (%d were background reviews)", r.Reviews)
	}
	if r.Errored > 0 {
		fmt.Fprintf(out, " · %d failed", r.Errored)
	}
	fmt.Fprintf(out, "\n%s in / %s out", humanCount(in), humanCount(outTok))
	if cached > 0 {
		fmt.Fprintf(out, " · %s of the input was cached", humanCount(cached))
	}
	fmt.Fprintln(out)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nMODEL\tTURNS\tINPUT\tOUTPUT\tCACHED")
	for _, m := range r.Models {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", m.Model, m.Turns,
			humanCount(m.InputTokens), humanCount(m.OutputTokens), humanCount(m.CachedTokens))
	}
	if len(r.Tools) > 0 {
		fmt.Fprintln(tw, "\nTOOL\tCALLS\tFAILED")
		for _, t := range r.Tools {
			fmt.Fprintf(tw, "%s\t%d\t%d\n", t.Name, t.Calls, t.Failed)
		}
	}
	_ = tw.Flush()

	fmt.Fprintln(out, "\nTokens only — nemuz ships no price list, because a stale one would be worse than none.")
}

// humanCount renders a token count compactly without losing the reader.
func humanCount(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	}
}

func indexCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "index",
		Short: "Manage the search index",
		Long: "The index is built from the journal and holds nothing the journal does\n" +
			"not. Deleting it loses nothing; rebuilding restores it exactly.",
	}
	c.AddCommand(indexRebuildCmd(), indexStatusCmd())
	return c
}

func indexRebuildCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the index from the journal",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ix, paths, err := openIndex()
			if err != nil {
				return err
			}
			defer ix.Close()

			bs, err := blob.Open(paths.Blobs)
			if err != nil {
				return err
			}

			started := time.Now()
			indexed, skipped, err := ix.Rebuild(cmd.Context(), paths.Journal, bs)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "indexed %d turns in %s\n", indexed, time.Since(started).Round(time.Millisecond))
			for _, s := range skipped {
				fmt.Fprintf(out, "  skipped %s\n", s)
			}
			if len(skipped) > 0 {
				fmt.Fprintf(out, "\n%d journal(s) could not be read. They are still on disk; the index simply omits them.\n", len(skipped))
			}
			return nil
		},
	}
}

func indexStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what is indexed, and whether it matches the journal",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ix, paths, err := openIndex()
			if err != nil {
				return err
			}
			defer ix.Close()

			indexed, err := ix.Count()
			if err != nil {
				return err
			}
			recorded, err := countJournals(paths.Journal)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "index\t%s\n", ix.Path())
			fmt.Fprintf(tw, "indexed turns\t%d\n", indexed)
			fmt.Fprintf(tw, "recorded turns\t%d\n", recorded)
			_ = tw.Flush()

			if indexed != recorded {
				fmt.Fprintf(out, "\nThe index is %d turns behind. Run: nemuz index rebuild\n", recorded-indexed)
			}
			return nil
		},
	}
}

// indexTurn adds one finished turn to the search index.
//
// A failure here is reported and dropped. The journal already has the turn, and
// the index can be rebuilt from it at any time, so an indexing problem must
// never cost the user the answer they were waiting for.
func indexTurn(cmd *cobra.Command, turnID string) {
	if turnID == "" {
		return
	}
	ix, paths, err := openIndex()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: could not open the search index: %v\n", err)
		return
	}
	defer ix.Close()

	bs, err := blob.Open(paths.Blobs)
	if err != nil {
		return
	}
	turn, err := journalPath(paths.Journal, turnID)
	if err != nil {
		return
	}
	if err := ix.IngestJournal(turn, bs); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: could not index turn %s: %v\n", turnID, err)
	}
}
