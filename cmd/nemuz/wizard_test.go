package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kansaok/nemuz/internal/cli"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/telegram"
)

// wizardTestIO drives the wizard from a script, as piped input would.
func wizardTestIO(answers string) (*cli.IO, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &cli.IO{In: strings.NewReader(answers), Out: buf}, buf
}

func loadWizardConfig(t *testing.T, dir string) *config.Config {
	t.Helper()
	s, err := config.LoadSettings(config.At(dir).Config)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWizardSetsUpALocalPresetWithoutAKey(t *testing.T) {
	dir := t.TempDir()
	io, buf := wizardTestIO("1\n8\n1\nllama3.2\n3\n")
	if err := configWizard(io, p(dir), &config.Config{}); err != nil {
		t.Fatal(err)
	}
	s := loadWizardConfig(t, dir)
	if s.Agents.Defaults.Provider != "ollama" {
		t.Errorf("provider = %q, want ollama", s.Agents.Defaults.Provider)
	}
	if s.Agents.Defaults.Model.Primary != "ollama/llama3.2" {
		t.Errorf("primary = %q", s.Agents.Defaults.Model.Primary)
	}
	if !strings.Contains(buf.String(), "model saved: ollama/llama3.2") {
		t.Errorf("no confirmation printed:\n%s", buf.String())
	}
}

func TestWizardSetsUpACustomProviderWithALiveModelList(t *testing.T) {
	dir := t.TempDir()
	old := wizardFetchModels
	wizardFetchModels = func(_ context.Context, wire, base string, key string) ([]string, error) {
		if wire != "openai-completions" {
			t.Errorf("wire = %q", wire)
		}
		if base != "https://ai.corpo.internal/v1" {
			t.Errorf("base = %q", base)
		}
		return []string{"engine-v1", "engine-v2"}, nil
	}
	defer func() { wizardFetchModels = old }()

	io, buf := wizardTestIO("1\n14\nhttps://ai.corpo.internal/v1\nsk-x\n2\n3\n")
	if err := configWizard(io, p(dir), &config.Config{}); err != nil {
		t.Fatal(err)
	}
	s := loadWizardConfig(t, dir)
	prov, ok := s.Models.Providers["ai-corpo-internal"]
	if !ok {
		t.Fatal("no provider entry saved under ai-corpo-internal")
	}
	if prov.BaseURL != "https://ai.corpo.internal/v1" || prov.API != "openai-completions" {
		t.Errorf("provider def = %+v", prov)
	}
	if len(prov.Models) != 1 || prov.Models[0].ID != "engine-v1" {
		t.Errorf("models = %+v", prov.Models)
	}
	if s.Agents.Defaults.Model.Primary != "ai-corpo-internal/engine-v1" {
		t.Errorf("primary = %q", s.Agents.Defaults.Model.Primary)
	}
	names, err := config.EnvNames(config.At(dir).Env)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "AI_CORPO_INTERNAL_API_KEY" {
		t.Errorf("env names = %v", names)
	}
	if !strings.Contains(buf.String(), "model saved: ai-corpo-internal/engine-v1") {
		t.Errorf("no confirmation printed:\n%s", buf.String())
	}
}

func TestWizardRetriesABadPresetKey(t *testing.T) {
	dir := t.TempDir()
	old := wizardFetchModels
	wizardFetchModels = func(_ context.Context, wire, base, key string) ([]string, error) {
		if key == "bad" {
			return nil, errBadKey
		}
		return []string{"gpt-4o"}, nil
	}
	defer func() { wizardFetchModels = old }()

	// provider openai is #9 in the sorted preset list
	io, _ := wizardTestIO("1\n9\nbad\ngood\n2\n3\n")
	if err := configWizard(io, p(dir), &config.Config{}); err != nil {
		t.Fatal(err)
	}
	s := loadWizardConfig(t, dir)
	if s.Agents.Defaults.Model.Primary != "openai/gpt-4o" {
		t.Errorf("primary = %q", s.Agents.Defaults.Model.Primary)
	}
}

func TestWizardChannelPairingSavesTokenOnly(t *testing.T) {
	dir := t.TempDir()
	old := wizardBotCheck
	wizardBotCheck = func(_ context.Context, token string) (telegram.User, error) {
		if token != "123:tok" {
			t.Errorf("token = %q", token)
		}
		return telegram.User{Username: "nemuz_bot"}, nil
	}
	defer func() { wizardBotCheck = old }()

	io, _ := wizardTestIO("2\n1\n123:tok\n1\n3\n")
	if err := configWizard(io, p(dir), &config.Config{}); err != nil {
		t.Fatal(err)
	}
	s := loadWizardConfig(t, dir)
	if s.Channels.Telegram == nil || s.Channels.Telegram.BotToken != "${TELEGRAM_BOT_TOKEN}" {
		t.Fatalf("botToken = %+v", s.Channels.Telegram)
	}
	if len(s.Channels.Telegram.AllowUsers) != 0 {
		t.Errorf("allowUsers = %v, want none in pairing mode", s.Channels.Telegram.AllowUsers)
	}
	if names, _ := config.EnvNames(config.At(dir).Env); len(names) != 1 || names[0] != "TELEGRAM_BOT_TOKEN" {
		t.Errorf("env names = %v", names)
	}
}

func TestWizardChannelManualAddsOwnerEntries(t *testing.T) {
	dir := t.TempDir()
	old := wizardBotCheck
	wizardBotCheck = func(_ context.Context, token string) (telegram.User, error) {
		return telegram.User{Username: "nemuz_bot"}, nil
	}
	defer func() { wizardBotCheck = old }()

	io, _ := wizardTestIO("2\n1\n123:tok\n2\n111,222\n3\n")
	if err := configWizard(io, p(dir), &config.Config{}); err != nil {
		t.Fatal(err)
	}
	s := loadWizardConfig(t, dir)
	if s.Channels.Telegram == nil {
		t.Fatal("no telegram channel")
	}
	if len(s.Channels.Telegram.AllowUsers) != 2 || s.Channels.Telegram.AllowUsers[0] != 111 {
		t.Errorf("allowUsers = %v", s.Channels.Telegram.AllowUsers)
	}
	want := []string{"telegram:111", "telegram:222"}
	if len(s.Commands.OwnerAllowFrom) != 2 || s.Commands.OwnerAllowFrom[0] != want[0] || s.Commands.OwnerAllowFrom[1] != want[1] {
		t.Errorf("ownerAllowFrom = %v", s.Commands.OwnerAllowFrom)
	}
}

func p(dir string) *config.Paths {
	paths := config.At(dir)
	return &paths
}
