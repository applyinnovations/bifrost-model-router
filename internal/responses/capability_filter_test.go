package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/applyinnovations/bifrost-model-router/internal/config"
)

func filterTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{
		Version: 1,
		Providers: map[string]config.ProviderProfile{
			"openai":  {CredentialMode: config.CredentialRequestPassthrough, ResponsesMode: config.ResponsesNative},
			"managed": {CredentialMode: config.CredentialBifrost, ResponsesMode: config.ResponsesChatPolyfill},
		},
		Models: map[string]config.ModelProfile{
			"openai/native":      {Codex: config.CodexProfile{SupportsSearch: true}},
			"managed/text-model": {Codex: config.CodexProfile{}},
		},
	}
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestFilterUnsupportedHostedToolsKeepsSelectedModel(t *testing.T) {
	body := []byte(`{"model":"managed/text-model","input":"hello","instructions":"be concise","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"shell"}]},{"type":"web_search"}]}`)
	filtered, decision, compatErr, err := FilterUnsupportedHostedTools(body, filterTestConfig(t))
	if err != nil || compatErr != nil {
		t.Fatalf("compat=%v err=%v", compatErr, err)
	}
	if decision == nil || decision.RequestedModel != "managed/text-model" || decision.EffectiveProvider != "managed" || strings.Join(decision.RemovedToolTypes, ",") != "web_search" {
		t.Fatalf("decision = %#v", decision)
	}
	var got struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Tools        []struct {
			Type  string `json:"type"`
			Tools []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(filtered, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "managed/text-model" || len(got.Tools) != 1 || got.Tools[0].Type != "namespace" || len(got.Tools[0].Tools) != 1 || got.Tools[0].Tools[0].Name != "shell" {
		t.Fatalf("filtered request = %#v", got)
	}
	if !strings.Contains(got.Instructions, "be concise") || !strings.Contains(got.Instructions, "web_search") || !strings.Contains(got.Instructions, "Do not claim") {
		t.Fatalf("instructions = %q", got.Instructions)
	}
}

func TestFilterUnsupportedHostedToolsRejectsExplicitChoice(t *testing.T) {
	tests := []string{
		`{"model":"managed/text-model","tools":[{"type":"web_search"}],"tool_choice":{"type":"web_search"}}`,
		`{"model":"managed/text-model","tools":[{"type":"web_search"}],"tool_choice":"required"}`,
	}
	for _, body := range tests {
		_, _, compatErr, err := FilterUnsupportedHostedTools([]byte(body), filterTestConfig(t))
		if err != nil || compatErr == nil || compatErr.Code != "hosted_tool_unsupported" {
			t.Fatalf("body=%s compat=%v err=%v", body, compatErr, err)
		}
	}
}

func TestFilterUnsupportedHostedToolsAllowsRequiredFunction(t *testing.T) {
	body := []byte(`{"model":"managed/text-model","tools":[{"type":"function","name":"shell"},{"type":"web_search"}],"tool_choice":"required"}`)
	filtered, decision, compatErr, err := FilterUnsupportedHostedTools(body, filterTestConfig(t))
	if err != nil || compatErr != nil || decision == nil {
		t.Fatalf("decision=%#v compat=%v err=%v", decision, compatErr, err)
	}
	if !strings.Contains(string(filtered), `"name":"shell"`) || strings.Contains(string(filtered), `"type":"web_search"`) {
		t.Fatalf("filtered = %s", filtered)
	}
}

func TestFilterUnsupportedHostedToolsPassesNativeResponsesUnchanged(t *testing.T) {
	body := []byte(`{"model":"openai/native","tools":[{"type":"web_search"}]}`)
	filtered, decision, compatErr, err := FilterUnsupportedHostedTools(body, filterTestConfig(t))
	if err != nil || compatErr != nil || decision != nil || string(filtered) != string(body) {
		t.Fatalf("filtered=%s decision=%#v compat=%v err=%v", filtered, decision, compatErr, err)
	}
}

func TestFilterUnsupportedHostedToolsFiltersSearchFromNativeModelWithoutCapability(t *testing.T) {
	cfg := filterTestConfig(t)
	model := cfg.Models["openai/native"]
	model.Codex.SupportsSearch = false
	cfg.Models["openai/native"] = model
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"openai/native","tools":[{"type":"web_search"}]}`)
	filtered, decision, compatErr, err := FilterUnsupportedHostedTools(body, cfg)
	if err != nil || compatErr != nil || decision == nil || strings.Contains(string(filtered), `"type":"web_search"`) {
		t.Fatalf("filtered=%s decision=%#v compat=%v err=%v", filtered, decision, compatErr, err)
	}
}

func TestFilterUnsupportedHostedToolsRemovesNestedHostedTool(t *testing.T) {
	body := []byte(`{"model":"managed/text-model","tools":[{"type":"namespace","name":"mixed","tools":[{"type":"function","name":"shell"},{"type":"file_search"}]}]}`)
	filtered, decision, compatErr, err := FilterUnsupportedHostedTools(body, filterTestConfig(t))
	if err != nil || compatErr != nil || decision == nil || strings.Join(decision.RemovedToolTypes, ",") != "file_search" {
		t.Fatalf("decision=%#v compat=%v err=%v", decision, compatErr, err)
	}
	if strings.Contains(string(filtered), "file_search") && !strings.Contains(string(filtered), "Unavailable tool types: file_search") {
		t.Fatalf("filtered = %s", filtered)
	}
}
