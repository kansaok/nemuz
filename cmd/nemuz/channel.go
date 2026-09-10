package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/kansaok/nemuz/internal/telegram"
	"github.com/spf13/cobra"
)

// telegramTokenEnv holds the bot token from @BotFather, kept out of
// nemuz config the same way every other API key is: a secret belongs in the
// environment, never in a file meant to be readable and shareable.
const telegramTokenEnv = "TELEGRAM_BOT_TOKEN"

// channelCmd groups the chat-platform channels — telegram today, others as
// they are built.
func channelCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "channel",
		Short: "Talk to the agent from a chat platform",
	}
	c.AddCommand(telegramCmd())
	return c
}

func telegramCmd() *cobra.Command {
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
		token         string
		allowUsers    []int64
		pollTimeout   int
	)

	c := &cobra.Command{
		Use:   "telegram",
		Short: "Answer Telegram messages as the agent",
		Long: "Long-polls the Telegram Bot API — no public URL or TLS certificate\n" +
			"needed, since the bot reaches out to Telegram rather than the other\n" +
			"way around. Each incoming message becomes its own recorded turn,\n" +
			"exactly like `nemuz run` or `nemuz chat`, replayable the same way.\n\n" +
			"Get a token from @BotFather, then either export " + telegramTokenEnv + "\n" +
			"or pass --token. --allow-user is required: without an explicit\n" +
			"allowlist of Telegram user ids, every message would be refused\n" +
			"rather than let a bot token alone open the agent to anyone who\n" +
			"finds it.\n\n" +
			"One workspace is shared across every allowed user — this is a\n" +
			"single-tenant channel, not yet a per-user sandbox.",
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

			if token == "" {
				token = os.Getenv(telegramTokenEnv)
			}
			if token == "" {
				return fmt.Errorf("channel telegram: no bot token; set %s or pass --token", telegramTokenEnv)
			}
			if len(allowUsers) == 0 {
				return fmt.Errorf("channel telegram: --allow-user is required — list the Telegram user id(s) allowed to talk to this bot, or anyone who finds the token can")
			}

			p, err := provider.Open(provider.Spec{Provider: providerName, Model: model, BaseURL: baseURL})
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

			tc := &telegram.Client{Token: token}
			me, err := tc.GetMe(cmd.Context())
			if err != nil {
				return fmt.Errorf("channel telegram: %w", err)
			}
			fmt.Fprintf(out, "nemuz %s · telegram @%s · %s via %s · %s · sandbox %s\n",
				Version, me.Username, model, p.Name(), ts.Workspace, ts.Sandbox)
			fmt.Fprintf(out, "allowed users: %v\n\n", allowUsers)

			offsetPath := filepath.Join(paths.Root, "telegram-offset")
			bot := &telegramBot{
				client:        tc,
				session:       sess,
				cmd:           cmd,
				out:           out,
				allowUsers:    allowUsers,
				pollTimeout:   pollTimeout,
				delegateDepth: delegateDepth,
				offsetPath:    offsetPath,
			}
			return bot.run(cmd.Context())
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
	c.Flags().StringVar(&token, "token", "", "bot token from @BotFather (defaults to "+telegramTokenEnv+")")
	c.Flags().Int64SliceVar(&allowUsers, "allow-user", nil, "Telegram user id allowed to talk to this bot; repeatable, required")
	c.Flags().IntVar(&pollTimeout, "poll-timeout", 30, "seconds Telegram may hold a getUpdates call open waiting for a message")
	return c
}

// telegramBot is the long-poll loop, separated from telegramCmd's flag
// parsing so it can be driven directly from a test with a fake client.
type telegramBot struct {
	client        *telegram.Client
	session       *turnSession
	cmd           *cobra.Command
	out           interface{ Write([]byte) (int, error) }
	allowUsers    []int64
	pollTimeout   int
	delegateDepth int
	offsetPath    string
}

func (b *telegramBot) run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	offset := loadTelegramOffset(b.offsetPath)
	for {
		if ctx.Err() != nil {
			return nil
		}
		updates, err := b.client.GetUpdates(ctx, offset, b.pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintf(b.out, "channel telegram: %v; retrying\n", err)
			if !sleepOrDone(ctx, 5*time.Second) {
				return nil
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			b.handle(ctx, u)
		}
		if len(updates) > 0 {
			_ = saveTelegramOffset(b.offsetPath, offset)
		}
	}
}

func (b *telegramBot) handle(ctx context.Context, u telegram.Update) {
	if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
		return
	}
	msg := u.Message
	if msg.From == nil || !allowedUser(b.allowUsers, msg.From.ID) {
		who := "unknown"
		if msg.From != nil {
			who = fmt.Sprintf("%d (@%s)", msg.From.ID, msg.From.Username)
		}
		fmt.Fprintf(b.out, "channel telegram: ignored message from unauthorized user %s\n", who)
		return
	}

	turnCtx := withDelegateDepth(ctx, b.delegateDepth)
	outcome, turnID, err := b.session.runTurn(turnCtx, b.cmd, b.out, msg.Text, false)
	if err != nil {
		fmt.Fprintf(b.out, "channel telegram: turn %s failed: %v\n", turnID, err)
		_ = b.client.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("(the turn failed: %v)", err))
		return
	}
	if err := b.client.SendMessage(ctx, msg.Chat.ID, outcome.Text); err != nil {
		fmt.Fprintf(b.out, "channel telegram: reply to chat %d failed: %v\n", msg.Chat.ID, err)
	}
}

func allowedUser(allow []int64, id int64) bool {
	for _, a := range allow {
		if a == id {
			return true
		}
	}
	return false
}

func loadTelegramOffset(path string) int64 {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func saveTelegramOffset(path string, offset int64) error {
	return os.WriteFile(path, []byte(strconv.FormatInt(offset, 10)), 0o600)
}

// sleepOrDone waits d, or returns false early if ctx is cancelled first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
