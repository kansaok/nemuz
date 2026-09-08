package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/kansaok/nemuz/internal/plugin"
	"github.com/kansaok/nemuz/internal/tool"
	"github.com/spf13/cobra"
)

// defaultSystemPrompt is the base a turn starts from. Learned skills are
// appended to it, so a skill adds to the agent's instructions rather than
// replacing them.
const defaultSystemPrompt = "You are a careful assistant. Use the tools to answer from the workspace."

func runCmd() *cobra.Command {
	var (
		providerName string
		model        string
		baseURL      string
		workspace    string
		system       string
		maxSteps     int
		pluginCmds   []string
		allowNet     []string
		allowExec    []string
		useSkills    bool
	)

	c := &cobra.Command{
		Use:   "run <prompt>",
		Short: "Run one turn against a live model",
		Long: "Runs a single turn and records it. The turn id it prints can be fed\n" +
			"straight to `nemuz replay` to reproduce the run without spending\n" +
			"another model call.\n\n" +
			"API keys come from the environment; see `nemuz providers`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			p, err := provider.Open(provider.Spec{
				Provider: providerName,
				Model:    model,
				BaseURL:  baseURL,
			})
			if err != nil {
				return err
			}

			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			if err := paths.EnsureDirs(); err != nil {
				return err
			}
			bs, err := blob.Open(paths.Blobs)
			if err != nil {
				return err
			}
			ws, err := tool.NewWorkspace(workspace)
			if err != nil {
				return err
			}
			tools := tool.NewRegistry()
			if err := tools.Register(tool.NewReadFile(ws), tool.NewWriteFile(ws), tool.NewListDir(ws)); err != nil {
				return err
			}

			// Plugins are started before the turn so a misconfigured one fails
			// here, with a clear message, rather than mid-conversation.
			policy := plugin.WorkspacePolicy{
				Workspace: ws.Root(),
				AllowNet:  allowNet,
				AllowExec: allowExec,
			}
			for _, command := range pluginCmds {
				client, _, err := startPlugin(cmd.Context(), command, ws.Root())
				if err != nil {
					return err
				}
				defer client.Close()

				pluginTools, err := client.Tools(policy)
				if err != nil {
					return err
				}
				if err := tools.Register(pluginTools...); err != nil {
					return err
				}
			}

			// Only skills that passed the gate reach the model. Quarantined
			// ones are stored and inspectable but never offered.
			systemPrompt, skillNames := system, []string(nil)
			if useSkills {
				systemPrompt, skillNames, err = activeSkillPrompt(system)
				if err != nil {
					return err
				}
			}

			turnID, err := journal.NewTurnID()
			if err != nil {
				return err
			}
			w, err := journal.Create(paths.Journal, turnID, bs)
			if err != nil {
				return err
			}
			defer w.Close()

			a := &agent.Agent{
				Provider: llm.Record(p, w),
				Tools:    tools,
				Journal:  w,
				Model:    model,
				System:   systemPrompt,
				MaxSteps: maxSteps,
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "turn %s · %s · %s\n%d tools: %s\n",
				turnID, p.Name(), ws.Root(), tools.Len(), strings.Join(tools.Names(), ", "))
			if len(skillNames) > 0 {
				fmt.Fprintf(out, "%d skills: %s\n", len(skillNames), strings.Join(skillNames, ", "))
			}
			fmt.Fprintln(out)

			outcome, runErr := a.Run(context.Background(), cmdArgs[0])
			if closeErr := w.Close(); closeErr != nil && runErr == nil {
				runErr = closeErr
			}
			if runErr != nil {
				fmt.Fprintf(out, "\nThe turn failed, but it was recorded: nemuz journal show %s\n", turnID)
				return runErr
			}

			fmt.Fprintf(out, "%s\n\n", outcome.Text)
			fmt.Fprintf(out, "%d steps · %d tool calls · %d in / %d out tokens",
				outcome.Steps, outcome.ToolCalls, outcome.Usage.InputTokens, outcome.Usage.OutputTokens)
			if outcome.Usage.CachedTokens > 0 {
				fmt.Fprintf(out, " · %d cached", outcome.Usage.CachedTokens)
			}
			fmt.Fprintf(out, "\nreplay with: nemuz replay %s --workspace %s\n", turnID, workspace)
			return nil
		},
	}

	c.Flags().StringVarP(&providerName, "provider", "p", "anthropic", "provider to call; see `nemuz providers`")
	c.Flags().StringVarP(&model, "model", "m", "", "model id (defaults to the provider's own default)")
	c.Flags().StringVar(&baseURL, "base-url", "", "override the provider endpoint, for proxies or self-hosting")
	c.Flags().StringVarP(&workspace, "workspace", "w", ".", "workspace the tools operate on")
	c.Flags().StringVar(&system, "system", defaultSystemPrompt, "system prompt")
	c.Flags().IntVar(&maxSteps, "max-steps", agent.DefaultMaxSteps, "maximum tool rounds before giving up")
	c.Flags().StringArrayVar(&pluginCmds, "plugin", nil, "plugin command to load; repeatable")
	c.Flags().StringArrayVar(&allowNet, "allow-net", nil, "network destination a plugin may reach; repeatable")
	c.Flags().StringArrayVar(&allowExec, "allow-exec", nil, "program a plugin may run; repeatable")
	c.Flags().BoolVar(&useSkills, "skills", true, "include active learned skills in the system prompt")
	return c
}

func providersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "providers",
		Short: "List the providers nemuz can talk to",
		Long: "Three wire formats cover this whole list. Anything speaking the\n" +
			"OpenAI shape needs no adapter of its own — point --base-url at it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PROVIDER\tDEFAULT MODEL\tFORMAT · KEY · NOTES")
			for _, name := range provider.Names() {
				p, _ := provider.Lookup(name)
				model := p.DefaultModel
				if model == "" {
					model = "(pass --model)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", name, model, p.Describe())
			}
			return tw.Flush()
		},
	}
}
