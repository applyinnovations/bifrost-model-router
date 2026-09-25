package catalog

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/applyinnovations/bifrost-model-router/internal/config"
)

func FuzzHydrateDeterministic(f *testing.F) {
	cfg := testConfig(f)
	f.Add([]byte(`{"data":[{"id":"openai/a","future":{"keep":true}}]}`))
	f.Add([]byte(`{"models":[]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		first, err := Hydrate(body, cfg)
		if err != nil {
			return
		}
		second, err := Hydrate(body, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("hydration is not deterministic")
		}
		var decoded any
		if err := json.Unmarshal(first, &decoded); err != nil {
			t.Fatalf("invalid hydrated JSON: %v", err)
		}
	})
}

func testConfig(t testing.TB) config.Config {
	t.Helper()
	cfg := config.Config{
		Version: 1,
		Providers: map[string]config.ProviderProfile{
			"openai": {CredentialMode: config.CredentialRequestPassthrough, ResponsesMode: config.ResponsesNative},
			"other":  {CredentialMode: config.CredentialBifrost, ResponsesMode: config.ResponsesChatPolyfill},
		},
		Models: map[string]config.ModelProfile{
			"openai/a": {Aliases: []string{"a"}, Codex: config.CodexProfile{ContextWindow: 42}},
			"other/b":  {Aliases: []string{"b"}, Codex: config.CodexProfile{ContextWindow: 84}},
		},
	}
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestHydratePreservesUnknownFieldsAndAddsConfiguredModels(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"id":"a","owned_by":"upstream","future":{"x":1}}]}`)
	out, err := Hydrate(body, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Models) != 2 {
		t.Fatalf("got %d models: %s", len(decoded.Models), out)
	}
	first := decoded.Models[0]
	if first["slug"] != "openai/a" || first["owned_by"] != "upstream" || first["future"] == nil {
		t.Fatalf("unexpected hydrated model: %#v", first)
	}
}

func TestHydrateDisablesSearchForChatPolyfillModels(t *testing.T) {
	body := []byte(`{"models":[{"slug":"openai/a","supports_search_tool":true},{"slug":"other/b","supports_search_tool":true}]}`)
	out, err := Hydrate(body, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	values := map[string]bool{}
	for _, model := range decoded.Models {
		value, _ := model["supports_search_tool"].(bool)
		values[model["slug"].(string)] = value
	}
	if !values["openai/a"] || values["other/b"] {
		t.Fatalf("search capabilities = %#v", values)
	}
}

func TestHydrateIsDeterministic(t *testing.T) {
	cfg := testConfig(t)
	one, err := Hydrate([]byte(`{"data":[]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Hydrate([]byte(`{"data":[]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Fatalf("hydration is not deterministic:\n%s\n%s", one, two)
	}
}

func TestHydrateFlowsThroughDiscoveredModelsAndHidesAbsentOverrides(t *testing.T) {
	cfg := testConfig(t)
	provider := cfg.Providers["other"]
	provider.DiscoverModels = true
	provider.CodexDefaults = config.CodexProfile{ContextWindow: 64000, InputModalities: []string{"text"}}
	cfg.Providers["other"] = provider
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	out, err := Hydrate([]byte(`{"models":[{"slug":"other/new-model","display_name":"New Model","future":{"keep":true}}]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	foundNew, foundAbsentOverride := false, false
	for _, model := range decoded.Models {
		switch model["slug"] {
		case "other/new-model":
			foundNew = model["display_name"] == "New Model (Other)" && model["context_window"] == float64(64000) && model["future"] != nil
		case "other/b":
			foundAbsentOverride = true
		}
	}
	if !foundNew || foundAbsentOverride {
		t.Fatalf("discovered catalog = %s", out)
	}
}

func TestHydrateMapsBifrostNameToCodexDisplayName(t *testing.T) {
	cfg := testConfig(t)
	provider := cfg.Providers["other"]
	provider.DiscoverModels = true
	cfg.Providers["other"] = provider
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	out, err := Hydrate([]byte(`{"models":[{"id":"other/new-model","name":"New Model (fast)"}]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if modelBySlug(decoded.Models, "other/new-model")["display_name"] != "New Model (Other)" {
		t.Fatalf("hydrated model = %s", out)
	}
}

func TestHydratePrefersCodexDisplayNameOverBifrostName(t *testing.T) {
	cfg := testConfig(t)
	provider := cfg.Providers["other"]
	provider.DiscoverModels = true
	cfg.Providers["other"] = provider
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	out, err := Hydrate([]byte(`{"models":[{"id":"other/new-model","name":"Normalized Name","display_name":"Codex Name"}]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if modelBySlug(decoded.Models, "other/new-model")["display_name"] != "Codex Name (Other)" {
		t.Fatalf("hydrated model = %s", out)
	}
}

func modelBySlug(models []map[string]any, slug string) map[string]any {
	for _, model := range models {
		if model["slug"] == slug {
			return model
		}
	}
	return nil
}

func TestHydrateCanonicalizesDiscoveredIDAndAppliesNameOverride(t *testing.T) {
	cfg := testConfig(t)
	provider := cfg.Providers["other"]
	provider.DiscoverModels = true
	provider.ModelNameOverrides = map[string]string{"vendor/reasoner": "Reasoner Pro"}
	cfg.Providers["other"] = provider
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	out, err := Hydrate([]byte(`{"models":[{"id":"other/vendor/reasoner","slug":"other/vendor/reasoner","display_name":"vendor/reasoner"}]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Models) < 1 || decoded.Models[0]["id"] != "other/vendor/reasoner" || decoded.Models[0]["slug"] != "other/vendor/reasoner" || decoded.Models[0]["display_name"] != "Reasoner Pro (Other)" {
		t.Fatalf("hydrated model = %s", out)
	}
}

func TestHydratePreservesUpstreamContextAndGeneratesAdjacentVariant(t *testing.T) {
	cfg := testConfig(t)
	model := cfg.Models["openai/a"]
	model.Codex.MaxContextWindow = 872000
	model.ContextVariants = []config.ContextVariant{{ContextWindow: 872000, EffectiveContextWindowPercent: 95}}
	cfg.Models["openai/a"] = model
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"models":[{"slug":"a","display_name":"Upstream A","description":"upstream","context_window":272000,"max_context_window":872000,"effective_context_window_percent":80,"supports_experimental_context":true,"future":{"keep":true}}]}`)
	out, err := Hydrate(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Models) != 3 {
		t.Fatalf("models = %s", out)
	}
	base, variant := decoded.Models[0], decoded.Models[1]
	if base["context_window"] != float64(272000) || base["max_context_window"] != float64(872000) || base["effective_context_window_percent"] != float64(80) || base["supports_experimental_context"] != true || base["future"] == nil {
		t.Fatalf("base metadata was overwritten: %#v", base)
	}
	if variant["slug"] != "a-872k" || variant["display_name"] != "Upstream A (872K)" || variant["context_window"] != float64(872000) || variant["max_context_window"] != float64(872000) || variant["effective_context_window_percent"] != float64(95) {
		t.Fatalf("variant = %#v", variant)
	}
}

func TestHydratePrefersEditorialNameAndUsesConfiguredProviderLabel(t *testing.T) {
	cfg := testConfig(t)
	provider := cfg.Providers["other"]
	provider.DisplayName = "Paid Plan"
	provider.DiscoverModels = true
	cfg.Providers["other"] = provider
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	out, err := HydrateWithNames(
		[]byte(`{"models":[{"id":"other/new-model","name":"new-model (fast)"}]}`),
		cfg,
		func(provider, upstream string) (string, bool) {
			return "Publisher: New Model", provider == "other" && upstream == "new-model"
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := modelBySlug(decoded.Models, "other/new-model")["display_name"]; got != "New Model (Paid Plan)" {
		t.Fatalf("display_name = %q", got)
	}
}

func TestDecorateCatalogPreservesEnvelopeAndUsesEditorialName(t *testing.T) {
	cfg := testConfig(t)
	body := []byte(`{"models":[{"slug":"other/b","display_name":"b [Other]"}],"recommended_model":"other/b"}`)
	out, err := DecorateCatalog(body, cfg, func(provider, upstream string) (string, int64, bool) {
		return "Publisher: Better Name", 1000000, provider == "other" && upstream == "b"
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models           []map[string]any `json:"models"`
		RecommendedModel string           `json:"recommended_model"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RecommendedModel != "other/b" || modelBySlug(decoded.Models, "other/b")["display_name"] != "Better Name (Other)" {
		t.Fatalf("decorated catalog = %s", out)
	}
}

func TestHydrateUsesDynamicContextOnlyWhenProviderOmitsIt(t *testing.T) {
	cfg := testConfig(t)
	provider := cfg.Providers["other"]
	provider.DiscoverModels = true
	provider.CodexDefaults = config.CodexProfile{ContextWindow: 131072, InputModalities: []string{"text"}}
	cfg.Providers["other"] = provider
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	lookup := func(_, _ string) (string, int64, bool) {
		return "Publisher: Dynamic", 1000000, true
	}
	out, err := HydrateWithMetadata([]byte(`{"models":[
		{"slug":"other/dynamic","display_name":"dynamic"},
		{"slug":"other/provider-value","display_name":"provider-value","context_window":262144,"max_context_window":524288}
	]}`), cfg, lookup)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	dynamic := modelBySlug(decoded.Models, "other/dynamic")
	providerValue := modelBySlug(decoded.Models, "other/provider-value")
	if dynamic["context_window"] != float64(1000000) || dynamic["max_context_window"] != float64(1000000) {
		t.Fatalf("dynamic metadata = %#v", dynamic)
	}
	if providerValue["context_window"] != float64(262144) || providerValue["max_context_window"] != float64(524288) {
		t.Fatalf("provider metadata was overwritten: %#v", providerValue)
	}
}

func TestDecorateModelHumanizesProviderFallbackAndSource(t *testing.T) {
	cfg := testConfig(t)
	provider := cfg.Providers["other"]
	provider.DisplayName = "VokeAPI"
	provider.DiscoverModels = true
	cfg.Providers["other"] = provider
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	resolved, ok := cfg.ResolveModel("other/grok-4.20-0309-non-reasoning")
	if !ok {
		t.Fatal("model did not resolve")
	}
	model := map[string]any{"display_name": "grok-4.20-0309-non-reasoning (fast) [VokeAPI]"}
	DecorateModel(model, resolved, cfg, nil)
	if got := model["display_name"]; got != "Grok 4.20 0309 Non-Reasoning (VokeAPI)" {
		t.Fatalf("display_name = %q", got)
	}
}

func TestDecorateModelOmitsDirectOpenAISource(t *testing.T) {
	cfg := testConfig(t)
	resolved, ok := cfg.ResolveModel("openai/a")
	if !ok {
		t.Fatal("model did not resolve")
	}
	model := map[string]any{"display_name": "stale [OpenAI]"}
	DecorateModel(model, resolved, cfg, func(_, _ string) (string, bool) {
		return "OpenAI: GPT-5.6 Sol", true
	})
	if got := model["display_name"]; got != "GPT-5.6 Sol" {
		t.Fatalf("display_name = %q", got)
	}
}
