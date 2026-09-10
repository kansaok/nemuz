package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingConfigFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	s, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("a config file that has never been written should not error: %v", err)
	}
	if s.Provider() != "" {
		t.Errorf("a fresh Config has a provider set: %+v", s)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	depth := 2
	c := &Config{Agents: Agents{Defaults: AgentDefaults{
		Provider: "anthropic", Model: ModelSel{Primary: "claude-opus-5"},
		AllowExec:     []string{"go", "make"},
		DelegateDepth: &depth,
	}}}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}

	got, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Provider(); p != "anthropic" {
		t.Errorf("provider = %q", p)
	}
	if m, _ := got.Get("model"); m != "claude-opus-5" {
		t.Errorf("model = %q", m)
	}
	if len(got.Agents.Defaults.AllowExec) != 2 || got.Agents.Defaults.AllowExec[0] != "go" {
		t.Errorf("allow-exec is %v", got.Agents.Defaults.AllowExec)
	}
	if got.Agents.Defaults.DelegateDepth == nil || *got.Agents.Defaults.DelegateDepth != 2 {
		t.Errorf("delegate-depth is %v", got.Agents.Defaults.DelegateDepth)
	}
}

// TestSavedConfigIsNestedJSON matters because the whole point of the format
// change is that a person opens ~/.nemuz/config.json and sees the OpenClaw
// layout — agents.defaults, models.providers, channels — without running
// nemuz at all.
func TestSavedConfigIsNestedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	if err := Save(path, &Config{Agents: Agents{Defaults: AgentDefaults{Provider: "anthropic", Model: ModelSel{Primary: "claude-opus-5"}}}}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `"agents"`) || !strings.Contains(text, `"defaults"`) {
		t.Errorf("config is not nested the OpenClaw way:\n%s", text)
	}
	if !strings.Contains(text, `"primary": "claude-opus-5"`) {
		t.Errorf("model should sit under agents.defaults.model.primary:\n%s", text)
	}
}

func TestLoadQuietFallsBackOnACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(path); err == nil {
		t.Fatal("LoadSettings accepted a corrupt file")
	}
	s := LoadSettingsQuiet(path)
	if s.Provider() != "" {
		t.Errorf("quiet load of a corrupt file returned non-empty settings: %+v", s)
	}
}

func TestGetReportsUnknownKeys(t *testing.T) {
	s := &Config{Agents: Agents{Defaults: AgentDefaults{Provider: "anthropic"}}}
	if v, ok := s.Get("provider"); !ok || v != "anthropic" {
		t.Errorf("got %q, %v", v, ok)
	}
	if _, ok := s.Get("does-not-exist"); ok {
		t.Error("an unknown key was reported as known")
	}
	if v, ok := s.Get("model"); !ok || v != "" {
		t.Errorf("an unset known key should be empty and known, got %q, %v", v, ok)
	}
}

func TestSetValidatesEachKeysShape(t *testing.T) {
	s := &Config{}

	for _, k := range []string{"sandbox", "skills", "memories", "review", "curate", "delegate-depth"} {
		if err := s.Set(k, "banana"); err == nil {
			t.Errorf("a nonsense value was accepted for %s", k)
		}
	}

	if err := s.Set("sandbox", "on"); err != nil || s.Sandbox() != "on" {
		t.Errorf("a valid sandbox mode was rejected: %v", err)
	}
	if err := s.Set("skills", "false"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get("skills"); v != "false" {
		t.Errorf("explicitly false skills report %q", v)
	}
	if err := s.Set("allow-exec", "go, make , bash"); err != nil {
		t.Fatal(err)
	}
	if len(s.Agents.Defaults.AllowExec) != 3 || s.Agents.Defaults.AllowExec[1] != "make" {
		t.Errorf("allow-exec parsed as %v", s.Agents.Defaults.AllowExec)
	}
	if err := s.Set("delegate-depth", "2"); err != nil {
		t.Fatal(err)
	}
	if d := s.Agents.Defaults.DelegateDepth; d == nil || *d != 2 {
		t.Errorf("delegate-depth is %v", d)
	}
	if err := s.Set("unknown-key", "x"); err == nil {
		t.Error("an unknown key was accepted by Set")
	}
}

func TestUnsetRevertsToNemuzDefault(t *testing.T) {
	s := &Config{Agents: Agents{Defaults: AgentDefaults{Provider: "anthropic", AllowExec: []string{"go"}}}}
	if err := s.Unset("provider"); err != nil {
		t.Fatal(err)
	}
	if s.Provider() != "" {
		t.Errorf("provider is %q after unset", s.Provider())
	}
	if err := s.Unset("allow-exec"); err != nil {
		t.Fatal(err)
	}
	if s.Agents.Defaults.AllowExec != nil {
		t.Errorf("allow-exec is %v after unset", s.Agents.Defaults.AllowExec)
	}
	if err := s.Unset("unknown-key"); err == nil {
		t.Error("an unknown key was accepted by Unset")
	}
}

// TestBoolSettingsDistinguishUnsetFromFalse is why the boolean settings are
// *bool rather than bool: the flags default to true, so a saved "false" and
// "never configured" must be told apart, or turning skills off could never be
// told from simply not having an opinion yet.
func TestBoolSettingsDistinguishUnsetFromFalse(t *testing.T) {
	s := &Config{}
	if v, _ := s.Get("skills"); v != "" {
		t.Errorf("an unset bool setting reports %q, want empty", v)
	}
	if err := s.Set("skills", "false"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get("skills"); v != "false" {
		t.Errorf("an explicitly false setting reports %q", v)
	}
}

func TestFlatKeysWriteNestedFields(t *testing.T) {
	s := &Config{}
	if err := s.Set("model", "custom-ai-sumopod-com/MiniMax-M2.7-highspeed"); err != nil {
		t.Fatal(err)
	}
	if s.Agents.Defaults.Model.Primary != "custom-ai-sumopod-com/MiniMax-M2.7-highspeed" {
		t.Errorf("model went to %q, want it under agents.defaults.model.primary",
			s.Agents.Defaults.Model.Primary)
	}
}

func TestKeysListsEveryGettableSetting(t *testing.T) {
	s := &Config{}
	for _, k := range Keys {
		if _, ok := s.Get(k); !ok {
			t.Errorf("Keys lists %q but Get does not recognise it", k)
		}
		if err := s.Set(k, s.mustExampleValue(k)); err != nil {
			t.Errorf("Keys lists %q but Set rejects a reasonable value: %v", k, err)
		}
	}
}

func (s *Config) mustExampleValue(key string) string {
	switch key {
	case "sandbox":
		return "auto"
	case "skills", "memories", "review", "curate":
		return "true"
	case "delegate-depth":
		return "2"
	case "workspace", "provider", "model", "review-model", "base-url":
		return "example"
	default:
		return "example"
	}
}

func TestExpandReplacesEnvReferences(t *testing.T) {
	t.Setenv("TEST_NEMUZ_TOKEN", "secret-value")
	c := &Config{
		Channels: Channels{Telegram: &ChannelTelegram{BotToken: "${TEST_NEMUZ_TOKEN}"}},
		Gateway:  Gateway{Auth: GatewayAuth{Token: "${TEST_NEMUZ_TOKEN}"}},
	}
	e := c.Expand()
	if e.Channels.Telegram.BotToken != "secret-value" {
		t.Errorf("botToken = %q", e.Channels.Telegram.BotToken)
	}
	if e.Gateway.Auth.Token != "secret-value" {
		t.Errorf("gateway token = %q", e.Gateway.Auth.Token)
	}
	// The original must be untouched, so `nemuz config` never shows a secret.
	if c.Channels.Telegram.BotToken != "${TEST_NEMUZ_TOKEN}" {
		t.Error("Expand mutated the original config")
	}
}

func TestExpandLeavesPlainValuesAlone(t *testing.T) {
	c := &Config{Agents: Agents{Defaults: AgentDefaults{Model: ModelSel{Primary: "claude-opus-5"}}}}
	if e := c.Expand(); e.Provider() != "" || e.Agents.Defaults.Model.Primary != "claude-opus-5" {
		t.Errorf("Expand disturbed non-secret values: %+v", e)
	}
}

func TestLoadDotEnvExportsKeptSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, EnvFileName)
	if err := os.WriteFile(path, []byte("# comment\nEXPORTED=from-file\nKEPT=kept\nQUOTED=\"with spaces\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXPORTED", "already-set")

	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("KEPT") != "kept" {
		t.Errorf("KEPT = %q", os.Getenv("KEPT"))
	}
	if os.Getenv("QUOTED") != "with spaces" {
		t.Errorf("QUOTED = %q", os.Getenv("QUOTED"))
	}
	// An explicitly exported variable always wins over the file.
	if os.Getenv("EXPORTED") != "already-set" {
		t.Errorf("EXPORTED = %q, want the already-set value", os.Getenv("EXPORTED"))
	}
}

func TestLoadDotEnvMissingFileIsNotAnError(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "nope.env")); err != nil {
		t.Fatal(err)
	}
}

func TestSetEnvWritesOnlyItsOwnLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), EnvFileName)
	if err := os.WriteFile(path, []byte("# comment\nexport ALREADY=kept\nKEPT=v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetEnv(path, "KEPT", "v2"); err != nil {
		t.Fatal(err)
	}
	if err := SetEnv(path, "NEW", "added"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "# comment\n") || !strings.Contains(got, "export ALREADY=kept\n") {
		t.Errorf("unrelated lines were disturbed:\n%s", got)
	}
	if !strings.Contains(got, "KEPT=v2\n") {
		t.Errorf("KEPT was not replaced:\n%s", got)
	}
	if !strings.Contains(got, "NEW=added\n") {
		t.Errorf("NEW was not appended:\n%s", got)
	}
	// Exactly one definition of each key survives.
	for _, key := range []string{"KEPT", "ALREADY", "NEW"} {
		if want := 1; strings.Count(got, key+"=") != want {
			t.Errorf("%s defined %d times:\n%s", key, strings.Count(got, key+"="), got)
		}
	}
	// The round-tripped file still loads.
	t.Setenv("KEPT", "")
	t.Setenv("NEW", "")
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("KEPT") != "v2" || os.Getenv("NEW") != "added" {
		t.Errorf("reload: KEPT=%q NEW=%q", os.Getenv("KEPT"), os.Getenv("NEW"))
	}
	// Owner-only permissions, even for a freshly created file.
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestUnsetEnvRemovesEveryDefinition(t *testing.T) {
	path := filepath.Join(t.TempDir(), EnvFileName)
	if err := os.WriteFile(path, []byte("DUPE=a\nKEEP=b\nDUPE=c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UnsetEnv(path, "DUPE"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "KEEP=b\n" {
		t.Errorf("after unset:\n%q", got)
	}
}

func TestEnvNamesListsKeysWithoutValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), EnvFileName)
	if err := os.WriteFile(path, []byte("# c\nA=one\nB=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := EnvNames(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "A" || names[1] != "B" {
		t.Errorf("names = %v", names)
	}
	if _, err := EnvNames(filepath.Join(t.TempDir(), "missing.env")); err != nil {
		t.Errorf("missing file should be an empty list, got %v", err)
	}
}

func TestSetEnvRejectsBadNames(t *testing.T) {
	if err := SetEnv(filepath.Join(t.TempDir(), EnvFileName), "bad name!", "v"); err == nil {
		t.Fatal("a name with spaces was accepted")
	}
	if err := SetEnv(filepath.Join(t.TempDir(), EnvFileName), "1X", "v"); err == nil {
		t.Fatal("a name starting with a digit was accepted")
	}
}

func TestMigratesLegacyYAMLOnFirstLoad(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "config.yaml")
	jsonPath := filepath.Join(dir, ConfigFileName)
	body := "provider: anthropic\nmodel: claude-opus-5\nsandbox: on\nallow_exec:\n  - go\n"
	if err := os.WriteFile(legacy, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadSettings(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider() != "anthropic" {
		t.Errorf("migrated provider = %q", cfg.Provider())
	}
	if m, _ := cfg.Get("model"); m != "claude-opus-5" {
		t.Errorf("migrated model = %q", m)
	}
	if len(cfg.Agents.Defaults.AllowExec) != 1 || cfg.Agents.Defaults.AllowExec[0] != "go" {
		t.Errorf("migrated allow-exec = %v", cfg.Agents.Defaults.AllowExec)
	}
	// The old file is renamed aside, so the migration happened exactly once.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy config.yaml should have been renamed aside, stat err = %v", err)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Errorf("config.json was not written: %v", err)
	}
}

func TestEmptyLegacyYAMLIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(legacy, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadSettings(filepath.Join(dir, ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider() != "" {
		t.Error("an empty legacy file migrated something")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("an empty legacy file should be left alone: %v", err)
	}
}

func TestTelegramAllowUsersFallsBackToOwnerList(t *testing.T) {
	c := &Config{Commands: Commands{OwnerAllowFrom: []string{"slack:U123", "telegram:7361357941", "telegram:42"}}}
	got := c.TelegramAllowUsers()
	if len(got) != 2 || got[0] != 7361357941 || got[1] != 42 {
		t.Errorf("allow users = %v", got)
	}

	c = &Config{Channels: Channels{Telegram: &ChannelTelegram{AllowUsers: []int64{7}}}}
	got = c.TelegramAllowUsers()
	if len(got) != 1 || got[0] != 7 {
		t.Errorf("explicit allowUsers should win, got %v", got)
	}
}
