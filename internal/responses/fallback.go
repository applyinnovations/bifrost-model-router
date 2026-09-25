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
// polyfills. A provider can declare hosted-tool types that it executes through
// its native Responses API. Other hosted tools retain the whole-request
// fallback because silently removing them would change the request's semantics.
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
	if canUseNativeHostedTools(body, envelope.Tools, requested.Provider.NativeHostedTools) {
		routed, err := rewriteNativeHostedTools(body, requested.Provider.NativeHostedTools)
		if err != nil {
			return body, nil, err
		}
		return routed, &HostedToolRoute{
			OriginalModel:     envelope.Model,
			EffectiveProvider: requested.Model.Provider,
			Reason:            "provider_native_hosted_tool:" + strings.Join(types, ","),
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

// canUseNativeHostedTools requires every hosted tool to have a provider-declared
// mapping. A renamed tool must be parameter-free because Bifrost cannot
// losslessly move provider-specific parameters between different tool schemas.
// An unchanged standard Responses tool keeps its parameters intact.
func canUseNativeHostedTools(body []byte, tools []rawTool, routes map[string]string) bool {
	if len(tools) == 0 || len(routes) == 0 {
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
			upstreamType, ok := routes[tool.Type]
			if !ok || len(tool.Tools) != 0 || (upstreamType != tool.Type && len(envelope.Tools[i]) != 1) {
				return false
			}
		}
		if len(tool.Tools) > 0 && len(hostedToolTypes(tool.Tools)) > 0 {
			return false
		}
	}
	return true
}

func rewriteNativeHostedTools(body []byte, routes map[string]string) ([]byte, error) {
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
		if upstreamType, ok := routes[toolType]; ok {
			encoded, err := json.Marshal(upstreamType)
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

// UsesNativeHostedTools reports whether a parsed request contains a complete
// provider-declared hosted-tool route and therefore must stay on the native
// Responses wire instead of being converted to Chat Completions.
func UsesNativeHostedTools(req *schemas.BifrostResponsesRequest, routes map[string]string) bool {
	if req == nil || req.Params == nil || len(routes) == 0 {
		return false
	}
	targets := make(map[schemas.ResponsesToolType]bool, len(routes))
	for _, target := range routes {
		targets[schemas.ResponsesToolType(target)] = true
	}
	found := false
	var visit func([]schemas.ResponsesTool, bool) bool
	visit = func(tools []schemas.ResponsesTool, nested bool) bool {
		for _, tool := range tools {
			if targets[tool.Type] {
				if nested {
					return false
				}
				found = true
			} else if isHostedToolType(string(tool.Type)) {
				return false
			}
			if tool.ResponsesToolNamespace != nil && !visit(tool.ResponsesToolNamespace.Tools, true) {
				return false
			}
		}
		return true
	}
	return visit(req.Params.Tools, false) && found
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
