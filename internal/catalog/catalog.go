package catalog

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/applyinnovations/bifrost-model-router/internal/config"
)

type responseEnvelope struct {
	Models []map[string]any `json:"models"`
}

type NameLookup func(provider, upstreamModel string) (string, bool)
type MetadataLookup func(provider, upstreamModel string) (name string, contextWindow int64, ok bool)

func Hydrate(body []byte, cfg config.Config) ([]byte, error) {
	return HydrateWithMetadata(body, cfg, nil)
}

func HydrateWithNames(body []byte, cfg config.Config, lookup NameLookup) ([]byte, error) {
	return HydrateWithMetadata(body, cfg, metadataFromNames(lookup))
}

// HydrateWithMetadata preserves provider-returned catalog fields, dynamically
// fills missing context metadata, and applies display-name enrichment.
func HydrateWithMetadata(body []byte, cfg config.Config, lookup MetadataLookup) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}

	models, err := decodeModels(raw)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	out := make([]map[string]any, 0, len(models)+len(cfg.Models))
	for _, original := range models {
		name := stringField(original, "slug")
		if name == "" {
			name = stringField(original, "id")
		}
		resolved, ok := cfg.ResolveModel(name)
		if !ok {
			continue
		}
		if seen[resolved.Slug] {
			continue
		}
		base := HydrateModel(enrichContext(original, resolved, lookup), resolved, cfg.Instructions)
		out = append(out, base)
		seen[resolved.Slug] = true
		if resolved.Variant == nil {
			out = appendConfiguredVariants(out, seen, base, resolved.BaseSlug, cfg, cfg.Instructions)
		}
	}
	for _, slug := range cfg.ModelNames() {
		if seen[slug] {
			continue
		}
		resolved, _ := cfg.ResolveModel(slug)
		if resolved.Provider.DiscoverModels {
			// With account-aware discovery enabled, an absent configured model is
			// metadata-only and must not be advertised until the upstream lists it.
			continue
		}
		base := HydrateModel(enrichContext(map[string]any{"id": slug}, resolved, lookup), resolved, cfg.Instructions)
		out = append(out, base)
		seen[slug] = true
		out = appendConfiguredVariants(out, seen, base, slug, cfg, cfg.Instructions)
	}
	for _, model := range out {
		identity := stringField(model, "slug")
		if identity == "" {
			identity = stringField(model, "id")
		}
		if resolved, ok := cfg.ResolveModel(identity); ok {
			DecorateModelWithMetadata(model, resolved, cfg, lookup)
		}
	}
	encoded, err := json.Marshal(responseEnvelope{Models: out})
	if err != nil {
		return nil, fmt.Errorf("encode hydrated catalog: %w", err)
	}
	return encoded, nil
}

// DecorateCatalog applies display-name enrichment to an already hydrated
// Codex catalog while preserving its envelope and all unknown fields.
func DecorateCatalog(body []byte, cfg config.Config, lookup MetadataLookup) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	models, err := decodeModels(raw)
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		identity := stringField(model, "slug")
		if identity == "" {
			identity = stringField(model, "id")
		}
		if resolved, ok := cfg.ResolveModel(identity); ok {
			DecorateModelWithMetadata(model, resolved, cfg, lookup)
		}
	}
	encoded, err := json.Marshal(models)
	if err != nil {
		return nil, fmt.Errorf("encode model entries: %w", err)
	}
	raw["models"] = encoded
	delete(raw, "data")
	return json.Marshal(raw)
}

// DecorateModel applies editorial naming and a configured-provider suffix.
func DecorateModel(model map[string]any, resolved config.ResolvedModel, cfg config.Config, lookup NameLookup) {
	DecorateModelWithMetadata(model, resolved, cfg, metadataFromNames(lookup))
}

// DecorateModelWithMetadata applies dynamic naming, fills a missing context
// window, and adds the configured-provider suffix.
func DecorateModelWithMetadata(model map[string]any, resolved config.ResolvedModel, cfg config.Config, lookup MetadataLookup) {
	enrichContext(model, resolved, lookup)
	providerLabel := cfg.ProviderDisplayName(resolved.Model.Provider)
	current := strings.TrimSpace(stringField(model, "display_name"))
	current = stripProviderSuffix(current, providerLabel)
	name := current
	if override := strings.TrimSpace(resolved.Provider.ModelNameOverrides[resolved.UpstreamModel]); override != "" {
		name = override
	} else if lookup != nil {
		if editorial, _, ok := lookup(resolved.Model.Provider, resolved.UpstreamModel); ok && strings.TrimSpace(editorial) != "" {
			name = normalizeEditorialName(editorial, resolved.UpstreamModel)
		}
	}
	if name == "" {
		name = resolved.UpstreamModel
	}
	if name == current {
		name = readableProviderName(name, resolved.UpstreamModel)
	}
	if resolved.Variant != nil {
		variantSuffix := " (" + contextLabel(resolved.Variant.ContextWindow) + ")"
		if !strings.HasSuffix(name, variantSuffix) {
			name += variantSuffix
		}
	}
	if resolved.Model.Provider != "openai" {
		name += " (" + providerLabel + ")"
	}
	model["display_name"] = name
}

func metadataFromNames(lookup NameLookup) MetadataLookup {
	if lookup == nil {
		return nil
	}
	return func(provider, upstreamModel string) (string, int64, bool) {
		name, ok := lookup(provider, upstreamModel)
		return name, 0, ok
	}
}

func enrichContext(model map[string]any, resolved config.ResolvedModel, lookup MetadataLookup) map[string]any {
	if positiveInteger(model["context_window"]) > 0 || lookup == nil {
		return model
	}
	window := positiveInteger(model["max_context_window"])
	if window == 0 {
		_, window, _ = lookup(resolved.Model.Provider, resolved.UpstreamModel)
	}
	if window <= 0 {
		return model
	}
	model["context_window"] = window
	if _, exists := model["max_context_window"]; !exists {
		model["max_context_window"] = window
	}
	return model
}

func positiveInteger(value any) int64 {
	switch number := value.(type) {
	case float64:
		if number > 0 && number == float64(int64(number)) {
			return int64(number)
		}
	case int64:
		if number > 0 {
			return number
		}
	case int:
		if number > 0 {
			return int64(number)
		}
	}
	return 0
}

func stripProviderSuffix(name, providerLabel string) string {
	name = strings.TrimSpace(name)
	for _, suffix := range []string{" [" + providerLabel + "]", " (" + providerLabel + ")"} {
		name = strings.TrimSpace(strings.TrimSuffix(name, suffix))
	}
	return name
}

func normalizeEditorialName(name, upstreamModel string) string {
	name = strings.TrimSpace(name)
	publisher, modelName, found := strings.Cut(name, ":")
	if !found || strings.TrimSpace(publisher) == "" || strings.TrimSpace(modelName) == "" {
		return readableProviderName(name, upstreamModel)
	}
	modelName = strings.TrimSpace(modelName)
	if strings.EqualFold(modelName, modelLeaf(upstreamModel)) {
		modelName = humanizeModelID(modelName)
	}
	return modelName
}

func readableProviderName(name, upstreamModel string) string {
	name = strings.TrimSpace(name)
	for _, tier := range []string{"fast", "frontier", "flagship", "open-weights"} {
		suffix := " (" + tier + ")"
		if strings.HasSuffix(strings.ToLower(name), suffix) {
			name = strings.TrimSpace(name[:len(name)-len(suffix)])
			break
		}
	}
	if strings.EqualFold(name, upstreamModel) || strings.EqualFold(name, modelLeaf(upstreamModel)) || name == "" {
		return humanizeModelID(modelLeaf(upstreamModel))
	}
	return name
}

func modelLeaf(model string) string {
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		return model[slash+1:]
	}
	return model
}

func humanizeModelID(model string) string {
	parts := strings.FieldsFunc(model, func(r rune) bool { return r == '-' || r == '_' || r == '/' })
	out := make([]string, 0, len(parts))
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if strings.EqualFold(part, "non") && i+1 < len(parts) {
			out = append(out, "Non-"+humanizeToken(parts[i+1]))
			i++
			continue
		}
		out = append(out, humanizeToken(part))
	}
	return strings.Join(out, " ")
}

func humanizeToken(token string) string {
	lower := strings.ToLower(token)
	brands := map[string]string{
		"ai": "AI", "api": "API", "deepseek": "DeepSeek", "gemini": "Gemini",
		"glm": "GLM", "gpt": "GPT", "grok": "Grok", "hy3": "Hy3",
		"kimi": "Kimi", "mimo": "MiMo", "minimax": "MiniMax", "oss": "OSS",
	}
	if brand := brands[lower]; brand != "" {
		return brand
	}
	if len(lower) > 1 {
		last := lower[len(lower)-1]
		if last == 'b' || last == 'm' || last == 'k' {
			prefix := lower[:len(lower)-1]
			allAlphaNumeric := true
			for _, r := range prefix {
				if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' {
					allAlphaNumeric = false
					break
				}
			}
			if allAlphaNumeric {
				return strings.ToUpper(prefix[:1]) + prefix[1:] + strings.ToUpper(string(last))
			}
		}
	}
	if len(lower) > 1 && lower[0] == 'v' && lower[1] >= '0' && lower[1] <= '9' {
		return "V" + lower[1:]
	}
	runes := []rune(lower)
	if len(runes) == 0 {
		return ""
	}
	runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
	return string(runes)
}

func decodeModels(raw map[string]json.RawMessage) ([]map[string]any, error) {
	field := raw["models"]
	if len(field) == 0 {
		field = raw["data"]
	}
	if len(field) == 0 {
		return []map[string]any{}, nil
	}
	var models []map[string]any
	if err := json.Unmarshal(field, &models); err != nil {
		return nil, fmt.Errorf("decode model entries: %w", err)
	}
	return models, nil
}

func appendConfiguredVariants(out []map[string]any, seen map[string]bool, base map[string]any, baseSlug string, cfg config.Config, instructions string) []map[string]any {
	for _, resolved := range cfg.ResolvedModels() {
		if resolved.BaseSlug != baseSlug || resolved.Variant == nil || seen[resolved.Slug] {
			continue
		}
		out = append(out, HydrateVariant(base, resolved, instructions))
		seen[resolved.Slug] = true
	}
	return out
}

// HydrateModel fills missing Codex catalog fields while preserving every field
// supplied by the upstream provider. Identity is normalized to the configured
// base slug for provider catalogs that only return a bare model ID.
func HydrateModel(original map[string]any, resolved config.ResolvedModel, instructions string) map[string]any {
	result := make(map[string]any, len(original)+32)
	for key, value := range original {
		result[key] = value
	}
	p := resolved.Model.Codex
	result["slug"] = resolved.Slug
	// The canonical ID prevents model IDs that contain their own slash (for
	// example vendor/model) from being mistaken for a Bifrost provider by
	// Codex on the next request.
	result["id"] = resolved.Slug
	if displayName := resolved.Provider.ModelNameOverrides[resolved.UpstreamModel]; displayName != "" {
		result["display_name"] = displayName
	} else if displayName := stringField(result, "display_name"); strings.TrimSpace(displayName) != "" {
		result["display_name"] = displayName
	} else if name := stringField(result, "name"); strings.TrimSpace(name) != "" {
		// Bifrost's normalized model schema exposes an upstream OpenAI-compatible
		// display_name as name. Codex only reads display_name in its model picker,
		// so bridge the normalized field back to the Codex catalog shape.
		result["display_name"] = name
	} else {
		result["display_name"] = p.DisplayName
	}
	setMissing(result, "description", p.Description)
	setMissing(result, "default_reasoning_level", p.DefaultReasoningLevel)
	setMissing(result, "supported_reasoning_levels", p.SupportedReasoningLevels)
	setMissing(result, "shell_type", "unified_exec")
	setMissing(result, "visibility", "list")
	setMissing(result, "supported_in_api", true)
	setMissing(result, "priority", 1)
	setMissing(result, "additional_speed_tiers", []string{})
	setMissing(result, "service_tiers", []any{})
	setMissing(result, "availability_nux", nil)
	setMissing(result, "upgrade", nil)
	setMissing(result, "base_instructions", instructions)
	setMissing(result, "model_messages", map[string]any{"instructions_template": instructions})
	setMissing(result, "include_skills_usage_instructions", false)
	setMissing(result, "include_plugin_usage_instructions", false)
	setMissing(result, "include_apps_usage_instructions", false)
	setMissing(result, "supports_reasoning_summary_parameter", p.SupportsReasoningSummaries)
	setMissing(result, "default_reasoning_summary", "auto")
	setMissing(result, "support_verbosity", p.SupportsVerbosity)
	setMissing(result, "truncation_policy", map[string]any{"mode": "bytes", "limit": p.TruncationLimit})
	setMissing(result, "supports_image_detail_original", p.SupportsImageDetailOriginal)
	setMissing(result, "context_window", p.ContextWindow)
	setMissing(result, "max_context_window", p.MaxContextWindow)
	setMissing(result, "effective_context_window_percent", p.EffectiveContextWindowPercent)
	setMissing(result, "experimental_supported_tools", []string{})
	setMissing(result, "input_modalities", p.InputModalities)
	if resolved.Model.ResponsesMode == config.ResponsesChatPolyfill {
		result["supports_search_tool"] = false
	} else {
		setMissing(result, "supports_search_tool", p.SupportsSearch)
	}
	setMissing(result, "supports_experimental_context", false)
	setMissing(result, "use_responses_lite", false)
	setMissing(result, "node_repl_auto_review_required", false)
	setMissing(result, "node_repl_disabled", false)
	setMissing(result, "responses_mode", resolved.Model.ResponsesMode)
	return result
}

// HydrateVariant derives an opt-in context entry from an already hydrated base.
func HydrateVariant(base map[string]any, resolved config.ResolvedModel, instructions string) map[string]any {
	result := HydrateModel(base, resolved, instructions)
	window := resolved.Model.Codex.ContextWindow
	result["slug"] = resolved.Slug
	result["display_name"] = strings.TrimSpace(stringField(base, "display_name")) + " (" + contextLabel(window) + ")"
	result["context_window"] = window
	result["max_context_window"] = window
	result["effective_context_window_percent"] = resolved.Model.Codex.EffectiveContextWindowPercent
	return result
}

func setMissing(object map[string]any, key string, value any) {
	if _, exists := object[key]; !exists {
		object[key] = value
	}
}

func contextLabel(window int64) string {
	if window >= 1000000 && window%1000000 == 0 {
		return fmt.Sprintf("%dM", window/1000000)
	}
	return fmt.Sprintf("%dK", window/1000)
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}
