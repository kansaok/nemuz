package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ConfigFileName is the configuration file. It is JSON, in the OpenClaw
// style, so anyone who has configured OpenClaw recognises the layout at a
// glance: agent defaults under agents.defaults, custom gateways under
// models.providers, chat surfaces under channels.
const ConfigFileName = "config.json"

// Config is the whole single-file configuration, shaped after OpenClaw's
// openclaw.json. Sections nemuz has no equivalent for (browser profiles, the
// setup wizard) are deliberately absent; a file that names them keeps
// working, the fields are simply not read.
//
// The one difference a reader should notice is secrecy: OpenClaw inlines
// apiKey, botToken, and its gateway token into the file, while nemuz keeps
// secrets in environment variables and lets the file refer to them with a
// ${NAME} reference. A config file is meant to be shareable; the .env sitting
// next to it is not.
type Config struct {
	Agents   Agents   `json:"agents,omitempty"`
	Models   Models   `json:"models,omitempty"`
	Gateway  Gateway  `json:"gateway,omitempty"`
	Plugins  Plugins  `json:"plugins,omitempty"`
	Channels Channels `json:"channels,omitempty"`
	Commands Commands `json:"commands,omitempty"`
	Meta     Meta     `json:"meta,omitempty"`
}

// Agents holds defaults that apply to every turn unless a flag overrides
// them. OpenClaw nests the same way; the nemuz-only keys (provider, base_url,
// sandbox, ...) live here too because this is the "how the agent runs" group.
type Agents struct {
	Defaults AgentDefaults `json:"defaults,omitempty"`
}

type AgentDefaults struct {
	Workspace     string   `json:"workspace,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	BaseURL       string   `json:"base_url,omitempty"`
	Model         ModelSel `json:"model,omitempty"`
	Sandbox       string   `json:"sandbox,omitempty"`
	AllowExec     []string `json:"allow_exec,omitempty"`
	AllowNet      []string `json:"allow_net,omitempty"`
	Skills        *bool    `json:"skills,omitempty"`
	Memories      *bool    `json:"memories,omitempty"`
	Review        *bool    `json:"review,omitempty"`
	ReviewModel   string   `json:"review_model,omitempty"`
	Curate        *bool    `json:"curate,omitempty"`
	DelegateDepth *int     `json:"delegate_depth,omitempty"`
}

// ModelSel is OpenClaw's model selection: a primary, plus optional aliases a
// shorter id can stand in for.
type ModelSel struct {
	Primary string                `json:"primary,omitempty"`
	Models  map[string]ModelAlias `json:"models,omitempty"`
}

type ModelAlias struct {
	Alias string `json:"alias,omitempty"`
}

// Models is the provider catalogue a config file can extend. nemuz's built-in
// providers stay in code; anything listed here is added to them.
type Models struct {
	Mode      string                 `json:"mode,omitempty"`
	Providers map[string]ProviderDef `json:"providers,omitempty"`
}

type ProviderDef struct {
	BaseURL string          `json:"baseUrl,omitempty"`
	API     string          `json:"api,omitempty"`
	APIKey  string          `json:"apiKey,omitempty"`
	Models  []ProviderModel `json:"models,omitempty"`
}

// ProviderModel mirrors the OpenClaw model table closely enough that an
// existing openclaw.json section survives a nemuz read-modify-write. nemuz
// uses only the first id as the provider's default model; the rest is carried
// for round-trip fidelity.
type ProviderModel struct {
	ID            string     `json:"id,omitempty"`
	Name          string     `json:"name,omitempty"`
	ContextWindow int        `json:"contextWindow,omitempty"`
	MaxTokens     int        `json:"maxTokens,omitempty"`
	Input         []string   `json:"input,omitempty"`
	Cost          *ModelCost `json:"cost,omitempty"`
	Reasoning     bool       `json:"reasoning,omitempty"`
}

type ModelCost struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cacheRead,omitempty"`
	CacheWrite float64 `json:"cacheWrite,omitempty"`
}

// Gateway configures the HTTP API (`nemuz serve`), the same way OpenClaw's
// gateway section does. token holds the key clients must present; mode keeps
// the OpenClaw "local" / "remote" distinction even though nemuz only reads it.
type Gateway struct {
	Mode string      `json:"mode,omitempty"`
	Auth GatewayAuth `json:"auth,omitempty"`
}

type GatewayAuth struct {
	Mode  string `json:"mode,omitempty"`
	Token string `json:"token,omitempty"`
}

// Plugins lists plugins to load by default, so a channel or an eval run picks
// them up without repeating --plugin on every command.
type Plugins struct {
	Entries map[string]PluginEntry `json:"entries,omitempty"`
}

type PluginEntry struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// Channels configures chat surfaces. Only telegram exists today; the struct is
// keyed by name so Slack, Discord, and WhatsApp slot in beside it.
type Channels struct {
	Telegram *ChannelTelegram `json:"telegram,omitempty"`
}

type ChannelTelegram struct {
	Enabled    *bool                    `json:"enabled,omitempty"`
	BotToken   string                   `json:"botToken,omitempty"`
	AllowUsers []int64                  `json:"allowUsers,omitempty"`
	Groups     map[string]TelegramGroup `json:"groups,omitempty"`
}

type TelegramGroup struct {
	RequireMention *bool `json:"requireMention,omitempty"`
}

// Commands holds who may command the agent, the "owner" list OpenClaw keeps
// under the same name. Entries of the form "telegram:<id>" are treated as the
// Telegram allowlist when channels.telegram.allowUsers is empty.
type Commands struct {
	OwnerAllowFrom []string `json:"ownerAllowFrom,omitempty"`
}

// Meta is bookkeeping, written by nemuz the same way OpenClaw writes its own.
type Meta struct {
	LastTouchedVersion string `json:"lastTouchedVersion,omitempty"`
	LastTouchedAt      string `json:"lastTouchedAt,omitempty"`
}

// LoadSettings reads the saved configuration. A missing file is not an error —
// it means nothing has been configured yet, the same way an empty journal
// directory means no turns have run yet — and it returns an empty Config rather
// than nil, so callers never need a nil check before reading a field. A legacy
// flat config.yaml next to the json is migrated on first use.
func LoadSettings(path string) (*Config, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return migrateLegacyYAML(path)
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("config: %s is not valid: %w", path, err)
	}
	return &c, nil
}

// LoadSettingsQuiet is LoadSettings with the failure mode every command that
// merely wants defaults should have: a corrupt or unreadable config file must
// not stop every other command from working, any more than a corrupt search
// index should. Called this way, at worst nemuz falls back to its built-in
// defaults; the sharp-edged LoadSettings stays available for `nemuz config`
// itself, which should say plainly when its own file is broken.
func LoadSettingsQuiet(path string) *Config {
	s, err := LoadSettings(path)
	if err != nil {
		return &Config{}
	}
	return s
}

// Save writes the configuration atomically: a temp file plus rename, the same
// pattern the blob store and skill store already use, so a crash mid-write
// cannot leave a half-written config file behind.
func Save(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", filepath.Dir(path), err)
	}
	if c.Meta.LastTouchedAt == "" {
		c.Meta.LastTouchedAt = time.Now().UTC().Format(time.RFC3339)
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	body = append(body, '\n')

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

// Expand returns a copy of c with every ${NAME} reference replaced by the
// value of that environment variable (and the plain $NAME form, as a bonus).
// Secrets therefore live in the environment — exported, or loaded by
// LoadDotEnv from the .env next to the config — while the config file itself
// only names them. A reference to an unset variable expands to empty rather
// than failing, so a missing secret surfaces later as a missing-api-key error
// instead of a config parse failure.
func (c *Config) Expand() *Config {
	if c == nil {
		return &Config{}
	}
	body, err := json.Marshal(c)
	if err != nil {
		return c
	}
	out := &Config{}
	if err := json.Unmarshal(body, out); err != nil {
		return c
	}
	expandStrings(reflect.ValueOf(out))
	return out
}

// expandStrings walks a Config value, replacing ${NAME} references in every
// string field, slice element, and map value.
func expandStrings(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return
		}
		expandStrings(v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).Tag.Get("json") == "-" {
				continue
			}
			expandStrings(v.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			expandStrings(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			cur := v.MapIndex(k)
			nv := reflect.New(cur.Type()).Elem()
			nv.Set(cur)
			expandStrings(nv)
			v.SetMapIndex(k, nv)
		}
	case reflect.String:
		v.SetString(os.Expand(v.String(), os.Getenv))
	}
}

// legacySettings is the pre-0.14 flat ~/.nemuz/config.yaml layout, kept so an
// existing installation is migrated rather than reconfigured from scratch.
type legacySettings struct {
	Provider      string   `yaml:"provider,omitempty"`
	Model         string   `yaml:"model,omitempty"`
	ReviewModel   string   `yaml:"review_model,omitempty"`
	BaseURL       string   `yaml:"base_url,omitempty"`
	Sandbox       string   `yaml:"sandbox,omitempty"`
	AllowExec     []string `yaml:"allow_exec,omitempty"`
	AllowNet      []string `yaml:"allow_net,omitempty"`
	Skills        *bool    `yaml:"skills,omitempty"`
	Memories      *bool    `yaml:"memories,omitempty"`
	DelegateDepth string   `yaml:"delegate_depth,omitempty"`
}

func (l *legacySettings) toConfig() *Config {
	depth := 0
	if n, err := fmt.Sscanf(l.DelegateDepth, "%d", &depth); err == nil && n == 1 {
		// keep
	} else {
		depth = 0
	}
	var d *int
	if depth > 0 {
		d = &depth
	} else if l.DelegateDepth == "0" {
		z := 0
		d = &z
	}
	return &Config{
		Agents: Agents{Defaults: AgentDefaults{
			Provider:      l.Provider,
			BaseURL:       l.BaseURL,
			Model:         ModelSel{Primary: l.Model},
			ReviewModel:   l.ReviewModel,
			Sandbox:       l.Sandbox,
			AllowExec:     l.AllowExec,
			AllowNet:      l.AllowNet,
			Skills:        l.Skills,
			Memories:      l.Memories,
			DelegateDepth: d,
		}},
	}
}

// migrateLegacyYAML converts a flat ~/.nemuz/config.yaml into the nested
// config.json on first use. The old file is renamed aside rather than deleted,
// so nothing is lost if the write fails; a legacy file with nothing configured
// is left alone.
func migrateLegacyYAML(jsonPath string) (*Config, error) {
	yamlPath := filepath.Join(filepath.Dir(jsonPath), "config.yaml")
	body, err := os.ReadFile(yamlPath)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return &Config{}, nil
	}
	var flat legacySettings
	if err := yaml.Unmarshal(body, &flat); err != nil {
		return &Config{}, nil
	}
	if flat.Provider == "" && flat.Model == "" && flat.Sandbox == "" &&
		flat.ReviewModel == "" && flat.BaseURL == "" && flat.DelegateDepth == "" &&
		flat.Skills == nil && flat.Memories == nil {
		return &Config{}, nil
	}
	cfg := flat.toConfig()
	if Save(jsonPath, cfg) == nil {
		_ = os.Rename(yamlPath, yamlPath+".bak")
	}
	return cfg, nil
}

// Provider returns the configured default provider name, or "".
func (c *Config) Provider() string {
	if c == nil {
		return ""
	}
	return c.Agents.Defaults.Provider
}

// Sandbox returns the configured default sandbox mode, or "".
func (c *Config) Sandbox() string {
	if c == nil {
		return ""
	}
	return c.Agents.Defaults.Sandbox
}

// SplitModelPrimary breaks an OpenClaw-style primary of the form
// "<provider>/<model>" into its parts. A primary without a slash is used whole
// as the model id.
func SplitModelPrimary(primary string) (provider, model string, slash bool) {
	head, tail, ok := strings.Cut(primary, "/")
	if !ok {
		return "", primary, false
	}
	return head, tail, true
}
