package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

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
	c.AddCommand(journalListCmd(), journalShowCmd(), journalVerifyCmd(), journalPruneCmd(), journalRedactCmd())
	return c
}

func journalRedactCmd() *cobra.Command {
	var fields []string
	var apply bool
	c := &cobra.Command{Use: "redact <turn>", Short: "Sanitise selected JSON fields in a turn", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(fields) == 0 {
			return fmt.Errorf("journal redact: name at least one --field")
		}
		if !apply {
			return fmt.Errorf("journal redact: refusing to alter a journal without --apply")
		}
		paths, err := config.Resolve()
		if err != nil {
			return err
		}
		turn, err := journal.Find(paths.Journal, args[0])
		if err != nil {
			return err
		}
		if journal.IsRedacted(turn) {
			return fmt.Errorf("journal redact: %s is already redacted", turn.ID)
		}
		bs, err := blob.Open(paths.Blobs)
		if err != nil {
			return err
		}
		changed, err := redactTurn(turn, bs, fields)
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("journal redact: none of %s appeared in %s", strings.Join(fields, ", "), turn.ID)
		}
		marker := []byte(fmt.Sprintf("redacted_at=%s\nfields=%s\n", time.Now().UTC().Format(time.RFC3339), strings.Join(fields, ",")))
		if err := os.WriteFile(journal.RedactionPath(turn.Path), marker, 0o600); err != nil {
			return err
		}
		if _, err := pruneJournals(paths, bs, time.Time{}, true); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "redacted %d value(s) in %s; replay is now disabled for this turn\n", changed, turn.ID)
		return nil
	}}
	c.Flags().StringArrayVar(&fields, "field", nil, "JSON field name to replace; repeatable")
	c.Flags().BoolVar(&apply, "apply", false, "perform redaction")
	return c
}

func redactTurn(turn journal.Turn, bs *blob.Store, fields []string) (int, error) {
	want := map[string]bool{}
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			want[f] = true
		}
	}
	events, err := journal.Read(turn.Path)
	if err != nil {
		return 0, err
	}
	changed := 0
	for i := range events {
		body, err := events[i].Content(bs)
		if err != nil {
			return 0, err
		}
		var value any
		if json.Unmarshal(body, &value) != nil {
			continue
		}
		n := redactValue(value, want)
		if n == 0 {
			continue
		}
		changed += n
		body, err = json.Marshal(value)
		if err != nil {
			return 0, err
		}
		events[i].Size, events[i].Payload, events[i].Blob = len(body), nil, ""
		if len(body) >= journal.SpillThreshold {
			ref, err := bs.PutBytes(body)
			if err != nil {
				return 0, err
			}
			events[i].Blob = ref
		} else {
			events[i].Payload = body
		}
	}
	if changed == 0 {
		return 0, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(turn.Path), ".redact-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return 0, err
	}
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			tmp.Close()
			return 0, err
		}
		if _, err := tmp.Write(append(line, '\n')); err != nil {
			tmp.Close()
			return 0, err
		}
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, turn.Path); err != nil {
		return 0, err
	}
	return changed, nil
}

func redactValue(v any, fields map[string]bool) int {
	count := 0
	switch node := v.(type) {
	case map[string]any:
		for key, value := range node {
			if fields[key] {
				node[key] = "[REDACTED]"
				count++
			} else {
				count += redactValue(value, fields)
			}
		}
	case []any:
		for _, value := range node {
			count += redactValue(value, fields)
		}
	}
	return count
}

func journalPruneCmd() *cobra.Command {
	var olderThan time.Duration
	var apply bool
	c := &cobra.Command{
		Use:   "prune",
		Short: "Remove old turns and unreferenced payload blobs",
		Long: "Shows turns older than --older-than by default. Pass --apply to remove\n" +
			"them, delete blobs no remaining turn references, and reset the derived\n" +
			"search index. Deleted turns cannot be replayed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if olderThan <= 0 {
				return fmt.Errorf("journal prune: --older-than must be positive")
			}
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			bs, err := blob.Open(paths.Blobs)
			if err != nil {
				return err
			}
			report, err := pruneJournals(paths, bs, time.Now().Add(-olderThan), apply)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, id := range report.Turns {
				fmt.Fprintf(out, "  %s\n", id)
			}
			if !apply {
				fmt.Fprintf(out, "%d turn(s) older than %s. Re-run with --apply to delete them.\n", len(report.Turns), olderThan)
				return nil
			}
			fmt.Fprintf(out, "deleted %d turn(s), %d unreferenced blob(s); search index reset\n", len(report.Turns), report.Blobs)
			return nil
		},
	}
	c.Flags().DurationVar(&olderThan, "older-than", 0, "remove turns started before this age, e.g. 720h")
	c.Flags().BoolVar(&apply, "apply", false, "perform deletion instead of previewing it")
	return c
}

type pruneReport struct {
	Turns []string
	Blobs int
}

func pruneJournals(paths config.Paths, bs *blob.Store, before time.Time, apply bool) (pruneReport, error) {
	turns, err := journal.List(paths.Journal)
	if err != nil {
		return pruneReport{}, err
	}
	var report pruneReport
	live := map[blob.Ref]bool{}
	for _, turn := range turns {
		events, err := journal.Read(turn.Path)
		if err != nil {
			return pruneReport{}, err
		}
		if len(events) > 0 && events[0].TS.Before(before) {
			report.Turns = append(report.Turns, turn.ID)
			continue
		}
		for _, e := range events {
			if e.Spilled() {
				live[e.Blob] = true
			}
		}
	}
	if !apply {
		return report, nil
	}
	for _, id := range report.Turns {
		if err := os.Remove(filepath.Join(paths.Journal, id+".jsonl")); err != nil {
			return report, err
		}
	}
	refs, err := bs.Refs()
	if err != nil {
		return report, err
	}
	for _, ref := range refs {
		if !live[ref] {
			if err := bs.Remove(ref); err != nil {
				return report, err
			}
			report.Blobs++
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(paths.DB + suffix)
	}
	return report, nil
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
