package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/spf13/cobra"
)

func chatCmd() *cobra.Command {
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
		Use:   "chat",
		Short: "Talk to the agent as a back-and-forth conversation",
		Long: "Opens the model and the toolset once, then reads one line at a time\n" +
			"and answers it — no need to re-run `nemuz run` for every message.\n\n" +
			"Each line is still its own recorded turn, replayable with `nemuz\n" +
			"replay` like any other; continuity between them comes from recall,\n" +
			"the same way it does for two separate `nemuz run` calls.\n\n" +
			"Type /exit or /quit to leave, or press Ctrl+D.",
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
			applyStringDefault(cmd, "sandbox", &sandboxMode, "sandbox", settings)
			applyStringDefault(cmd, "review-model", &reviewModel, "review-model", settings)
			applyListDefault(cmd, "allow-exec", &allowExec, "allow-exec", settings)
			applyListDefault(cmd, "allow-net", &allowNet, "allow-net", settings)
			applyBoolDefault(cmd, "skills", &useSkills, "skills", settings)
			applyBoolDefault(cmd, "memories", &useMemories, "memories", settings)
			applyIntDefault(cmd, "delegate-depth", &delegateDepth, "delegate-depth", settings)

			p, err := provider.Open(provider.Spec{
				Provider: providerName,
				Model:    model,
				BaseURL:  baseURL,
			})
			if err != nil {
				return err
			}
			if model == "" {
				if preset, ok := provider.Lookup(providerName); ok {
					model = preset.DefaultModel
				}
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

			fmt.Fprintf(out, "nemuz %s · %s · %s · sandbox %s\n", model, p.Name(), ts.Workspace, ts.Sandbox)
			fmt.Fprintln(out, "Type /exit to leave.")
			fmt.Fprintln(out)

			return runChatLoop(cmd, cmd.InOrStdin(), out, sess, delegateDepth)
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
	c.Flags().StringVar(&sandboxMode, "sandbox", string(SandboxAuto), "confine the built-in tools: on, auto, or off")
	c.Flags().BoolVar(&useMemories, "memories", true, "recall relevant memories into the system prompt")
	c.Flags().BoolVar(&doReview, "review", true, "after each turn, decide what was worth remembering")
	c.Flags().StringVar(&reviewModel, "review-model", "", "cheaper model for the review (defaults to --model)")
	c.Flags().BoolVar(&doCurate, "curate", true, "once a day, re-verify and tidy the agent's own skills")
	c.Flags().IntVar(&delegateDepth, "delegate-depth", DefaultDelegateDepth,
		"levels an agent may delegate a sub-task to another agent turn; 0 disables delegation")
	return c
}

// runChatLoop reads one line at a time and answers each as its own turn,
// until the input closes or the user asks to leave. Each line is a fresh
// top-level turn, so its delegation budget is reseeded to the full depth
// rather than carried over from whatever the previous line spent.
func runChatLoop(cmd *cobra.Command, in io.Reader, out io.Writer, sess *turnSession, delegateDepth int) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		fmt.Fprint(out, "you> ")
		if !scanner.Scan() {
			fmt.Fprintln(out)
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch line {
		case "/exit", "/quit":
			return nil
		}

		ctx := withDelegateDepth(context.Background(), delegateDepth)
		outcome, turnID, err := sess.runTurn(ctx, cmd, out, line, false)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			fmt.Fprintf(out, "nemuz> (error) %v\n\n", err)
			continue
		}
		fmt.Fprintf(out, "nemuz> %s\n", outcome.Text)
		fmt.Fprintf(out, "       %d steps · %d tool calls · turn %s\n\n", outcome.Steps, outcome.ToolCalls, turnID)
	}
}
