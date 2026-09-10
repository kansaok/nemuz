package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/spf13/cobra"
)

// defaultSystemPrompt is the base a turn starts from. Learned skills are
// appended to it, so a skill adds to the agent's instructions rather than
// replacing them.
const defaultSystemPrompt = "You are a careful assistant. Use the tools to answer from the workspace."

func runCmd() *cobra.Command {
	var (
		providerName  string
		model         string
		baseURL       string
		workspace     string
		system        string
		maxSteps      int
		pluginCmds    []string
		allowNet      []string
		allowExec     []string
		useSkills     bool
		useMemories   bool
		doReview      bool
		reviewModel   string
		sandboxMode   string
		doCurate      bool
		delegateDepth int
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
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			if err := paths.EnsureDirs(); err != nil {
				return err
			}
			settings := config.LoadSettingsQuiet(paths.Config)
			applyProviderModelDefaults(cmd, settings, &providerName, &model)
			applyStringDefault(cmd, "base-url", &baseURL, "base-url", settings)
			applyStringDefault(cmd, "sandbox", &sandboxMode, "sandbox", settings)
			applyStringDefault(cmd, "workspace", &workspace, "workspace", settings)
			applyStringDefault(cmd, "review-model", &reviewModel, "review-model", settings)
			applyListDefault(cmd, "allow-exec", &allowExec, "allow-exec", settings)
			applyListDefault(cmd, "allow-net", &allowNet, "allow-net", settings)
			applyBoolDefault(cmd, "skills", &useSkills, "skills", settings)
			applyBoolDefault(cmd, "memories", &useMemories, "memories", settings)
			applyBoolDefault(cmd, "review", &doReview, "review", settings)
			applyBoolDefault(cmd, "curate", &doCurate, "curate", settings)
			applyIntDefault(cmd, "delegate-depth", &delegateDepth, "delegate-depth", settings)
			applyPluginDefault(cmd, &pluginCmds, settings)

			p, err := provider.Open(provider.Spec{
				Provider: providerName,
				Model:    model,
				BaseURL:  baseURL,
			})
			if err != nil {
				return err
			}
			bs, err := blob.Open(paths.Blobs)
			if err != nil {
				return err
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

			sess := &turnSession{
				paths:       paths,
				provider:    p,
				model:       model,
				ts:          ts,
				bs:          bs,
				baseSystem:  system,
				maxSteps:    maxSteps,
				useSkills:   useSkills,
				useMemories: useMemories,
				doReview:    doReview,
				reviewModel: reviewModel,
				doCurate:    doCurate,
			}

			out := cmd.OutOrStdout()
			if err := registerDelegateTool(sess, cmd, out, delegateDepth); err != nil {
				return err
			}

			ctx := withDelegateDepth(context.Background(), delegateDepth)
			outcome, turnID, runErr := sess.runTurn(ctx, cmd, out, cmdArgs[0], true)
			if runErr != nil {
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
	c.Flags().StringArrayVar(&pluginCmds, "plugin", nil, "plugin command to load; repeatable (default: plugins.entries in config)")
	c.Flags().StringArrayVar(&allowNet, "allow-net", nil, "network destination a plugin may reach; repeatable")
	c.Flags().StringArrayVar(&allowExec, "allow-exec", nil, "program a plugin may run; repeatable")
	c.Flags().BoolVar(&useSkills, "skills", true, "include active learned skills in the system prompt")
	c.Flags().StringVar(&sandboxMode, "sandbox", string(SandboxAuto), "confine the built-in tools: on, auto, or off")
	c.Flags().BoolVar(&useMemories, "memories", true, "recall relevant memories into the system prompt")
	c.Flags().BoolVar(&doReview, "review", true, "after the turn, decide what was worth remembering")
	c.Flags().StringVar(&reviewModel, "review-model", "", "cheaper model for the review (defaults to --model)")
	c.Flags().BoolVar(&doCurate, "curate", true, "once a day, re-verify and tidy the agent's own skills")
	c.Flags().IntVar(&delegateDepth, "delegate-depth", DefaultDelegateDepth,
		"levels an agent may delegate a sub-task to another agent turn; 0 disables delegation")
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
				if preset, ok := provider.Lookup(name); ok {
					model := preset.DefaultModel
					if model == "" {
						model = "(pass --model)"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", name, model, preset.Describe())
					continue
				}
				if cp, ok := provider.LookupCustom(name); ok {
					model := cp.DefaultModel
					if model == "" {
						model = "(pass --model)"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", name, model, strings.TrimSpace(cp.API+" · config-defined"))
				}
			}
			return tw.Flush()
		},
	}
}
