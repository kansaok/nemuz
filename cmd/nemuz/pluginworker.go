package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// pluginWorkerCmd is a private launcher that confines a third-party plugin
// before its first instruction executes. Capability manifests arrive only
// after a plugin starts, so they cannot safely be used as a pre-start security
// decision; this worker applies the operator's baseline policy instead.
func pluginWorkerCmd() *cobra.Command {
	var workspace, argvJSON string
	var allowNet, sandboxed bool
	c := &cobra.Command{
		Use:    "plugin-worker",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var argv []string
			if err := json.Unmarshal([]byte(argvJSON), &argv); err != nil || len(argv) == 0 {
				return fmt.Errorf("plugin-worker: --argv-json must contain a non-empty JSON argument list")
			}
			return runPluginWorker(workspace, argv, allowNet, sandboxed)
		},
	}
	c.Flags().StringVar(&workspace, "workspace", "", "workspace the plugin may access")
	c.Flags().StringVar(&argvJSON, "argv-json", "", "encoded plugin argv")
	c.Flags().BoolVar(&allowNet, "allow-net", false, "allow plugin socket operations")
	c.Flags().BoolVar(&sandboxed, "sandbox", true, "require kernel confinement")
	_ = c.MarkFlagRequired("workspace")
	_ = c.MarkFlagRequired("argv-json")
	return c
}
