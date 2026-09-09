package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/spf13/cobra"
)

func replayCmd() *cobra.Command {
	var workspace string
	var keep bool
	var pluginCmds []string
	var allowNet []string
	var allowExec []string
	var sandboxMode string

	c := &cobra.Command{
		Use:   "replay <turn>",
		Short: "Run a recorded turn again and check it matches",
		Long: "Re-runs a recorded turn with its real tools, drawing model responses\n" +
			"from the recording instead of calling a provider. The replayed events\n" +
			"are compared against the original, and the first difference is\n" +
			"reported with the event that caused it.\n\n" +
			"A turn that replays cleanly is reproducible. One that does not has\n" +
			"found a real difference — in the code, the tools, or the workspace.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			return runReplay(cmd, cmdArgs[0], replayOptions{
				workspace:  workspace,
				keep:       keep,
				pluginCmds: pluginCmds,
				allowNet:   allowNet,
				allowExec:  allowExec,
				sandbox:    sandboxMode,
			})
		},
	}
	c.Flags().StringVarP(&workspace, "workspace", "w", ".", "workspace the tools operate on")
	c.Flags().BoolVar(&keep, "keep", false, "keep the replay's own journal instead of deleting it")
	c.Flags().StringArrayVar(&pluginCmds, "plugin", nil, "plugin command the recorded turn used; repeatable")
	c.Flags().StringArrayVar(&allowNet, "allow-net", nil, "network destination a plugin may reach; repeatable")
	c.Flags().StringArrayVar(&allowExec, "allow-exec", nil, "program a plugin may run; repeatable")
	c.Flags().StringVar(&sandboxMode, "sandbox", string(SandboxAuto), "confine the built-in tools: on, auto, or off")
	return c
}

// replayOptions is what a replay needs beyond the recording itself.
//
// Plugins are not reconstructed from the journal: a recorded turn names the
// tools it used, but starting arbitrary processes because a file said so would
// be a poor idea. The operator states which plugins to load, and the digest
// check confirms they behaved the same way.
type replayOptions struct {
	workspace  string
	keep       bool
	pluginCmds []string
	allowNet   []string
	allowExec  []string
	sandbox    string
}

// turnStart is the subset of the opening event a replay needs to rebuild the run.
type turnStart struct {
	Prompt string            `json:"prompt"`
	Model  string            `json:"model"`
	System string            `json:"system"`
	Env    map[string]string `json:"env,omitempty"`
}

func runReplay(cmd *cobra.Command, turnRef string, opts replayOptions) error {
	workspace, keep := opts.workspace, opts.keep
	paths, err := config.Resolve()
	if err != nil {
		return err
	}
	bs, err := blob.Open(paths.Blobs)
	if err != nil {
		return err
	}
	turn, err := journal.Find(paths.Journal, turnRef)
	if err != nil {
		return err
	}
	recorded, err := journal.Read(turn.Path)
	if err != nil {
		return err
	}

	start, err := openingEvent(recorded, bs)
	if err != nil {
		return err
	}
	cassette, err := journal.CassetteFrom(recorded, bs)
	if err != nil {
		return err
	}

	settings := config.LoadSettingsQuiet(paths.Config)
	applyStringDefault(cmd, "sandbox", &opts.sandbox, "sandbox", settings)
	applyListDefault(cmd, "allow-exec", &opts.allowExec, "allow-exec", settings)
	applyListDefault(cmd, "allow-net", &opts.allowNet, "allow-net", settings)

	ts, err := buildToolset(cmd.Context(), toolsetOptions{
		Workspace:  workspace,
		Sandbox:    SandboxMode(opts.sandbox),
		PluginCmds: opts.pluginCmds,
		AllowNet:   opts.allowNet,
		AllowExec:  opts.allowExec,
	})
	if err != nil {
		return err
	}
	defer ts.Close()

	replayID, err := journal.NewTurnID()
	if err != nil {
		return err
	}
	w, err := journal.Create(paths.Journal, replayID, bs)
	if err != nil {
		return err
	}
	defer w.Close()

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "replaying %s\n  workspace %s\n  model     %s\n  sandbox   %s\n  calls     %d\n\n",
		turn.ID, ts.Workspace, start.Model, ts.Sandbox, cassette.Len())

	a := &agent.Agent{
		Provider:    llm.Replay(cassette, w),
		Tools:       ts.Registry,
		Journal:     w,
		Model:       start.Model,
		System:      start.System,
		Environment: start.Env,
	}
	runErr := func() error {
		_, err := a.Run(context.Background(), start.Prompt)
		return err
	}()
	if err := w.Close(); err != nil {
		return err
	}

	replayed, readErr := journal.Read(w.Path())
	if readErr != nil {
		return readErr
	}
	if !keep {
		defer removeQuietly(w.Path())
	}

	if runErr != nil {
		fmt.Fprintf(out, "DIVERGED  %v\n", runErr)
		return fmt.Errorf("turn %s is not reproducible", turn.ID)
	}
	if err := journal.Verify(recorded, replayed); err != nil {
		fmt.Fprintf(out, "DIVERGED  %v\n", err)
		return fmt.Errorf("turn %s is not reproducible", turn.ID)
	}

	fmt.Fprintf(out, "  ok  %d events reproduced exactly\n      digest %s\n",
		len(replayed), journal.Digest(replayed))
	if keep {
		fmt.Fprintf(out, "      replay journal kept as %s\n", replayID)
	}
	return nil
}

func openingEvent(events []journal.Event, bs *blob.Store) (turnStart, error) {
	for _, e := range events {
		if e.Kind != journal.KindTurnStart {
			continue
		}
		body, err := e.Content(bs)
		if err != nil {
			return turnStart{}, err
		}
		var start turnStart
		if err := json.Unmarshal(body, &start); err != nil {
			return turnStart{}, fmt.Errorf("decode turn.start: %w", err)
		}
		if start.Model == "" {
			return turnStart{}, fmt.Errorf("the recording does not name a model")
		}
		return start, nil
	}
	return turnStart{}, fmt.Errorf("the recording has no turn.start event")
}

// removeQuietly deletes a replay's scratch journal. A failure to clean up is
// not worth failing the command over — the operator already has their answer.
func removeQuietly(path string) {
	_ = os.Remove(path)
}
