package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/kansaok/nemuz/internal/config"
	"github.com/spf13/cobra"
)

// applyStringDefault fills dst from the saved setting for key, unless flag was
// passed explicitly on this invocation. An explicit flag always wins over a
// saved default — a default is a starting point, never a lock-in.
func applyStringDefault(cmd *cobra.Command, flag string, dst *string, key string, s *config.Settings) {
	if cmd.Flags().Changed(flag) {
		return
	}
	if v, ok := s.Get(key); ok && v != "" {
		*dst = v
	}
}

// applyListDefault is applyStringDefault for a repeatable, comma-joined flag
// such as --allow-exec.
func applyListDefault(cmd *cobra.Command, flag string, dst *[]string, key string, s *config.Settings) {
	if cmd.Flags().Changed(flag) {
		return
	}
	if v, ok := s.Get(key); ok && v != "" {
		*dst = strings.Split(v, ",")
	}
}

// applyBoolDefault is applyStringDefault for a tri-state setting (skills,
// memories) whose saved value may be "true", "false", or unset.
func applyBoolDefault(cmd *cobra.Command, flag string, dst *bool, key string, s *config.Settings) {
	if cmd.Flags().Changed(flag) {
		return
	}
	if v, ok := s.Get(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

func configCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "List the saved defaults for provider, model, sandbox, and more",
		Long: "Every command that takes --provider, --model, --sandbox and the\n" +
			"like reads its default from here first, and a flag passed on the\n" +
			"command line always overrides it. Nothing here is required — nemuz\n" +
			"runs fine with no config file at all — it just saves retyping the\n" +
			"same flags on every command.\n\n" +
			"  nemuz config get model\n" +
			"  nemuz config set model claude-opus-5\n" +
			"  nemuz config unset model",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			settings, err := config.LoadSettings(paths.Config)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tVALUE")
			keys := append([]string(nil), config.Keys...)
			sort.Strings(keys)
			any := false
			for _, k := range keys {
				v, _ := settings.Get(k)
				if v == "" {
					continue
				}
				any = true
				fmt.Fprintf(tw, "%s\t%s\n", k, v)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if !any {
				fmt.Fprintf(out, "nothing saved yet — %s\n", paths.Config)
			}
			return nil
		},
	}
	c.AddCommand(configGetCmd(), configSetCmd(), configUnsetCmd())
	return c
}

func configGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print one saved setting",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			settings, err := config.LoadSettings(paths.Config)
			if err != nil {
				return err
			}
			v, ok := settings.Get(args[0])
			if !ok {
				return fmt.Errorf("config: unknown setting %q; see `nemuz config` for the list", args[0])
			}
			if v == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "%s is not set\n", args[0])
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), v)
			return nil
		},
	}
}

func configSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Save a default so future commands don't need the flag",
		Long: "Valid keys: " + strings.Join(config.Keys, ", ") + ".\n\n" +
			"allow-exec and allow-net take a comma-separated list; skills and\n" +
			"memories take true or false; sandbox takes on, auto, or off.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			settings, err := config.LoadSettings(paths.Config)
			if err != nil {
				return err
			}
			if err := settings.Set(args[0], args[1]); err != nil {
				return err
			}
			if err := config.Save(paths.Config, settings); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s = %s\n", args[0], args[1])
			return nil
		},
	}
}

func configUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <key>",
		Short: "Clear a saved default back to nemuz's built-in behavior",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			settings, err := config.LoadSettings(paths.Config)
			if err != nil {
				return err
			}
			if err := settings.Unset(args[0]); err != nil {
				return err
			}
			if err := config.Save(paths.Config, settings); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s unset\n", args[0])
			return nil
		},
	}
}
