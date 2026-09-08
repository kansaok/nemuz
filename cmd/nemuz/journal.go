package main

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/spf13/cobra"
)

func journalCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "journal",
		Short: "Inspect recorded turns",
	}
	c.AddCommand(journalListCmd(), journalShowCmd(), journalVerifyCmd())
	return c
}

func journalListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Short:   "List recorded turns, oldest first",
		Args:    cobra.NoArgs,
		Aliases: []string{"list"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			turns, err := journal.List(paths.Journal)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(turns) == 0 {
				fmt.Fprintf(out, "No turns recorded yet in %s\n", paths.Journal)
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TURN\tEVENTS\tDIGEST")
			for _, t := range turns {
				events, err := journal.Read(t.Path)
				if err != nil {
					fmt.Fprintf(tw, "%s\t?\tunreadable: %v\n", t.ID, err)
					continue
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\n", t.ID, len(events), journal.Digest(events)[:16])
			}
			return tw.Flush()
		},
	}
}

func journalShowCmd() *cobra.Command {
	var full bool
	c := &cobra.Command{
		Use:   "show <turn>",
		Short: "Print the events of one turn",
		Long:  "Accepts a full turn id or any unambiguous prefix of one.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			turn, err := journal.Find(paths.Journal, args[0])
			if err != nil {
				return err
			}
			events, err := journal.Read(turn.Path)
			if err != nil {
				return err
			}
			bs, err := blob.Open(paths.Blobs)
			if err != nil {
				return err
			}
			return printEvents(cmd.OutOrStdout(), events, bs, full)
		},
	}
	c.Flags().BoolVar(&full, "full", false, "fetch spilled payloads from the blob store instead of showing their hash")
	return c
}

func printEvents(out io.Writer, events []journal.Event, bs *blob.Store, full bool) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SEQ\tKIND\tSIZE\tPAYLOAD")
	for _, e := range events {
		payload, err := renderPayload(e, bs, full)
		if err != nil {
			payload = "<" + err.Error() + ">"
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%s\n", e.Seq, e.Kind, e.Size, payload)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%d events, digest %s\n", len(events), journal.Digest(events))
	return nil
}

// renderPayload keeps `journal show` readable: spilled payloads are summarised
// by hash unless the operator asks for them, and long lines are truncated.
func renderPayload(e journal.Event, bs *blob.Store, full bool) (string, error) {
	if e.Spilled() && !full {
		return fmt.Sprintf("blob:%s (%d bytes)", e.Blob.Short(), e.Size), nil
	}
	body, err := e.Content(bs)
	if err != nil {
		return "", err
	}
	var buf json.RawMessage = body
	compact, err := json.Marshal(buf)
	if err != nil {
		compact = body
	}
	const limit = 96
	if !full && len(compact) > limit {
		return string(compact[:limit]) + "…", nil
	}
	return string(compact), nil
}

func journalVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify [turn]",
		Short: "Check that recorded turns are intact",
		Long: "Re-reads each turn and confirms its events are contiguous and its\n" +
			"spilled payloads are still present in the blob store. A turn that\n" +
			"fails here cannot be replayed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			bs, err := blob.Open(paths.Blobs)
			if err != nil {
				return err
			}

			var turns []journal.Turn
			if len(args) == 1 {
				t, err := journal.Find(paths.Journal, args[0])
				if err != nil {
					return err
				}
				turns = []journal.Turn{t}
			} else if turns, err = journal.List(paths.Journal); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if len(turns) == 0 {
				fmt.Fprintf(out, "No turns recorded yet in %s\n", paths.Journal)
				return nil
			}

			var broken int
			for _, t := range turns {
				if err := verifyTurn(t, bs); err != nil {
					fmt.Fprintf(out, "FAIL  %s  %v\n", t.ID, err)
					broken++
					continue
				}
				fmt.Fprintf(out, "  ok  %s\n", t.ID)
			}
			if broken > 0 {
				return fmt.Errorf("%d of %d turns cannot be replayed", broken, len(turns))
			}
			fmt.Fprintf(out, "\n%d turns intact\n", len(turns))
			return nil
		},
	}
}

func verifyTurn(t journal.Turn, bs *blob.Store) error {
	events, err := journal.Read(t.Path)
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.Spilled() && !bs.Has(e.Blob) {
			return fmt.Errorf("event %d references missing blob %s", e.Seq, e.Blob.Short())
		}
	}
	return nil
}

// countJournals reports how many turns are recorded on disk.
func countJournals(dir string) (int, error) {
	turns, err := journal.List(dir)
	if err != nil {
		return 0, err
	}
	return len(turns), nil
}

// journalPath resolves a turn id to its journal file.
func journalPath(dir, turnID string) (string, error) {
	turn, err := journal.Find(dir, turnID)
	if err != nil {
		return "", err
	}
	return turn.Path, nil
}
