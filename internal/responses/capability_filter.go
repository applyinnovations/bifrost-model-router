package responses

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/applyinnovations/bifrost-model-router/internal/config"
)

const unavailableHostedToolsInstruction = "Hosted tools removed by the router are unavailable for this model. Do not claim to have used them or accessed their results. If the task requires one, tell the user that this model cannot use it."

// HostedToolFilter describes optional hosted tools removed from a
// Chat-Completions-polyfilled request. It is safe to use in routing logs.
type HostedToolFilter struct {
	RequestedModel    string
	EffectiveProvider string
	RemovedToolTypes  []string
}

// FilterUnsupportedHostedTools removes optional server-hosted tools from
// Chat-Completions-polyfilled requests while retaining the selected model.
// Explicitly requiring an unavailable hosted tool fails instead of weakening
// the request's semantics.
func FilterUnsupportedHostedTools(body []byte, cfg config.Config) ([]byte, *HostedToolFilter, *CompatibilityError, error) {
	var envelope struct {
		Model      string          `json:"model"`
		Tools      []any           `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body, nil, nil, err
	}
	resolved, ok := cfg.ResolveModel(envelope.Model)
	if !ok {
		return body, nil, nil, nil
	}

	removed := make(map[string]bool)
	filtered := filterToolList(envelope.Tools, removed, resolved)
	if len(removed) == 0 {
		return body, nil, nil, nil
	}
	types := sortedTypes(removed)
	decision := &HostedToolFilter{
		RequestedModel:    resolved.Slug,
		EffectiveProvider: resolved.Model.Provider,
		RemovedToolTypes:  types,
	}
	if explicitlyRequiresHostedTool(envelope.ToolChoice) {
		return body, decision, compatError("hosted_tool_unsupported", "the selected model cannot execute the explicitly selected hosted tool"), nil
	}
	if toolChoiceString(envelope.ToolChoice) == "required" && len(filtered) == 0 {
		return body, decision, compatError("hosted_tool_unsupported", "the selected model cannot satisfy tool_choice required because only unsupported hosted tools were offered"), nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, nil, nil, err
	}
	if len(filtered) == 0 {
		delete(fields, "tools")
		if choice := toolChoiceString(envelope.ToolChoice); choice == "auto" || choice == "none" {
			delete(fields, "tool_choice")
		}
	} else {
		encoded, err := json.Marshal(filtered)
		if err != nil {
			return body, nil, nil, err
		}
		fields["tools"] = encoded
	}
	if err := appendUnavailableToolInstruction(fields, types); err != nil {
		return body, nil, nil, err
	}
	filteredBody, err := json.Marshal(fields)
	if err != nil {
		return body, nil, nil, err
	}
	return filteredBody, decision, nil, nil
}

func filterToolList(tools []any, removed map[string]bool, resolved config.ResolvedModel) []any {
	filtered := make([]any, 0, len(tools))
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			filtered = append(filtered, value)
			continue
		}
		toolType, _ := tool["type"].(string)
		if shouldFilterHostedTool(toolType, resolved) {
			removed[toolType] = true
			continue
		}
		if nested, ok := tool["tools"].([]any); ok {
			copy := cloneMap(tool)
			copy["tools"] = filterToolList(nested, removed, resolved)
			if len(copy["tools"].([]any)) == 0 {
				continue
			}
			tool = copy
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

func shouldFilterHostedTool(toolType string, resolved config.ResolvedModel) bool {
	if !isHostedToolType(toolType) {
		return false
	}
	if resolved.Model.ResponsesMode == config.ResponsesChatPolyfill {
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(toolType))
	return strings.HasPrefix(normalized, "web_search") && !resolved.Model.Codex.SupportsSearch
}

func cloneMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func explicitlyRequiresHostedTool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(value any) bool {
		switch value := value.(type) {
		case string:
			return isHostedToolType(value)
		case map[string]any:
			if toolType, ok := value["type"].(string); ok && isHostedToolType(toolType) {
				return true
			}
			for _, child := range value {
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range value {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func toolChoiceString(raw json.RawMessage) string {
	var choice string
	_ = json.Unmarshal(raw, &choice)
	return choice
}

func appendUnavailableToolInstruction(fields map[string]json.RawMessage, types []string) error {
	message := unavailableHostedToolsInstruction + " Unavailable tool types: " + strings.Join(types, ", ") + "."
	var existing string
	if raw, ok := fields["instructions"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &existing); err != nil {
			return err
		}
	}
	if existing != "" {
		message = existing + "\n\n" + message
	}
	encoded, err := json.Marshal(message)
	if err == nil {
		fields["instructions"] = encoded
	}
	return err
}

func sortedTypes(types map[string]bool) []string {
	result := make([]string, 0, len(types))
	for toolType := range types {
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
