package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestConfigCmdHasAWizardFlag(t *testing.T) {
	cmd := configCmd()
	f := cmd.Flags().Lookup("wizard")
	if f == nil {
		t.Fatal("config has no --wizard flag")
	}
	if f.Shorthand != "w" {
		t.Errorf("--wizard shorthand = %q, want w", f.Shorthand)
	}
}

func TestConfigListPointsToTheWizard(t *testing.T) {
	paths := config.At(t.TempDir())
	cmd := &cobra.Command{}
	cmd.Flags().AddFlagSet(&pflag.FlagSet{})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetArgs(nil)

	if err := configList(cmd, paths); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--wizard") {
		t.Errorf("the table should point at the interactive setup, got:\n%s", out.String())
	}
}

func TestConfigListShowsSavedDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := config.At(dir)
	s := &config.Config{}
	s.Set("provider", "anthropic")
	s.Set("model", "claude-opus-5")
	if err := config.Save(cfg.Config, s); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	if err := configList(cmd, cfg); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"provider", "anthropic", "model", "claude-opus-5"} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q:\n%s", want, got)
		}
	}
}

func TestApplyProviderModelDefaultsSplitsOpenClawPrimary(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	_ = provider.RegisterCustom(map[string]provider.Custom{
		"custom-ai-foo-com": {API: "openai-completions", BaseURL: "http://127.0.0.1:9999/v1"},
	})

	cmd := &cobra.Command{}
	cmd.Flags().String("provider", "anthropic", "")
	cmd.Flags().String("model", "", "")
	settings := &config.Config{Agents: config.Agents{Defaults: config.AgentDefaults{
		Model: config.ModelSel{Primary: "custom-ai-foo-com/agent-v1"},
	}}}

	var providerName = "anthropic"
	var model = ""
	applyProviderModelDefaults(cmd, settings, &providerName, &model)

	if providerName != "custom-ai-foo-com" {
		t.Errorf("provider = %q, want the head of the primary", providerName)
	}
	if model != "agent-v1" {
		t.Errorf("model = %q, want the tail of the primary", model)
	}
}

func TestApplyProviderModelDefaultsLeavesPlainModelWhole(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("provider", "anthropic", "")
	cmd.Flags().String("model", "", "")
	settings := &config.Config{Agents: config.Agents{Defaults: config.AgentDefaults{
		Model: config.ModelSel{Primary: "ft:gpt-4o:org"},
	}}}

	var providerName = "anthropic"
	var model = ""
	applyProviderModelDefaults(cmd, settings, &providerName, &model)
	if providerName != "anthropic" || model != "ft:gpt-4o:org" {
		t.Errorf("provider/model = %q/%q, want unchanged", providerName, model)
	}
}

func TestApplyProviderModelDefaultsExplicitFlagWins(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("provider", "anthropic", "")
	cmd.Flags().String("model", "", "")
	if err := cmd.Flags().Set("provider", "openai"); err != nil {
		t.Fatal(err)
	}
	settings := &config.Config{Agents: config.Agents{Defaults: config.AgentDefaults{
		Model: config.ModelSel{Primary: "custom-ai-foo-com/agent-v1"},
	}}}

	var providerName = "openai"
	var model = ""
	applyProviderModelDefaults(cmd, settings, &providerName, &model)
	if providerName != "openai" {
		t.Errorf("an explicit --provider was overridden: %q", providerName)
	}
}

func TestApplyPluginDefaultFromConfig(t *testing.T) {
	enabled := true
	disabled := false
	settings := &config.Config{Plugins: config.Plugins{Entries: map[string]config.PluginEntry{
		"plugin-b": {Enabled: &enabled},
		"plugin-a": {},
		"plugin-c": {Enabled: &disabled},
	}}}

	var got []string
	applyPluginDefault(&cobra.Command{}, &got, settings)
	if len(got) != 2 || got[0] != "plugin-a" || got[1] != "plugin-b" {
		t.Errorf("plugin defaults = %v, want the enabled entries in sorted order", got)
	}
}

func TestApplyPluginDefaultExplicitFlagWins(t *testing.T) {
	var got []string
	cmd := &cobra.Command{}
	cmd.Flags().StringArrayVar(&got, "plugin", nil, "")
	if err := cmd.Flags().Set("plugin", "only-this"); err != nil {
		t.Fatal(err)
	}
	settings := &config.Config{Plugins: config.Plugins{Entries: map[string]config.PluginEntry{
		"plugin-a": {},
	}}}

	applyPluginDefault(cmd, &got, settings)
	if len(got) != 1 || got[0] != "only-this" {
		t.Errorf("plugin defaults overrode --plugin: %v", got)
	}
}
