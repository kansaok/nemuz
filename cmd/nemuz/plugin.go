package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/kansaok/nemuz/internal/plugin"
	"github.com/kansaok/nemuz/internal/tool"
	"github.com/spf13/cobra"
)

func pluginCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "plugin",
		Short: "Inspect and exercise tool plugins",
		Long: "A plugin is any process that speaks nemuz's JSON-RPC protocol on\n" +
			"stdio. These commands start one directly, so it can be checked before\n" +
			"a model is ever given access to it.",
	}
	c.AddCommand(pluginInspectCmd(), pluginCallCmd())
	return c
}

func pluginInspectCmd() *cobra.Command {
	var workspace string
	var allowNet []string
	var allowExec []string
	c := &cobra.Command{
		Use:   "inspect <command>",
		Short: "Start a plugin and show what it offers, and what it would be granted",
		Long: "Shows each tool the plugin declares, what it asked for, and what the\n" +
			"host would actually grant. A tool the policy refuses is marked, with\n" +
			"the reason, so a plugin can be fixed before it is trusted.",
		Args:    cobra.ExactArgs(1),
		Example: `  nemuz plugin inspect "node ./examples/plugin-ts/plugin.mjs"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, ws, err := startPlugin(cmd.Context(), args[0], workspace)
			if err != nil {
				return err
			}
			defer client.Close()

			out := cmd.OutOrStdout()
			m := client.Manifest()
			fmt.Fprintf(out, "%s %s\n  protocol %d · workspace %s\n\n", m.Name, m.Version, m.Protocol, ws)

			policy := plugin.WorkspacePolicy{Workspace: ws, AllowNet: allowNet, AllowExec: allowExec}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TOOL\tASKED FOR\tGRANTED")
			for _, spec := range m.Tools {
				granted, err := policy.Review(m.Name, spec)
				status := describeCaps(granted)
				if err != nil {
					status = "REFUSED — " + trimPrefix(err.Error(), m.Name)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", spec.Name, describeSpec(spec.Capabilities), status)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out, "\nTool names are namespaced as %s__<tool> when registered.\n", m.Name)
			return nil
		},
	}
	c.Flags().StringVarP(&workspace, "workspace", "w", ".", "workspace to offer the plugin")
	c.Flags().StringArrayVar(&allowNet, "allow-net", nil, "network destination to approve; repeatable")
	c.Flags().StringArrayVar(&allowExec, "allow-exec", nil, "program to approve; repeatable")
	return c
}

func pluginCallCmd() *cobra.Command {
	var workspace string
	var allowNet []string
	var allowExec []string
	c := &cobra.Command{
		Use:   "call <command> <tool> [json-arguments]",
		Short: "Run one of a plugin's tools directly",
		Long: "Calls a tool without involving a model, which is the fastest way to\n" +
			"develop one. Arguments default to {}.",
		Args:    cobra.RangeArgs(2, 3),
		Example: `  nemuz plugin call "node ./examples/plugin-ts/plugin.mjs" shout '{"text":"halo"}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := json.RawMessage(`{}`)
			if len(args) == 3 {
				if !json.Valid([]byte(args[2])) {
					return fmt.Errorf("arguments are not valid JSON: %s", args[2])
				}
				payload = json.RawMessage(args[2])
			}

			client, ws, err := startPlugin(cmd.Context(), args[0], workspace)
			if err != nil {
				return err
			}
			defer client.Close()

			// Reviewed before calling, so this command exercises exactly what a
			// turn would — a tool the host would refuse must be refused here.
			policy := plugin.WorkspacePolicy{Workspace: ws, AllowNet: allowNet, AllowExec: allowExec}
			if _, err := client.Tools(policy); err != nil {
				return err
			}

			result, err := client.Call(cmd.Context(), args[1], payload)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if result.IsError {
				fmt.Fprintf(out, "tool failed: %s\n", result.Content)
				return fmt.Errorf("%s reported a failure", args[1])
			}
			fmt.Fprintln(out, result.Content)
			return nil
		},
	}
	c.Flags().StringVarP(&workspace, "workspace", "w", ".", "workspace to offer the plugin")
	c.Flags().StringArrayVar(&allowNet, "allow-net", nil, "network destination to approve; repeatable")
	c.Flags().StringArrayVar(&allowExec, "allow-exec", nil, "program to approve; repeatable")
	return c
}

// startPlugin launches a plugin from a command line and returns it with the
// resolved workspace.
func startPlugin(ctx context.Context, command, workspace string) (*plugin.Client, string, error) {
	argv, err := splitCommand(command)
	if err != nil {
		return nil, "", err
	}
	ws, err := plugin.CleanWorkspace(workspace)
	if err != nil {
		return nil, "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := plugin.Start(ctx, plugin.Options{
		Command:     argv,
		Workspace:   ws,
		HostVersion: Version,
	})
	if err != nil {
		return nil, "", err
	}
	return client, ws, nil
}

// splitCommand splits a command line on spaces, honouring single and double
// quotes so paths with spaces survive.
func splitCommand(s string) ([]string, error) {
	var argv []string
	var current strings.Builder
	var quote rune
	inWord := false

	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				argv = append(argv, current.String())
				current.Reset()
				inWord = false
			}
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced %c in command: %s", quote, s)
	}
	if inWord {
		argv = append(argv, current.String())
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty plugin command")
	}
	return argv, nil
}

func describeSpec(c plugin.CapabilitySpec) string {
	return describeCaps(tool.Capabilities{FSRead: c.FSRead, FSWrite: c.FSWrite, Net: c.Net, Exec: c.Exec})
}

func describeCaps(c tool.Capabilities) string {
	if c.IsZero() {
		return "nothing"
	}
	var parts []string
	if len(c.FSRead) > 0 {
		parts = append(parts, "read "+strings.Join(c.FSRead, ","))
	}
	if len(c.FSWrite) > 0 {
		parts = append(parts, "write "+strings.Join(c.FSWrite, ","))
	}
	if len(c.Net) > 0 {
		parts = append(parts, "net "+strings.Join(c.Net, ","))
	}
	if len(c.Exec) > 0 {
		parts = append(parts, "exec "+strings.Join(c.Exec, ","))
	}
	return strings.Join(parts, " · ")
}

func trimPrefix(msg, pluginName string) string {
	return strings.TrimPrefix(msg, "plugin "+pluginName+": ")
}
