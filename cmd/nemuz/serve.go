package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kansaok/nemuz"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/httpapi"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/kansaok/nemuz/internal/metrics"
	"github.com/spf13/cobra"
)

// DefaultAPIPort is where the HTTP API listens.
const DefaultAPIPort = "8642"

// apiKeyEnv holds the key clients must present.
const apiKeyEnv = "NEMUZ_API_KEY"

func serveCmd() *cobra.Command {
	var (
		addr         string
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
		withMetrics  bool
	)

	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the agent over an OpenAI-compatible HTTP API",
		Long: "Any client that already talks to OpenAI can talk to this instead.\n" +
			"Point its base URL at http://127.0.0.1:" + DefaultAPIPort + "/v1.\n\n" +
			"Every request becomes a recorded turn, and the completion id it\n" +
			"returns is the turn id — so an answer that looks wrong can be handed\n" +
			"straight to `nemuz replay`.\n\n" +
			"The server binds to loopback by default. Listening anywhere else\n" +
			"without " + apiKeyEnv + " set is refused: an agent endpoint with no\n" +
			"key is a remote shell with extra steps.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			settings := config.LoadSettingsQuiet(paths.Config)
			applyStringDefault(cmd, "provider", &providerName, "provider", settings)
			applyStringDefault(cmd, "model", &model, "model", settings)
			applyStringDefault(cmd, "base-url", &baseURL, "base-url", settings)
			applyStringDefault(cmd, "sandbox", &sandboxMode, "sandbox", settings)
			applyListDefault(cmd, "allow-exec", &allowExec, "allow-exec", settings)
			applyListDefault(cmd, "allow-net", &allowNet, "allow-net", settings)
			applyBoolDefault(cmd, "skills", &useSkills, "skills", settings)
			applyBoolDefault(cmd, "memories", &useMemories, "memories", settings)

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

			// The server runs on the same public API a third party would use,
			// which is the most convincing check that the API is complete.
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

			var recorder *metrics.Metrics
			if withMetrics {
				recorder = metrics.New()
			}
			server, err := httpapi.NewServer(httpapi.Options{
				Runner:  agent,
				Model:   model,
				APIKey:  os.Getenv(apiKeyEnv),
				Addr:    addr,
				Metrics: recorder,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "nemuz %s listening on http://%s\n", Version, addr)
			fmt.Fprintf(out, "  model     %s via %s\n", model, p.Name())
			fmt.Fprintf(out, "  workspace %s\n", ts.Workspace)
			fmt.Fprintf(out, "  sandbox   %s\n", ts.Sandbox)
			if withMetrics {
				fmt.Fprintf(out, "  metrics   http://%s/metrics\n", addr)
			}
			if os.Getenv(apiKeyEnv) == "" {
				fmt.Fprintf(out, "  auth      none — loopback only. Set %s to require a key.\n", apiKeyEnv)
			} else {
				fmt.Fprintf(out, "  auth      %s\n", apiKeyEnv)
			}
			fmt.Fprintf(out, "\nTry it:\n  curl -s http://%s/v1/chat/completions \\\n"+
				"    -H 'Content-Type: application/json' \\\n"+
				"    -d '{\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'\n\n", addr)

			return listen(cmd.Context(), out, addr, server.Handler())
		},
	}

	c.Flags().StringVar(&addr, "addr", "127.0.0.1:"+DefaultAPIPort, "address to listen on")
	c.Flags().StringVarP(&providerName, "provider", "p", "anthropic", "provider to call; see `nemuz providers`")
	c.Flags().StringVarP(&model, "model", "m", "", "model id (defaults to the provider's own default)")
	c.Flags().StringVar(&baseURL, "base-url", "", "override the provider endpoint")
	c.Flags().StringVarP(&workspace, "workspace", "w", ".", "workspace the tools operate on")
	c.Flags().StringVar(&system, "system", defaultSystemPrompt, "system prompt")
	c.Flags().StringVar(&sandboxMode, "sandbox", string(SandboxAuto), "confine the built-in tools: on, auto, or off")
	c.Flags().StringArrayVar(&pluginCmds, "plugin", nil, "plugin command to load; repeatable")
	c.Flags().StringArrayVar(&allowNet, "allow-net", nil, "network destination a plugin may reach; repeatable")
	c.Flags().StringArrayVar(&allowExec, "allow-exec", nil, "program a plugin may run; repeatable")
	c.Flags().BoolVar(&useSkills, "skills", true, "include active learned skills in the system prompt")
	c.Flags().BoolVar(&useMemories, "memories", true, "recall relevant memories into the system prompt")
	c.Flags().BoolVar(&withMetrics, "metrics", true, "serve Prometheus metrics at /metrics")
	return c
}

// listen serves until interrupted, then drains in-flight requests.
//
// A turn can take minutes, so an abrupt exit would abandon work the caller is
// still waiting on — and, worse, leave its journal unclosed.
func listen(ctx context.Context, out interface{ Write([]byte) (int, error) }, addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		fmt.Fprintln(out, "\nshutting down; finishing any turn already in flight")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}
