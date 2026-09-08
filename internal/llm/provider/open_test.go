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
	_, err := Open(Spec{Provider: "anthropic"})
	if err == nil {
		t.Fatal("a provider was opened with no key")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("the error should name the variable to set, got: %v", err)
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
