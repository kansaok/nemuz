package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Settings are the defaults `nemuz config` saves, so a provider, model, or
// sandbox policy chosen once does not have to be typed on every command.
//
// A flag passed explicitly on the command line always wins over these — a
// saved default is a starting point, not a lock-in, and never overrides
// something the operator just said on this exact invocation.
type Settings struct {
	Provider    string   `yaml:"provider,omitempty"`
	Model       string   `yaml:"model,omitempty"`
	ReviewModel string   `yaml:"review_model,omitempty"`
	BaseURL     string   `yaml:"base_url,omitempty"`
	Sandbox     string   `yaml:"sandbox,omitempty"`
	AllowExec   []string `yaml:"allow_exec,omitempty"`
	AllowNet    []string `yaml:"allow_net,omitempty"`
	Skills      *bool    `yaml:"skills,omitempty"`
	Memories    *bool    `yaml:"memories,omitempty"`
}

// Keys names every setting `nemuz config` accepts, in the order they are
// listed. Kept as one slice so `config` (with no arguments) and validation of
// an unknown key name agree on exactly the same list.
var Keys = []string{
	"provider", "model", "review-model", "base-url",
	"sandbox", "allow-exec", "allow-net", "skills", "memories",
}

// LoadSettings reads the saved defaults. A missing file is not an error — it
// means nothing has been configured yet, the same way an empty journal
// directory means no turns have run yet — and it returns an empty Settings
// rather than nil, so callers never need a nil check before reading a field.
func LoadSettings(path string) (*Settings, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Settings{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var s Settings
	if err := yaml.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("config: %s is not valid: %w", path, err)
	}
	return &s, nil
}

// LoadSettingsQuiet is LoadSettings with the failure mode every command that
// merely wants defaults should have: a corrupt or unreadable config file must
// not stop every other command from working, any more than a corrupt search
// index should. Called this way, at worst nemuz falls back to its built-in
// defaults; the sharp-edged LoadSettings stays available for `nemuz config`
// itself, which should say plainly when its own file is broken.
func LoadSettingsQuiet(path string) *Settings {
	s, err := LoadSettings(path)
	if err != nil {
		return &Settings{}
	}
	return s
}

// Save writes the settings atomically: a temp file plus rename, the same
// pattern the blob store and skill store already use, so a crash mid-write
// cannot leave a half-written config file behind.
func Save(path string, s *Settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", filepath.Dir(path), err)
	}
	body, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return os.Rename(tmpName, path)
}

// Get returns one setting's current value as a string, for `nemuz config get`
// and for listing. A key that has never been set returns "" and true — it is
// a known, valid key with nothing saved, distinct from an unknown key name.
func (s *Settings) Get(key string) (value string, known bool) {
	switch key {
	case "provider":
		return s.Provider, true
	case "model":
		return s.Model, true
	case "review-model":
		return s.ReviewModel, true
	case "base-url":
		return s.BaseURL, true
	case "sandbox":
		return s.Sandbox, true
	case "allow-exec":
		return strings.Join(s.AllowExec, ","), true
	case "allow-net":
		return strings.Join(s.AllowNet, ","), true
	case "skills":
		return boolPtrString(s.Skills), true
	case "memories":
		return boolPtrString(s.Memories), true
	default:
		return "", false
	}
}

// Set stores one setting by name, parsing value according to the key's type.
// allow-exec and allow-net take a comma-separated list; skills and memories
// take "true" or "false". An unknown key or an unparseable value is refused,
// so a typo in a shell script fails loudly here rather than being silently
// ignored the first time a command tries to use it.
func (s *Settings) Set(key, value string) error {
	switch key {
	case "provider":
		s.Provider = value
	case "model":
		s.Model = value
	case "review-model":
		s.ReviewModel = value
	case "base-url":
		s.BaseURL = value
	case "sandbox":
		if value != "on" && value != "auto" && value != "off" {
			return fmt.Errorf("config: sandbox must be on, auto, or off, not %q", value)
		}
		s.Sandbox = value
	case "allow-exec":
		s.AllowExec = splitList(value)
	case "allow-net":
		s.AllowNet = splitList(value)
	case "skills":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("config: skills must be true or false, not %q", value)
		}
		s.Skills = &b
	case "memories":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("config: memories must be true or false, not %q", value)
		}
		s.Memories = &b
	default:
		return fmt.Errorf("config: unknown setting %q; see `nemuz config` for the list", key)
	}
	return nil
}

// Unset clears one setting back to nemuz's built-in default.
func (s *Settings) Unset(key string) error {
	switch key {
	case "provider":
		s.Provider = ""
	case "model":
		s.Model = ""
	case "review-model":
		s.ReviewModel = ""
	case "base-url":
		s.BaseURL = ""
	case "sandbox":
		s.Sandbox = ""
	case "allow-exec":
		s.AllowExec = nil
	case "allow-net":
		s.AllowNet = nil
	case "skills":
		s.Skills = nil
	case "memories":
		s.Memories = nil
	default:
		return fmt.Errorf("config: unknown setting %q; see `nemuz config` for the list", key)
	}
	return nil
}

func boolPtrString(b *bool) string {
	if b == nil {
		return ""
	}
	if *b {
		return "true"
	}
	return "false"
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var out []string
	for _, v := range strings.Split(value, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
