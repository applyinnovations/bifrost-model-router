package responses

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/applyinnovations/bifrost-model-router/internal/config"
	"github.com/maximhq/bifrost/core/schemas"
)

// HostedToolRoute describes a hosted-tool routing decision. The caller's
// credential is selected separately by the transport credential policy.
type HostedToolRoute struct {
	OriginalModel string
	FallbackModel string
	// EffectiveProvider is the provider selected by this routing decision.
	EffectiveProvider string
	// Reason is safe to record in routing logs. It contains no request content.
	Reason    string
	ToolTypes []string
}

type rawTool struct {
	Type  string    `json:"type"`
	Tools []rawTool `json:"tools,omitempty"`
}

// ApplyHostedToolRouting routes hosted tools offered to Chat Completions
// polyfills. OpenRouter can execute a plain web_search through its native
// Responses API, where the model still decides whether to search. Other hosted
// tools retain the whole-request fallback because silently removing them would
// change the request's semantics.
func ApplyHostedToolRouting(body []byte, cfg config.Config) ([]byte, *HostedToolRoute, error) {
	var envelope struct {
		Model string    `json:"model"`
		Tools []rawTool `json:"tools"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body, nil, err
	}
	requested, ok := cfg.ResolveModel(envelope.Model)
	if !ok || requested.Model.ResponsesMode != config.ResponsesChatPolyfill {
		return body, nil, nil
	}

	types := hostedToolTypes(envelope.Tools)
	if len(types) == 0 {
		return body, nil, nil
	}
	if requested.Model.Provider == "openrouter" && canBridgeOpenRouterWebSearch(body, envelope.Tools) {
		routed, err := rewriteOpenRouterWebSearch(body)
		if err != nil {
			return body, nil, err
		}
		return routed, &HostedToolRoute{
			OriginalModel:     envelope.Model,
			EffectiveProvider: "openrouter",
			Reason:            "openrouter_native_web_search",
			ToolTypes:         types,
		}, nil
	}
	if cfg.HostedToolFallbackModel == "" {
		return body, nil, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, nil, err
	}
	encodedModel, err := json.Marshal(cfg.HostedToolFallbackModel)
	if err != nil {
		return body, nil, err
	}
	fields["model"] = encodedModel
	routed, err := json.Marshal(fields)
	if err != nil {
		return body, nil, err
	}
	return routed, &HostedToolRoute{
		OriginalModel:     envelope.Model,
		FallbackModel:     cfg.HostedToolFallbackModel,
		EffectiveProvider: "openai",
		Reason:            "unsupported_hosted_tool:" + strings.Join(types, ","),
		ToolTypes:         types,
	}, nil
}

// canBridgeOpenRouterWebSearch deliberately accepts only the parameter-free,
// top-level web_search shape Codex currently offers. OpenRouter's equivalent is
// an openrouter:web_search server tool. Other OpenAI web-search options do not
// have a lossless representation in Bifrost's neutral OpenRouter tool type, so
// those requests must use the existing OpenAI fallback.
func canBridgeOpenRouterWebSearch(body []byte, tools []rawTool) bool {
	if len(tools) == 0 {
		return false
	}
	var envelope struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Tools) != len(tools) {
		return false
	}
	for i, tool := range tools {
		if isHostedToolType(tool.Type) {
			if strings.TrimSpace(tool.Type) != "web_search" || len(tool.Tools) != 0 || len(envelope.Tools[i]) != 1 {
				return false
			}
		}
		if len(tool.Tools) > 0 && len(hostedToolTypes(tool.Tools)) > 0 {
			return false
		}
	}
	return true
}

func rewriteOpenRouterWebSearch(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(fields["tools"], &tools); err != nil {
		return nil, err
	}
	for _, tool := range tools {
		var toolType string
		if err := json.Unmarshal(tool["type"], &toolType); err != nil {
			return nil, err
		}
		if toolType == "web_search" {
			encoded, err := json.Marshal("openrouter:web_search")
			if err != nil {
				return nil, err
			}
			tool["type"] = encoded
		}
	}
	encodedTools, err := json.Marshal(tools)
	if err != nil {
		return nil, err
	}
	fields["tools"] = encodedTools
	return json.Marshal(fields)
}

// UsesOpenRouterNativeWebSearch reports whether a request was bridged to
// OpenRouter's server-side search tool and therefore must stay on the native
// Responses wire instead of being converted to Chat Completions.
func UsesOpenRouterNativeWebSearch(req *schemas.BifrostResponsesRequest) bool {
	if req == nil || req.Provider != schemas.OpenRouter || req.Params == nil {
		return false
	}
	for _, tool := range req.Params.Tools {
		if tool.Type == schemas.ResponsesToolType("openrouter:web_search") {
			return true
		}
	}
	return false
}

func hostedToolTypes(tools []rawTool) []string {
	seen := make(map[string]bool)
	var visit func([]rawTool)
	visit = func(items []rawTool) {
		for _, tool := range items {
			if isHostedToolType(tool.Type) {
				seen[tool.Type] = true
			}
			visit(tool.Tools)
		}
	}
	visit(tools)

	result := make([]string, 0, len(seen))
	for toolType := range seen {
		result = append(result, toolType)
	}
	sort.Strings(result)
	return result
}

func isHostedToolType(toolType string) bool {
	t := strings.ToLower(strings.TrimSpace(toolType))
	switch t {
	case "file_search", "computer_use_preview", "web_fetch", "mcp",
		"code_interpreter", "image_generation", "memory", "tool_search",
		"x_search", "advisor":
		return true
	}
	return strings.HasPrefix(t, "web_search") ||
		strings.HasPrefix(t, "computer_") ||
		strings.HasPrefix(t, "code_execution") ||
		strings.HasPrefix(t, "tool_search_tool_") ||
		strings.HasPrefix(t, "memory_") ||
		strings.HasPrefix(t, "advisor_")
}
