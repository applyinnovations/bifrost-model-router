package config

import (
	"strings"
	"testing"
)

func TestDecodeDefaultsAndResolveAliases(t *testing.T) {
	cfg, err := Decode(strings.NewReader(`
version: 1
providers:
  openai:
    credential_mode: request_passthrough
    responses_mode: native
models:
  openai/gpt-test:
    aliases: [gpt-test]
    codex: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := cfg.ResolveModel("gpt-test")
	if !ok || resolved.Slug != "openai/gpt-test" {
		t.Fatalf("resolution = %#v, %v", resolved, ok)
	}
	if resolved.Model.Codex.ContextWindow != 131072 {
		t.Fatalf("context window = %d", resolved.Model.Codex.ContextWindow)
	}
	if resolved.UpstreamModel != "gpt-test" || resolved.BaseSlug != "openai/gpt-test" {
		t.Fatalf("upstream resolution = %#v", resolved)
	}
}

func TestImageGenerationModelDefaultsToDiscoverableOpenAILuna(t *testing.T) {
	cfg, err := Decode(strings.NewReader(`
version: 1
providers:
  openai: {credential_mode: request_passthrough, responses_mode: native, discover_models: true}
  managed: {credential_mode: bifrost, responses_mode: chat_polyfill, discover_models: true}
models:
  openai/gpt-5.6-sol: {codex: {}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageGenerationModel != DefaultImageGenerationModel {
		t.Fatalf("image generation model = %q", cfg.ImageGenerationModel)
	}
}

func TestExplicitImageGenerationModelTakesPrecedence(t *testing.T) {
	cfg, err := Decode(strings.NewReader(`
version: 1
image_generation_model: openai/gpt-5.6-sol
providers:
  openai: {credential_mode: request_passthrough, responses_mode: native, discover_models: true}
  managed: {credential_mode: bifrost, responses_mode: chat_polyfill, discover_models: true}
models:
  openai/gpt-5.6-sol: {codex: {}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageGenerationModel != "openai/gpt-5.6-sol" {
		t.Fatalf("image generation model = %q", cfg.ImageGenerationModel)
	}
}

func TestImageGenerationModelDoesNotInventStaticModel(t *testing.T) {
	cfg, err := Decode(strings.NewReader(`
version: 1
providers:
  openai: {credential_mode: request_passthrough, responses_mode: native}
  managed: {credential_mode: bifrost, responses_mode: chat_polyfill, discover_models: true}
models:
  openai/gpt-5.6-sol: {codex: {}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageGenerationModel != "" {
		t.Fatalf("unlisted static image model = %q", cfg.ImageGenerationModel)
	}
}

func TestContextVariantsResolveDeterministically(t *testing.T) {
	cfg, err := Decode(strings.NewReader(`
version: 1
providers:
  openai: {credential_mode: request_passthrough, responses_mode: native, discover_models: true}
  managed: {credential_mode: bifrost, responses_mode: chat_polyfill}
models:
  openai/sol:
    aliases: [sol]
    upstream_model: gpt-sol-upstream
    codex: {context_window: 272000, max_context_window: 872000, effective_context_window_percent: 95}
    context_variants:
      - {context_window: 872000}
  managed/text-model:
    aliases: [text-model]
    codex: {context_window: 256000, max_context_window: 1000000, effective_context_window_percent: 90}
    context_variants:
      - {context_window: 256000}
      - {context_window: 1000000, effective_context_window_percent: 95}
`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, slug, base, upstream string
		window, percent            int64
	}{
		{"sol-872k", "sol-872k", "openai/sol", "gpt-sol-upstream", 872000, 95},
		{"openai/sol-872k", "sol-872k", "openai/sol", "gpt-sol-upstream", 872000, 95},
		{"managed/text-model-256k", "managed/text-model-256k", "managed/text-model", "text-model", 256000, 90},
		{"text-model-1m", "managed/text-model-1m", "managed/text-model", "text-model", 1000000, 95},
	}
	for _, test := range tests {
		resolved, ok := cfg.ResolveModel(test.name)
		if !ok || resolved.Variant == nil || resolved.Slug != test.slug || resolved.BaseSlug != test.base || resolved.UpstreamModel != test.upstream || resolved.Model.Codex.ContextWindow != test.window || resolved.Model.Codex.EffectiveContextWindowPercent != test.percent {
			t.Errorf("ResolveModel(%q) = %#v, %v", test.name, resolved, ok)
		}
	}
	if _, ok := cfg.ResolveModel("sol-512k"); ok {
		t.Fatal("unconfigured context suffix resolved")
	}
}

func TestDiscoveredModelsResolveFromProviderDefaults(t *testing.T) {
	cfg, err := Decode(strings.NewReader(`
version: 1
providers:
  openai: {credential_mode: request_passthrough, responses_mode: native, discover_models: true}
  managed:
    credential_mode: bifrost
    responses_mode: chat_polyfill
    discover_models: true
    codex_defaults: {context_window: 64000, input_modalities: [text]}
models:
  openai/sol: {aliases: [sol], codex: {context_window: 272000, max_context_window: 872000}, context_variants: [{context_window: 872000}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	managed, ok := cfg.ResolveModel("managed/new-model")
	if !ok || managed.UpstreamModel != "new-model" || managed.Model.ResponsesMode != ResponsesChatPolyfill || managed.Model.Codex.ContextWindow != 64000 {
		t.Fatalf("managed discovery = %#v, %v", managed, ok)
	}
	openAI, ok := cfg.ResolveModel("new-openai-model")
	if !ok || openAI.Slug != "openai/new-openai-model" || openAI.Provider.CredentialMode != CredentialRequestPassthrough {
		t.Fatalf("OpenAI discovery = %#v, %v", openAI, ok)
	}
	if _, ok := cfg.ResolveModel("unknown/model"); ok {
		t.Fatal("unknown provider resolved dynamically")
	}
	if _, ok := cfg.ResolveModel("sol-512k"); ok {
		t.Fatal("unknown configured context variant resolved dynamically")
	}
	if _, ok := cfg.ResolveModel("openai/sol-512k"); ok {
		t.Fatal("unknown canonical context variant resolved dynamically")
	}
}

func TestDiscoveredModelWithNestedUpstreamIDRequiresCanonicalProvider(t *testing.T) {
	cfg, err := Decode(strings.NewReader(`
version: 1
providers:
  openai: {credential_mode: request_passthrough, responses_mode: native}
  managed:
    credential_mode: bifrost
    responses_mode: chat_polyfill
    discover_models: true
    model_name_overrides:
      vendor/reasoner: Reasoner
models:
  openai/default: {aliases: [default], codex: {}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.ResolveModel("vendor/reasoner"); ok {
		t.Fatal("display-name override inferred routing ownership")
	}
	resolved, ok := cfg.ResolveModel("managed/vendor/reasoner")
	if !ok || resolved.Slug != "managed/vendor/reasoner" || resolved.UpstreamModel != "vendor/reasoner" || resolved.Model.Provider != "managed" {
		t.Fatalf("nested discovered model = %#v, %v", resolved, ok)
	}
}

func TestContextVariantValidation(t *testing.T) {
	tests := []struct{ name, variant string }{
		{"non-round", "872001"},
		{"above maximum", "873000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(`
version: 1
providers:
  openai: {credential_mode: request_passthrough, responses_mode: native}
models:
  openai/sol:
    aliases: [sol]
    codex: {context_window: 272000, max_context_window: 872000}
    context_variants: [{context_window: ` + test.variant + `}]
`))
			if err == nil {
				t.Fatal("expected context variant validation error")
			}
		})
	}
}

func TestContextVariantCollidesWithAlias(t *testing.T) {
	_, err := Decode(strings.NewReader(`
version: 1
providers:
  openai: {credential_mode: request_passthrough, responses_mode: native}
models:
  openai/sol:
    aliases: [sol, sol-872k]
    codex: {context_window: 272000, max_context_window: 872000}
    context_variants: [{context_window: 872000}]
`))
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("expected generated slug collision, got %v", err)
	}
}

func TestRealModelNameEndingInContextLikeSuffixIsNotRewritten(t *testing.T) {
	cfg, err := Decode(strings.NewReader(`
version: 1
providers:
  managed: {credential_mode: bifrost, responses_mode: native}
models:
  managed/native-1m:
    upstream_model: actual-native-1m
    codex: {context_window: 128000}
`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := cfg.ResolveModel("managed/native-1m")
	if !ok || resolved.Variant != nil || resolved.UpstreamModel != "actual-native-1m" {
		t.Fatalf("resolution = %#v, %v", resolved, ok)
	}
}

func TestRejectsPassthroughForNonOpenAI(t *testing.T) {
	_, err := Decode(strings.NewReader(`
version: 1
providers:
  other:
    credential_mode: request_passthrough
    responses_mode: native
models:
  other/model:
    codex: {}
`))
	if err == nil || !strings.Contains(err.Error(), "only openai") {
		t.Fatalf("expected passthrough rejection, got %v", err)
	}
}

func TestRejectsAliasCollision(t *testing.T) {
	_, err := Decode(strings.NewReader(`
version: 1
providers:
  one: {credential_mode: bifrost, responses_mode: native}
models:
  one/a: {aliases: [shared], codex: {}}
  one/b: {aliases: [shared], codex: {}}
`))
	if err == nil || !strings.Contains(err.Error(), "owned by both") {
		t.Fatalf("expected collision rejection, got %v", err)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	_, err := Decode(strings.NewReader(`
version: 1
unexpected: true
providers:
  openai: {credential_mode: request_passthrough, responses_mode: native}
models:
  openai/test: {codex: {}}
`))
	if err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestFromAnyRejectsUnknownFields(t *testing.T) {
	_, err := FromAny(map[string]any{
		"version":    1,
		"unexpected": true,
		"providers": map[string]any{
			"openai": map[string]any{"credential_mode": "request_passthrough", "responses_mode": "native"},
		},
		"models": map[string]any{"openai/test": map[string]any{"codex": map[string]any{}}},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestProviderDisplayName(t *testing.T) {
	cfg := Config{Providers: map[string]ProviderProfile{
		"openai":       {},
		"openrouter":   {},
		"nvidia-build": {},
		"custom-plan":  {DisplayName: "Team Plan"},
	}}
	if got := cfg.ProviderDisplayName("openai"); got != "OpenAI" {
		t.Fatalf("OpenAI label = %q", got)
	}
	if got := cfg.ProviderDisplayName("nvidia-build"); got != "NVIDIA Build" {
		t.Fatalf("NVIDIA label = %q", got)
	}
	if got := cfg.ProviderDisplayName("openrouter"); got != "OpenRouter" {
		t.Fatalf("OpenRouter label = %q", got)
	}
	if got := cfg.ProviderDisplayName("custom-plan"); got != "Team Plan" {
		t.Fatalf("custom label = %q", got)
	}
}

func TestImageGenerationModelMustBeNativeRequestPassthroughModel(t *testing.T) {
	t.Run("canonicalizes alias", func(t *testing.T) {
		cfg, err := Decode(strings.NewReader(`
version: 1
image_generation_model: luna
providers:
  openai: {credential_mode: request_passthrough, responses_mode: native}
models:
  openai/luna: {aliases: [luna], codex: {}}
`))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ImageGenerationModel != "openai/luna" {
			t.Fatalf("image generation model = %q", cfg.ImageGenerationModel)
		}
	})

	t.Run("rejects managed provider", func(t *testing.T) {
		_, err := Decode(strings.NewReader(`
version: 1
image_generation_model: other/chat
providers:
  other: {credential_mode: bifrost, responses_mode: chat_polyfill}
models:
  other/chat: {codex: {}}
`))
		if err == nil || !strings.Contains(err.Error(), "native Responses with request_passthrough") {
			t.Fatalf("expected image model validation error, got %v", err)
		}
	})
}
