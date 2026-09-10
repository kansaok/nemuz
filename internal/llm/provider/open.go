package provider

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kansaok/nemuz/internal/llm"
)

// kind is which wire format a provider speaks. There are only three, and that
// is the point: most vendors serve the OpenAI shape, so the work of supporting
// them is a table entry rather than an adapter.
type kind int

const (
	kindOpenAI kind = iota
	kindAnthropic
	kindGemini
)

// Preset is a known provider: which format it speaks, where it lives, and which
// environment variable carries its key.
type Preset struct {
	Kind kind
	// BaseURL is the API root. Empty means the adapter's own default.
	BaseURL string
	// KeyEnv names the environment variable holding the API key. Empty means
	// the provider needs no key.
	KeyEnv string
	// DefaultModel is used when none is given.
	DefaultModel string
	// Note explains anything surprising, and is shown by `nemuz providers`.
	Note string
}

// presets is the whole provider catalogue.
//
// Adding a vendor that speaks the OpenAI format costs one line here. That is
// the difference between this approach and writing an adapter per vendor: the
// three formats below already reach every entry in this table.
var presets = map[string]Preset{
	"openai":     {Kind: kindOpenAI, KeyEnv: "OPENAI_API_KEY", DefaultModel: "gpt-5"},
	"anthropic":  {Kind: kindAnthropic, KeyEnv: "ANTHROPIC_API_KEY", DefaultModel: "claude-opus-5"},
	"gemini":     {Kind: kindGemini, KeyEnv: "GEMINI_API_KEY", DefaultModel: "gemini-2.5-pro"},
	"openrouter": {Kind: kindOpenAI, BaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY"},
	"groq":       {Kind: kindOpenAI, BaseURL: "https://api.groq.com/openai/v1", KeyEnv: "GROQ_API_KEY"},
	"deepseek":   {Kind: kindOpenAI, BaseURL: "https://api.deepseek.com/v1", KeyEnv: "DEEPSEEK_API_KEY"},
	"together":   {Kind: kindOpenAI, BaseURL: "https://api.together.xyz/v1", KeyEnv: "TOGETHER_API_KEY"},
	"fireworks":  {Kind: kindOpenAI, BaseURL: "https://api.fireworks.ai/inference/v1", KeyEnv: "FIREWORKS_API_KEY"},
	"mistral":    {Kind: kindOpenAI, BaseURL: "https://api.mistral.ai/v1", KeyEnv: "MISTRAL_API_KEY"},
	"xai":        {Kind: kindOpenAI, BaseURL: "https://api.x.ai/v1", KeyEnv: "XAI_API_KEY"},
	"ollama": {
		Kind: kindOpenAI, BaseURL: "http://localhost:11434/v1", DefaultModel: "llama3.2",
		Note: "local; needs no API key",
	},
	"lmstudio": {
		Kind: kindOpenAI, BaseURL: "http://localhost:1234/v1",
		Note: "local; needs no API key",
	},
	"vllm": {
		Kind: kindOpenAI, BaseURL: "http://localhost:8000/v1",
		Note: "self-hosted; set --base-url to your server",
	},
}

// GenericKeyEnv is read for any provider whose preset-specific variable is
// unset. It exists for --base-url pointed at a custom OpenAI-compatible
// gateway: the provider name chosen there (often just "openai", the most
// generic preset that speaks the shape) does not have to match where the key
// actually comes from, so one variable works regardless of which preset's
// name was picked to reach it.
const GenericKeyEnv = "NEMUZ_API_KEY"

// Custom is a provider defined in config (models.providers) rather than in
// the built-in catalogue. It needs the same three things a preset spells out
// in code — which wire format, which endpoint, where the key comes from —
// plus a default model if the config listed any.
type Custom struct {
	// BaseURL is the API root the provider speaks at.
	BaseURL string
	// API is the wire format: openai-completions, anthropic, or google-ai
	// (gemini-native and the other OpenAI-shaped spellings are accepted too).
	API string
	// APIKey is the key, already resolved from its ${NAME} reference. Empty
	// lets the generic NEMUZ_API_KEY fallback be used instead.
	APIKey string
	// DefaultModel is the first model id in models.providers.<name>.models.
	DefaultModel string
}

// custom holds providers registered from config. package-private because
// registration is a startup step, not something a turn may race.
var custom = map[string]Custom{}

// RegisterCustom adds models.providers entries from config to the catalogue. A
// name that collides with a built-in preset is refused — config must not
// silently replace what code ships — and so is an API name nemuz cannot speak.
// Everything else is registered, and all the rejections come back as one
// error so a caller can show them all at once.
func RegisterCustom(in map[string]Custom) error {
	var errs []string
	next := make(map[string]Custom, len(in))
	for name, c := range in {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if _, ok := presets[name]; ok {
			errs = append(errs, fmt.Sprintf("provider: %s is a built-in provider; pick a different key under models.providers", name))
			continue
		}
		if _, err := customKind(c.API); err != nil {
			errs = append(errs, fmt.Sprintf("provider: %s: %v", name, err))
			continue
		}
		next[name] = c
	}
	custom = next
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

// CustomNames reports the config-defined provider names, sorted, for display.
func CustomNames() []string {
	out := make([]string, 0, len(custom))
	for name := range custom {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// LookupCustom returns a registered config provider.
func LookupCustom(name string) (Custom, bool) {
	c, ok := custom[strings.ToLower(strings.TrimSpace(name))]
	return c, ok
}

// customKind maps an OpenClaw api spelling onto nemuz's three wire formats.
func customKind(api string) (kind, error) {
	switch strings.ToLower(strings.TrimSpace(api)) {
	case "", "openai-completions", "openai-responses", "ollama", "llamacpp", "vllm", "mistral":
		return kindOpenAI, nil
	case "anthropic":
		return kindAnthropic, nil
	case "google-ai", "gemini-native", "google-vertex":
		return kindGemini, nil
	default:
		return 0, fmt.Errorf("unsupported api %q; use openai-completions, anthropic, or google-ai", api)
	}
}

// Names returns the known provider names, sorted. Config-defined providers are
// included once registered, so `nemuz providers` and error messages agree on
// what is available.
func Names() []string {
	out := make([]string, 0, len(presets))
	for name := range presets {
		out = append(out, name)
	}
	for name := range custom {
		if _, ok := presets[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Known reports whether name is a usable provider: built-in or registered from
// config. Used to split an OpenClaw-style "provider/model" primary.
func Known(name string) bool {
	if _, ok := Lookup(name); ok {
		return true
	}
	name = strings.ToLower(strings.TrimSpace(name))
	_, ok := custom[name]
	return ok
}

// Lookup returns the preset for name.
func Lookup(name string) (Preset, bool) {
	p, ok := presets[strings.ToLower(name)]
	return p, ok
}

// PresetNames lists the built-in provider names, sorted, excluding config-defined
// ones. `nemuz config` offers these plus a "custom provider" option.
func PresetNames() []string {
	out := make([]string, 0, len(presets))
	for name := range presets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Wire names the wire format a preset speaks, in the same spelling
// models.providers.api accepts ("openai-completions", "anthropic", "google-ai").
func (p Preset) Wire() string {
	switch p.Kind {
	case kindAnthropic:
		return "anthropic"
	case kindGemini:
		return "google-ai"
	default:
		return "openai-completions"
	}
}

// Wire is the OpenClaw api spelling for a config-defined provider: it is what
// gets written back under models.providers.<name>.api.
func (c Custom) Wire() (string, error) {
	switch strings.ToLower(strings.TrimSpace(c.API)) {
	case "google-ai", "gemini-native", "google-vertex":
		return "google-ai", nil
	case "anthropic":
		return "anthropic", nil
	default:
		return "openai-completions", nil
	}
}

// Spec selects and configures a provider.
type Spec struct {
	// Provider is a name from Names().
	Provider string
	// Model overrides the preset's default.
	Model string
	// BaseURL overrides the preset's endpoint, for proxies and self-hosting.
	BaseURL string
	// APIKey overrides the environment variable. Prefer the environment.
	APIKey string
}

// Open builds a provider from spec, reading the API key from the environment
// unless one was supplied.
func Open(spec Spec) (llm.Provider, error) {
	name := strings.ToLower(strings.TrimSpace(spec.Provider))
	if name == "" {
		return nil, fmt.Errorf("provider: no provider named; known providers are %s", strings.Join(Names(), ", "))
	}
	preset, ok := Lookup(name)
	if !ok {
		if c, in := LookupCustom(name); in {
			return openCustom(spec, name, c)
		}
		return nil, fmt.Errorf("provider: unknown provider %q; known providers are %s", name, strings.Join(Names(), ", "))
	}

	key := spec.APIKey
	if key == "" && preset.KeyEnv != "" {
		key = os.Getenv(preset.KeyEnv)
	}
	if key == "" && preset.KeyEnv != "" {
		key = os.Getenv(GenericKeyEnv)
	}
	if key == "" && preset.KeyEnv != "" {
		return nil, fmt.Errorf("provider: %s needs an API key; set %s (or the generic %s)", name, preset.KeyEnv, GenericKeyEnv)
	}

	model := spec.Model
	if model == "" {
		model = preset.DefaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("provider: %s has no default model; pass --model", name)
	}

	baseURL := spec.BaseURL
	if baseURL == "" {
		baseURL = preset.BaseURL
	}
	cfg := Config{BaseURL: baseURL, APIKey: key, Model: model}

	switch preset.Kind {
	case kindAnthropic:
		return NewAnthropic(cfg), nil
	case kindGemini:
		return NewGemini(cfg), nil
	default:
		return NewOpenAI(name, cfg), nil
	}
}

// openCustom builds a provider from a config-defined entry. Its behaviour
// matches a preset's: an explicit Spec field wins over the config value, and
// the key comes from the spec, then the config, then the generic fallback.
func openCustom(spec Spec, name string, c Custom) (llm.Provider, error) {
	k, err := customKind(c.API)
	if err != nil {
		return nil, fmt.Errorf("provider: %s: %w", name, err)
	}

	key := spec.APIKey
	if key == "" {
		key = c.APIKey
	}
	if key == "" {
		key = os.Getenv(GenericKeyEnv)
	}

	model := spec.Model
	if model == "" {
		model = c.DefaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("provider: %s has no default model; pass --model or list one under models.providers", name)
	}

	baseURL := spec.BaseURL
	if baseURL == "" {
		baseURL = c.BaseURL
	}
	if baseURL == "" {
		return nil, fmt.Errorf("provider: %s has no baseUrl; set one under models.providers", name)
	}

	cfg := Config{BaseURL: baseURL, APIKey: key, Model: model}
	switch k {
	case kindAnthropic:
		return NewAnthropic(cfg), nil
	case kindGemini:
		return NewGemini(cfg), nil
	default:
		return NewOpenAI(name, cfg), nil
	}
}

// Describe renders a preset for display.
func (p Preset) Describe() string {
	format := map[kind]string{
		kindOpenAI:    "openai-compatible",
		kindAnthropic: "anthropic",
		kindGemini:    "gemini",
	}[p.Kind]

	parts := []string{format}
	if p.KeyEnv != "" {
		parts = append(parts, p.KeyEnv)
	}
	if p.Note != "" {
		parts = append(parts, p.Note)
	}
	return strings.Join(parts, " · ")
}
