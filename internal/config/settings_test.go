package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingConfigFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	s, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("a config file that has never been written should not error: %v", err)
	}
	if s.Provider != "" {
		t.Errorf("a fresh Settings has a provider set: %+v", s)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	s := &Settings{Provider: "anthropic", Model: "claude-opus-5", AllowExec: []string{"go", "make"}}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "anthropic" || got.Model != "claude-opus-5" {
		t.Errorf("got %+v", got)
	}
	if len(got.AllowExec) != 2 || got.AllowExec[0] != "go" {
		t.Errorf("allow-exec is %v", got.AllowExec)
	}
}

// TestSavedConfigIsHumanReadable matters for the same reason skills and
// memories are Markdown: a person should be able to open ~/.nemuz/config.yaml
// in an editor and understand it without running nemuz at all.
func TestSavedConfigIsHumanReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, &Settings{Provider: "anthropic", Model: "claude-opus-5"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "provider: anthropic") {
		t.Errorf("config is not plain readable YAML:\n%s", text)
	}
}

func TestLoadQuietFallsBackOnACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{not: valid: yaml: at: all"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The sharp-edged loader must say so.
	if _, err := LoadSettings(path); err == nil {
		t.Fatal("LoadSettings accepted a corrupt file")
	}
	// The quiet loader, used by every command's flag defaulting, must not —
	// a broken config file cannot be allowed to break every other command.
	s := LoadSettingsQuiet(path)
	if s.Provider != "" {
		t.Errorf("quiet load of a corrupt file returned non-empty settings: %+v", s)
	}
}

func TestGetReportsUnknownKeys(t *testing.T) {
	s := &Settings{Provider: "anthropic"}
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
	s := &Settings{}

	if err := s.Set("sandbox", "sometimes"); err == nil {
		t.Error("an invalid sandbox mode was accepted")
	}
	if err := s.Set("sandbox", "on"); err != nil || s.Sandbox != "on" {
		t.Errorf("a valid sandbox mode was rejected: %v", err)
	}

	if err := s.Set("skills", "maybe"); err == nil {
		t.Error("a non-boolean value was accepted for skills")
	}
	if err := s.Set("skills", "false"); err != nil {
		t.Fatal(err)
	}
	if s.Skills == nil || *s.Skills != false {
		t.Errorf("skills is %v", s.Skills)
	}

	if err := s.Set("allow-exec", "go, make , bash"); err != nil {
		t.Fatal(err)
	}
	if len(s.AllowExec) != 3 || s.AllowExec[1] != "make" {
		t.Errorf("allow-exec parsed as %v", s.AllowExec)
	}

	if err := s.Set("delegate-depth", "not-a-number"); err == nil {
		t.Error("a non-integer value was accepted for delegate-depth")
	}
	if err := s.Set("delegate-depth", "-1"); err == nil {
		t.Error("a negative value was accepted for delegate-depth")
	}
	if err := s.Set("delegate-depth", "2"); err != nil || s.DelegateDepth != "2" {
		t.Errorf("a valid delegate-depth was rejected: %v", err)
	}

	if err := s.Set("unknown-key", "x"); err == nil {
		t.Error("an unknown key was accepted by Set")
	}
}

func TestUnsetRevertsToNemuzDefault(t *testing.T) {
	s := &Settings{Provider: "anthropic", AllowExec: []string{"go"}}
	if err := s.Unset("provider"); err != nil {
		t.Fatal(err)
	}
	if s.Provider != "" {
		t.Errorf("provider is %q after unset", s.Provider)
	}
	if err := s.Unset("allow-exec"); err != nil {
		t.Fatal(err)
	}
	if s.AllowExec != nil {
		t.Errorf("allow-exec is %v after unset", s.AllowExec)
	}
	if err := s.Unset("unknown-key"); err == nil {
		t.Error("an unknown key was accepted by Unset")
	}
}

// TestBoolSettingsDistinguishUnsetFromFalse is why Skills/Memories are *bool
// rather than bool: both flags default to true, so a saved "false" and "never
// configured" must be told apart, or turning skills off could never be told
// from simply not having an opinion yet.
func TestBoolSettingsDistinguishUnsetFromFalse(t *testing.T) {
	s := &Settings{}
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

func TestKeysListsEveryGettableSetting(t *testing.T) {
	s := &Settings{}
	for _, k := range Keys {
		if _, ok := s.Get(k); !ok {
			t.Errorf("Keys lists %q but Get does not recognise it", k)
		}
		if err := s.Set(k, s.mustExampleValue(k)); err != nil {
			t.Errorf("Keys lists %q but Set rejects a reasonable value: %v", k, err)
		}
	}
}

// mustExampleValue returns a value Set should accept for key, so
// TestKeysListsEveryGettableSetting can check Keys and Set agree without
// hardcoding per-key expectations twice.
func (s *Settings) mustExampleValue(key string) string {
	switch key {
	case "sandbox":
		return "auto"
	case "skills", "memories":
		return "true"
	case "delegate-depth":
		return "2"
	default:
		return "example"
	}
}
