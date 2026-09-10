package provider

import (
	"strings"
	"testing"
)

func TestOpenBuildsTheRightAdapterPerFormat(t *testing.T) {
	for _, env := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(env, "k")
	}

	for name, wantName := range map[string]string{
		"anthropic":  "anthropic",
		"gemini":     "gemini",
		"openai":     "openai",
		"ollama":     "ollama",
		"openrouter": "openrouter",
	} {
		// A model is passed explicitly because presets that exist only to
		// route to many models deliberately have no default.
		p, err := Open(Spec{Provider: name, Model: "m"})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if p.Name() != wantName {
			t.Errorf("%s built a provider called %q", name, p.Name())
		}
	}
}

// TestLocalProvidersNeedNoKey is the point of running a model locally: no
// account, no key, no egress.
func TestLocalProvidersNeedNoKey(t *testing.T) {
	for _, name := range []string{"ollama", "lmstudio"} {
		if _, err := Open(Spec{Provider: name, Model: "m"}); err != nil {
			t.Errorf("%s asked for credentials: %v", name, err)
		}
	}
}

func TestMissingKeyNamesTheEnvironmentVariable(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv(GenericKeyEnv, "")
	_, err := Open(Spec{Provider: "anthropic"})
	if err == nil {
		t.Fatal("a provider was opened with no key")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("the error should name the variable to set, got: %v", err)
	}
}

// TestGenericKeyEnvIsAFallback is why NEMUZ_API_KEY exists: --base-url
// pointed at a custom OpenAI-compatible gateway still has to pick some
// preset's name to get the right wire format (often "openai", the most
// generic one), but the key living in a variable literally named after that
// choice is a needless trap. One generic variable works no matter which
// preset was picked to reach a custom endpoint.
func TestGenericKeyEnvIsAFallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv(GenericKeyEnv, "generic-key")

	p, err := Open(Spec{Provider: "openai", Model: "m", BaseURL: "http://127.0.0.1:9999/v1"})
	if err != nil {
		t.Fatalf("the generic fallback was not used: %v", err)
	}
	oa, ok := p.(*OpenAI)
	if !ok {
		t.Fatalf("got %T, want *OpenAI", p)
	}
	if oa.tr.cfg.Headers["Authorization"] != "Bearer generic-key" {
		t.Errorf("the key from %s was not used, got header %q", GenericKeyEnv, oa.tr.cfg.Headers["Authorization"])
	}
}

// TestPresetKeyEnvWinsOverGeneric protects the common case: a real
// provider-specific key already set should never be shadowed by a leftover
// NEMUZ_API_KEY from some other project.
func TestPresetKeyEnvWinsOverGeneric(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "specific-key")
	t.Setenv(GenericKeyEnv, "generic-key")

	p, err := Open(Spec{Provider: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	an, ok := p.(*Anthropic)
	if !ok {
		t.Fatalf("got %T, want *Anthropic", p)
	}
	if an.tr.cfg.Headers["x-api-key"] != "specific-key" {
		t.Errorf("api key header = %q, want the preset-specific value", an.tr.cfg.Headers["x-api-key"])
	}
}

func TestUnknownProviderListsTheKnownOnes(t *testing.T) {
	_, err := Open(Spec{Provider: "tidak-ada"})
	if err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	if !strings.Contains(err.Error(), "anthropic") || !strings.Contains(err.Error(), "ollama") {
		t.Errorf("the error should list what is available, got: %v", err)
	}
}

// TestMostProvidersReuseOneAdapter is the claim the catalogue exists to make.
func TestMostProvidersReuseOneAdapter(t *testing.T) {
	var compatible int
	for _, name := range Names() {
		if p, _ := Lookup(name); p.Kind == kindOpenAI {
			compatible++
		}
	}
	if compatible < len(presets)-2 {
		t.Fatalf("only %d of %d providers reuse the OpenAI adapter", compatible, len(presets))
	}
	t.Logf("%d of %d providers are served by one adapter", compatible, len(presets))
}

func TestOverridesWin(t *testing.T) {
	p, err := Open(Spec{Provider: "openai", Model: "custom", BaseURL: "http://127.0.0.1:9999/v1", APIKey: "inline"})
	if err != nil {
		t.Fatal(err)
	}
	oa, ok := p.(*OpenAI)
	if !ok {
		t.Fatalf("got %T", p)
	}
	if oa.model != "custom" {
		t.Errorf("model is %q", oa.model)
	}
	if oa.tr.cfg.BaseURL != "http://127.0.0.1:9999/v1" {
		t.Errorf("base URL is %q", oa.tr.cfg.BaseURL)
	}
	if oa.tr.cfg.Headers["Authorization"] != "Bearer inline" {
		t.Errorf("inline key was not used")
	}
}

func TestRegisterCustomAndOpen(t *testing.T) {
	t.Cleanup(func() { custom = map[string]Custom{} })
	t.Setenv(GenericKeyEnv, "")

	if err := RegisterCustom(map[string]Custom{
		"custom-ai-foo-com": {
			BaseURL:      "https://ai.foo.com/v1",
			API:          "openai-completions",
			APIKey:       "config-key",
			DefaultModel: "model-v1",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !Known("custom-ai-foo-com") {
		t.Fatal("a registered custom provider is not known")
	}
	if !contains(Names(), "custom-ai-foo-com") {
		t.Fatal("a registered custom provider is missing from Names()")
	}

	p, err := Open(Spec{Provider: "custom-ai-foo-com"})
	if err != nil {
		t.Fatal(err)
	}
	oa, ok := p.(*OpenAI)
	if !ok {
		t.Fatalf("got %T, want *OpenAI", p)
	}
	if oa.model != "model-v1" {
		t.Errorf("default model = %q", oa.model)
	}
	if oa.tr.cfg.BaseURL != "https://ai.foo.com/v1" {
		t.Errorf("base URL = %q", oa.tr.cfg.BaseURL)
	}
	if oa.tr.cfg.Headers["Authorization"] != "Bearer config-key" {
		t.Errorf("the config key was not used")
	}
}

func TestRegisterCustomRejectsCollisions(t *testing.T) {
	t.Cleanup(func() { custom = map[string]Custom{} })
	err := RegisterCustom(map[string]Custom{
		"anthropic": {API: "anthropic"},
		"bad-wire":  {API: "lisp-with-parens"},
		"ok-one":    {API: "openai-completions", BaseURL: "http://127.0.0.1:9999/v1"},
	})
	if err == nil {
		t.Fatal("a config that collides with a preset should be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "anthropic") || !strings.Contains(msg, "bad-wire") {
		t.Errorf("the error should name both rejections, got: %v", err)
	}
	// The valid entry still got registered.
	if !Known("ok-one") {
		t.Error("a valid custom provider was dropped with the bad ones")
	}
	// And the built-in preset is untouched.
	if _, ok := Lookup("anthropic"); !ok {
		t.Error("RegisterCustom replaced a built-in provider")
	}
}

func TestCustomProviderFallsBackToGenericKeyEnv(t *testing.T) {
	t.Cleanup(func() { custom = map[string]Custom{} })
	t.Setenv(GenericKeyEnv, "generic-key")
	if err := RegisterCustom(map[string]Custom{
		"no-key-in-config": {API: "openai-completions", BaseURL: "http://127.0.0.1:9999/v1"},
	}); err != nil {
		t.Fatal(err)
	}
	p, err := Open(Spec{Provider: "no-key-in-config", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	oa, ok := p.(*OpenAI)
	if !ok {
		t.Fatalf("got %T", p)
	}
	if oa.tr.cfg.Headers["Authorization"] != "Bearer generic-key" {
		t.Errorf("the generic fallback was not used, header = %q", oa.tr.cfg.Headers["Authorization"])
	}
}

func TestCustomProviderNeedsBaseURLAndModel(t *testing.T) {
	t.Cleanup(func() { custom = map[string]Custom{} })
	if err := RegisterCustom(map[string]Custom{"bare": {API: "openai-completions"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Spec{Provider: "bare"}); err == nil {
		t.Error("a custom provider with no baseUrl or model opened successfully")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
