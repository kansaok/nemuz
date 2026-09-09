package provider

import (
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

// Names returns the known provider names, sorted.
func Names() []string {
	out := make([]string, 0, len(presets))
	for name := range presets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Lookup returns the preset for name.
func Lookup(name string) (Preset, bool) {
	p, ok := presets[strings.ToLower(name)]
	return p, ok
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
