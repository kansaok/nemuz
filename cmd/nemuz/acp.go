package main

import (
	"fmt"
	"os"

	"github.com/kansaok/nemuz"
	"github.com/kansaok/nemuz/internal/acp"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/spf13/cobra"
)

func acpCmd() *cobra.Command {
	var (
		providerName string
		model        string
		baseURL      string
		workspace    string
		system       string
		sandboxMode  string
		pluginCmds   []string
		allowNet     []string
		allowExec    []string
		useSkills    bool
		useMemories  bool
	)

	c := &cobra.Command{
		Use:   "acp",
		Short: "Serve the agent over the Agent Client Protocol, on stdio",
		Long: "Editors that speak ACP — Zed among them — drive nemuz through this.\n" +
			"It is not run by hand: the editor launches it and talks JSON-RPC over\n" +
			"stdin and stdout, so nothing but the protocol may be written to stdout.\n\n" +
			"In Zed, add an external agent whose command is:\n\n" +
			"  nemuz acp --provider anthropic\n\n" +
			"The agent is bound to one workspace, so launch it from the project you\n" +
			"want it to work on. A session opened for a different directory is\n" +
			"refused rather than answered from the wrong place.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			settings := config.LoadSettingsQuiet(paths.Config)
			applyProviderModelDefaults(cmd, settings, &providerName, &model)
			applyStringDefault(cmd, "base-url", &baseURL, "base-url", settings)
			applyStringDefault(cmd, "sandbox", &sandboxMode, "sandbox", settings)
			applyStringDefault(cmd, "workspace", &workspace, "workspace", settings)
			applyListDefault(cmd, "allow-exec", &allowExec, "allow-exec", settings)
			applyListDefault(cmd, "allow-net", &allowNet, "allow-net", settings)
			applyBoolDefault(cmd, "skills", &useSkills, "skills", settings)
			applyBoolDefault(cmd, "memories", &useMemories, "memories", settings)
			applyPluginDefault(cmd, &pluginCmds, settings)

			p, err := provider.Open(provider.Spec{Provider: providerName, Model: model, BaseURL: baseURL})
			if err != nil {
				return err
			}
			if model == "" {
				if preset, ok := provider.Lookup(providerName); ok {
					model = preset.DefaultModel
				}
			}

			ts, err := buildToolset(cmd.Context(), toolsetOptions{
				Workspace:  workspace,
				Sandbox:    SandboxMode(sandboxMode),
				PluginCmds: pluginCmds,
				AllowNet:   allowNet,
				AllowExec:  allowExec,
			})
			if err != nil {
				return err
			}
			defer ts.Close()

			agent, err := nemuz.Open(nemuz.Options{
				Provider:        p,
				Model:           model,
				Workspace:       ts.Workspace,
				System:          system,
				Tools:           ts.Registry.Tools(),
				WithoutBuiltins: true,
				WithoutSkills:   !useSkills,
				WithoutMemories: !useMemories,
			})
			if err != nil {
				return err
			}
			defer agent.Close()

			server, err := acp.NewServer(acp.Options{
				Runner:    agent,
				Workspace: ts.Workspace,
				Name:      "nemuz",
				Version:   Version,
			})
			if err != nil {
				return err
			}

			// stdout carries the protocol, so the startup banner goes to
			// stderr — where the editor shows it as agent output rather than
			// choking on it.
			fmt.Fprintf(os.Stderr, "nemuz %s on ACP v%d · %s via %s · %s · sandbox %s\n",
				Version, acp.ProtocolVersion, model, p.Name(), ts.Workspace, ts.Sandbox)

			return server.Serve(cmd.Context(), os.Stdin, os.Stdout)
		},
	}

	c.Flags().StringVarP(&providerName, "provider", "p", "anthropic", "provider to call; see `nemuz providers`")
	c.Flags().StringVarP(&model, "model", "m", "", "model id (defaults to the provider's own default)")
	c.Flags().StringVar(&baseURL, "base-url", "", "override the provider endpoint")
	c.Flags().StringVarP(&workspace, "workspace", "w", ".", "workspace the tools operate on")
	c.Flags().StringVar(&system, "system", defaultSystemPrompt, "system prompt")
	c.Flags().StringVar(&sandboxMode, "sandbox", string(SandboxAuto), "confine the built-in tools: on, auto, or off")
	c.Flags().StringArrayVar(&pluginCmds, "plugin", nil, "plugin command to load; repeatable (default: plugins.entries in config)")
	c.Flags().StringArrayVar(&allowNet, "allow-net", nil, "network destination a plugin may reach; repeatable")
	c.Flags().StringArrayVar(&allowExec, "allow-exec", nil, "program a plugin may run; repeatable")
	c.Flags().BoolVar(&useSkills, "skills", true, "include active learned skills in the system prompt")
	c.Flags().BoolVar(&useMemories, "memories", true, "recall relevant memories into the system prompt")
	return c
}
