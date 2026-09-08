package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/memory"
	"github.com/spf13/cobra"
)

func memoryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "memory",
		Aliases: []string{"mem"},
		Short:   "Inspect and correct what the agent remembers",
		Long: "Memories are facts the agent kept from past turns. Unlike skills they\n" +
			"are not gated: a skill is a procedure whose misuse does damage, while a\n" +
			"memory is a claim about the world, corrected the way a person corrects\n" +
			"one — by saying so.\n\n" +
			"They are plain Markdown files, so they can also be read, edited, and\n" +
			"deleted without nemuz.",
	}
	c.AddCommand(memoryListCmd(), memoryShowCmd(), memoryForgetCmd(),
		memoryRecallCmd(), memoryPinCmd(true), memoryPinCmd(false))
	return c
}

func openMemoryStore() (*memory.Store, error) {
	paths, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	return memory.Open(paths.Memories)
}

func memoryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List everything the agent remembers, oldest first",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openMemoryStore()
			if err != nil {
				return err
			}
			all, err := store.List()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(all) == 0 {
				fmt.Fprintf(out, "Nothing remembered yet in %s\n", store.Root())
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tUSES\tFACT")
			for _, m := range all {
				marker := ""
				if m.Pinned {
					marker = " (pinned)"
				}
				fmt.Fprintf(tw, "%s\t%s%s\t%d\t%s\n", short(m.ID), m.Kind, marker, m.UseCount, oneLine(m.Text, 70))
			}
			return tw.Flush()
		},
	}
}

func memoryShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Print one memory in full, with where it came from",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openMemoryStore()
			if err != nil {
				return err
			}
			m, err := resolveMemory(store, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s\n\n", m.Text)
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "id\t%s\n", m.ID)
			fmt.Fprintf(tw, "kind\t%s\n", m.Kind)
			fmt.Fprintf(tw, "learned\t%s\n", m.CreatedAt.Format(time.RFC3339))
			if m.Source != "" {
				fmt.Fprintf(tw, "from turn\t%s — inspect with: nemuz journal show %s\n", m.Source, m.Source)
			}
			if len(m.Tags) > 0 {
				fmt.Fprintf(tw, "tags\t%s\n", strings.Join(m.Tags, ", "))
			}
			fmt.Fprintf(tw, "recalled\t%d times\n", m.UseCount)
			if m.Pinned {
				fmt.Fprintf(tw, "pinned\tyes — included in every prompt\n")
			}
			return tw.Flush()
		},
	}
}

func memoryRecallCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use:   "recall <query>",
		Short: "Show which memories a question would pull into the prompt",
		Long: "Recall is deterministic: the same query against the same store always\n" +
			"returns the same memories in the same order. This command runs exactly\n" +
			"what a turn would run, so a surprising answer can be traced.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openMemoryStore()
			if err != nil {
				return err
			}
			got, err := store.Recall(args[0], limit)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(got) == 0 {
				fmt.Fprintln(out, "Nothing would be recalled for that.")
				return nil
			}
			for _, m := range got {
				fmt.Fprintf(out, "%s  %s\n", short(m.ID), m.Prompt())
			}
			return nil
		},
	}
	c.Flags().IntVarP(&limit, "limit", "n", memory.DefaultRecallLimit, "how many memories to recall")
	return c
}

func memoryForgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <id>",
		Short: "Delete a memory the agent got wrong",
		Long: "Deletion here is real, unlike skill archiving. A skill's removal changes\n" +
			"what the agent can do, so it stays reversible. A wrong fact should simply\n" +
			"stop being there.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openMemoryStore()
			if err != nil {
				return err
			}
			m, err := resolveMemory(store, args[0])
			if err != nil {
				return err
			}
			if err := store.Forget(m.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "forgot %s: %s\n", short(m.ID), oneLine(m.Text, 60))
			return nil
		},
	}
}

func memoryPinCmd(pin bool) *cobra.Command {
	use, short := "unpin <id>", "Let a memory be recalled only when relevant"
	if pin {
		use, short = "pin <id>", "Include a memory in every prompt, whatever the question"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openMemoryStore()
			if err != nil {
				return err
			}
			m, err := resolveMemory(store, args[0])
			if err != nil {
				return err
			}
			if err := store.Pin(m.ID, pin); err != nil {
				return err
			}
			state := "unpinned"
			if pin {
				state = "pinned"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", state, shortID(m.ID))
			return nil
		},
	}
}

// resolveMemory accepts a full id or any unambiguous prefix, so an operator can
// copy the shortened id that `memory ls` prints.
func resolveMemory(store *memory.Store, ref string) (*memory.Memory, error) {
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	ref = strings.ToUpper(ref)
	var matches []*memory.Memory
	for _, m := range all {
		if m.ID == ref {
			return m, nil
		}
		if strings.HasPrefix(m.ID, ref) {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no memory matching %q", ref)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%q matches %d memories; use more characters", ref, len(matches))
	}
}

func short(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:10]
}

func shortID(id string) string { return short(id) }

func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
