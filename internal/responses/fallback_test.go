package responses

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/applyinnovations/bifrost-model-router/internal/config"
)

func fallbackTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{
		Version:                 1,
		HostedToolFallbackModel: "luna",
		Providers: map[string]config.ProviderProfile{
			"openai":     {CredentialMode: config.CredentialRequestPassthrough, ResponsesMode: config.ResponsesNative},
			"managed":    {CredentialMode: config.CredentialBifrost, ResponsesMode: config.ResponsesChatPolyfill},
			"openrouter": {CredentialMode: config.CredentialBifrost, ResponsesMode: config.ResponsesChatPolyfill, NativeHostedTools: map[string]string{"web_search": "openrouter:web_search"}},
			"custom":     {CredentialMode: config.CredentialBifrost, ResponsesMode: config.ResponsesChatPolyfill, NativeHostedTools: map[string]string{"web_search": "web_search"}},
		},
		Models: map[string]config.ModelProfile{
			"openai/luna":                          {Aliases: []string{"luna"}, Codex: config.CodexProfile{}},
			"managed/text-model":                   {Codex: config.CodexProfile{}},
			"openrouter/stealth/space-bunny-alpha": {Codex: config.CodexProfile{}},
			"custom/search-model":                  {Codex: config.CodexProfile{}},
		},
	}
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestApplyHostedToolRoutingSupportsCustomProviderNativeTool(t *testing.T) {
	body := []byte(`{"model":"custom/search-model","tools":[{"type":"web_search","search_context_size":"high"}]}`)
	routed, decision, err := ApplyHostedToolRouting(body, fallbackTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if decision == nil || decision.EffectiveProvider != "custom" || decision.FallbackModel != "" || decision.Reason != "provider_native_hosted_tool:web_search" {
		t.Fatalf("decision = %#v", decision)
	}
	if string(routed) != string(body) {
		var got, want any
		_ = json.Unmarshal(routed, &got)
		_ = json.Unmarshal(body, &want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("routed request = %s", routed)
		}
	}
}

func TestApplyHostedToolFallbackBridgesPlainOpenRouterWebSearch(t *testing.T) {
	body := []byte(`{"model":"openrouter/stealth/space-bunny-alpha","input":"what model are you?","tools":[{"type":"web_search"}]}`)
	cfg := fallbackTestConfig(t)
	// The native OpenRouter bridge does not depend on an OpenAI fallback being
	// configured; only unsupported hosted tools do.
	cfg.HostedToolFallbackModel = ""
	routed, decision, err := ApplyHostedToolRouting(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if decision == nil || decision.FallbackModel != "" || decision.EffectiveProvider != "openrouter" || decision.Reason != "provider_native_hosted_tool:web_search" {
		t.Fatalf("decision = %#v", decision)
	}
	var got struct {
		Model string `json:"model"`
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(routed, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "openrouter/stealth/space-bunny-alpha" || len(got.Tools) != 1 || got.Tools[0].Type != "openrouter:web_search" {
		t.Fatalf("routed request = %#v", got)
	}
}

func TestApplyHostedToolFallbackKeepsWholeRequestFallbackForUnrepresentableSearch(t *testing.T) {
	for _, body := range []string{
		`{"model":"openrouter/stealth/space-bunny-alpha","tools":[{"type":"web_search","search_context_size":"high"}]}`,
		`{"model":"openrouter/stealth/space-bunny-alpha","tools":[{"type":"file_search"}]}`,
		`{"model":"openrouter/stealth/space-bunny-alpha","tools":[{"type":"namespace","tools":[{"type":"web_search"}]}]}`,
	} {
		routed, decision, err := ApplyHostedToolRouting([]byte(body), fallbackTestConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		if decision == nil || decision.FallbackModel != "openai/luna" || !strings.HasPrefix(decision.Reason, "unsupported_hosted_tool:") {
			t.Fatalf("decision = %#v", decision)
		}
		var got struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(routed, &got); err != nil || got.Model != "openai/luna" {
			t.Fatalf("model = %q, err = %v", got.Model, err)
		}
	}
}

func TestApplyHostedToolFallbackRewritesWholeRequest(t *testing.T) {
	body := []byte(`{"model":"managed/text-model","input":"hello","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"shell"}]},{"type":"web_search"}]}`)
	routed, decision, err := ApplyHostedToolRouting(body, fallbackTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if decision == nil || decision.OriginalModel != "managed/text-model" || decision.FallbackModel != "openai/luna" {
		t.Fatalf("decision = %#v", decision)
	}
	if len(decision.ToolTypes) != 1 || decision.ToolTypes[0] != "web_search" {
		t.Fatalf("tool types = %#v", decision.ToolTypes)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(routed, &got); err != nil {
		t.Fatal(err)
	}
	var model string
	if err := json.Unmarshal(got["model"], &model); err != nil || model != "openai/luna" {
		t.Fatalf("model = %q, err = %v", model, err)
	}
	if _, ok := got["input"]; !ok {
		t.Fatal("request fields were not preserved")
	}
}

func TestApplyHostedToolFallbackLeavesNamespaceForCore(t *testing.T) {
	body := []byte(`{"model":"managed/text-model","tools":[{"type":"namespace","name":"codex","tools":[{"type":"function","name":"shell"}]}]}`)
	routed, decision, err := ApplyHostedToolRouting(body, fallbackTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if decision != nil || string(routed) != string(body) {
		t.Fatalf("unexpected fallback: decision=%#v body=%s", decision, routed)
	}
}

func TestApplyHostedToolFallbackFindsNestedHostedTool(t *testing.T) {
	body := []byte(`{"model":"managed/text-model","tools":[{"type":"namespace","name":"mixed","tools":[{"type":"file_search"}]}]}`)
	_, decision, err := ApplyHostedToolRouting(body, fallbackTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if decision == nil || len(decision.ToolTypes) != 1 || decision.ToolTypes[0] != "file_search" {
		t.Fatalf("decision = %#v", decision)
	}
}
