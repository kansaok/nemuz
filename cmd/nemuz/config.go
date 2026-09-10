package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/spf13/cobra"
)

// applyStringDefault fills dst from the saved setting for key, unless flag was
// passed explicitly on this invocation. An explicit flag always wins over a
// saved default — a default is a starting point, never a lock-in.
func applyStringDefault(cmd *cobra.Command, flag string, dst *string, key string, s *config.Config) {
	if cmd.Flags().Changed(flag) {
		return
	}
	if v, ok := s.Get(key); ok && v != "" {
		*dst = v
	}
}

// applyListDefault is applyStringDefault for a repeatable, comma-joined flag
// such as --allow-exec.
func applyListDefault(cmd *cobra.Command, flag string, dst *[]string, key string, s *config.Config) {
	if cmd.Flags().Changed(flag) {
		return
	}
	if v, ok := s.Get(key); ok && v != "" {
		*dst = strings.Split(v, ",")
	}
}

// applyBoolDefault is applyStringDefault for a tri-state setting (skills,
// memories) whose saved value may be "true", "false", or unset.
func applyBoolDefault(cmd *cobra.Command, flag string, dst *bool, key string, s *config.Config) {
	if cmd.Flags().Changed(flag) {
		return
	}
	if v, ok := s.Get(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

// applyIntDefault is applyStringDefault for an integer-valued setting such as
// delegate-depth.
func applyIntDefault(cmd *cobra.Command, flag string, dst *int, key string, s *config.Config) {
	if cmd.Flags().Changed(flag) {
		return
	}
	if v, ok := s.Get(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

// applyProviderModelDefaults pulls provider and model defaults together, then
// lets agents.defaults.model.primary written the OpenClaw way — as
// "provider/model" — split into its two parts. That is what makes a primary
// like "custom-ai-foo.com/agent-v1" (with that provider registered under
// models.providers) actually call the configured gateway rather than silently
// staying on the default anthropic provider. A primary without a slash is left
// whole, so an OpenAI model id like "ft:gpt-4o:org" is untouched.
func applyProviderModelDefaults(cmd *cobra.Command, s *config.Config, providerName, model *string) {
	applyStringDefault(cmd, "provider", providerName, "provider", s)
	applyStringDefault(cmd, "model", model, "model", s)
	if cmd.Flags().Changed("provider") || *providerName == "" || *model == "" {
		return
	}
	head, tail, slash := config.SplitModelPrimary(*model)
	if !slash || head == *providerName {
		return
	}
	if provider.Known(head) {
		*providerName = head
		*model = tail
	}
}

// applyPluginDefault fills pluginCmds from plugins.entries in config unless a
// --plugin flag was passed, so a configured plugin is loaded by every command
// without repeating the flag. Entries are applied in sorted order so repeated
// startup is deterministic.
func applyPluginDefault(cmd *cobra.Command, dst *[]string, s *config.Config) {
	if cmd.Flags().Changed("plugin") {
		return
	}
	names := make([]string, 0, len(s.Plugins.Entries))
	for name := range s.Plugins.Entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := s.Plugins.Entries[name]
		if e.Enabled == nil || *e.Enabled {
			*dst = append(*dst, name)
		}
	}
}

// providersFromConfig converts config's models.providers into the provider
// package's custom catalogue. The config is expected to be the expanded copy,
// so ${NAME} keys are already resolved against the environment.
func providersFromConfig(m config.Models) map[string]provider.Custom {
	if len(m.Providers) == 0 {
		return nil
	}
	out := make(map[string]provider.Custom, len(m.Providers))
	for name, def := range m.Providers {
		defaultModel := ""
		if len(def.Models) > 0 {
			defaultModel = def.Models[0].ID
		}
		out[name] = provider.Custom{
			BaseURL:      def.BaseURL,
			API:          def.API,
			APIKey:       def.APIKey,
			DefaultModel: defaultModel,
		}
	}
	return out
}

func configCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "List the saved defaults for provider, model, sandbox, and more",
		Long: "Saved defaults live in one JSON file, " + config.ConfigFileName + ",\n" +
			"shaped after OpenClaw's openclaw.json — agents.defaults, then\n" +
			"models.providers, channels, gateway, plugins. Every command that\n" +
			"takes --provider, --model, --sandbox and the like reads its default\n" +
			"from here first, and a flag passed on the command line always\n" +
			"overrides it. Nothing here is required — nemuz runs fine with no\n" +
			"config file at all.\n\n" +
			"These flat commands set the agents.defaults.* slots. The nested\n" +
			"sections (providers, channels, gateway) are edited in the file\n" +
			"itself; secrets live in a .env next to it, named with ${NAME} in the\n" +
			"config, and are set from the command line — never opened by hand and\n" +
			"never printed back:\n\n" +
			"  nemuz config env CORPO_API_KEY=sk-...\n" +
			"  nemuz config get model\n" +
			"  nemuz config set model custom-ai-foo.com/agent-v1\n" +
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
	c.AddCommand(configGetCmd(), configSetCmd(), configUnsetCmd(), configEnvCmd())
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

// configEnvCmd edits the .env secrets file next to config.json from the
// command line, so a credential is stored without ever opening the file. It
// deliberately refuses to print a value back: a secret that was already typed
// once into a shell history needs no second appearance.
func configEnvCmd() *cobra.Command {
	var unsetName string
	c := &cobra.Command{
		Use:   "env [NAME=VALUE]",
		Short: "Set a secret in the .env next to config.json",
		Long: "Secrets are the values a config file refers to with ${NAME} — provider\n" +
			"keys, bot tokens, gateway passwords. They live in " + config.EnvFileName + "\n" +
			"next to config.json, not inside it, so the config file stays readable\n" +
			"and shareable. This command edits that file without opening it:\n\n" +
			"  nemuz config env CORPO_API_KEY=sk-...   save or replace a secret\n" +
			"  nemuz config env                          list which names are set\n" +
			"  nemuz config env --unset CORPO_API_KEY    forget a secret\n\n" +
			"The value is never echoed back, and the file is written 0600.\n" +
			"Reference a secret from config.json as \"${CORPO_API_KEY}\"; it is\n" +
			"expanded at load from this file or the process environment.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			if err := paths.EnsureDirs(); err != nil {
				return err
			}

			if unsetName != "" {
				if len(args) > 0 {
					return fmt.Errorf("config env: --unset takes the name by itself, not as a positional argument")
				}
				if err := config.UnsetEnv(paths.Env, unsetName); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s forgotten\n", unsetName)
				return nil
			}

			if len(args) == 0 {
				names, err := config.EnvNames(paths.Env)
				if err != nil {
					return err
				}
				sort.Strings(names)
				if len(names) == 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "no secrets yet — %s\n", paths.Env)
					return nil
				}
				for _, n := range names {
					fmt.Fprintln(cmd.OutOrStdout(), n)
				}
				return nil
			}

			key, value, ok := strings.Cut(args[0], "=")
			if !ok {
				return fmt.Errorf("config env: expected NAME=VALUE, got %q; use `nemuz config env --unset %s` to delete it", args[0], args[0])
			}
			if err := config.SetEnv(paths.Env, key, value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s set in %s\n", key, paths.Env)
			return nil
		},
	}
	c.Flags().StringVar(&unsetName, "unset", "", "forget this secret name (delete its line from the .env)")
	return c
}
