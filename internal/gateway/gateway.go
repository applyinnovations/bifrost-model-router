package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/applyinnovations/bifrost-model-router/internal/catalog"
	"github.com/applyinnovations/bifrost-model-router/internal/config"
	"github.com/applyinnovations/bifrost-model-router/internal/editorial"
	responsescompat "github.com/applyinnovations/bifrost-model-router/internal/responses"
)

const chatGPTResponsesPath = "/chatgpt_passthrough/backend-api/codex/responses"

// Handler dispatches Codex wire requests without translating native OpenAI
// traffic. Bifrost's dedicated ChatGPT passthrough owns the raw OpenAI path;
// all managed providers continue through Bifrost's normal Responses endpoint.
type Handler struct {
	cfg             config.Config
	bifrostURL      *url.URL
	chatGPTURL      *url.URL
	bifrostProxy    *httputil.ReverseProxy
	client          *http.Client
	chatGPTModelURL string
	metadataLookup  catalog.MetadataLookup
}

func New(cfg config.Config, bifrostURL, chatGPTURL string) (*Handler, error) {
	bifrost, err := url.Parse(bifrostURL)
	if err != nil || bifrost.Scheme == "" || bifrost.Host == "" {
		return nil, fmt.Errorf("invalid Bifrost URL %q", bifrostURL)
	}
	chatGPT, err := url.Parse(chatGPTURL)
	if err != nil || chatGPT.Scheme == "" || chatGPT.Host == "" {
		return nil, fmt.Errorf("invalid ChatGPT URL %q", chatGPTURL)
	}
	proxy := httputil.NewSingleHostReverseProxy(bifrost)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, proxyErr error) {
		writeError(w, http.StatusBadGateway, "upstream_unavailable", proxyErr.Error())
	}
	return &Handler{
		cfg:             cfg,
		bifrostURL:      bifrost,
		chatGPTURL:      chatGPT,
		bifrostProxy:    proxy,
		client:          &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		chatGPTModelURL: "/backend-api/codex/models",
		metadataLookup:  editorial.NewOpenRouterResolver().LookupMetadata,
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/v1/responses":
		h.serveResponses(w, req)
	case req.Method == http.MethodPost && req.URL.Path == "/v1/images/generations":
		h.serveImageGeneration(w, req)
	case req.Method == http.MethodGet && req.URL.Path == "/v1/models" && req.URL.Query().Get("client_version") != "":
		h.serveModels(w, req)
	default:
		h.proxyBifrost(w, req, req.URL.Path, nil)
	}
}

func (h *Handler) serveResponses(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read request body")
		return
	}
	routed, filtering, compatErr, err := responsescompat.FilterUnsupportedHostedTools(body, h.cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	if compatErr != nil {
		if filtering != nil {
			log.Printf("responses_route requested_model=%q effective_provider=%q capability_action=%q", filtering.RequestedModel, filtering.EffectiveProvider, "rejected_required_hosted_tools:"+strings.Join(filtering.RemovedToolTypes, ","))
		}
		writeError(w, http.StatusBadRequest, compatErr.Code, compatErr.Message)
		return
	}
	resolved, err := h.resolveRequestModel(routed)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unresolved_model", err.Error())
		return
	}
	action := "none"
	if filtering != nil {
		action = "removed_optional_hosted_tools:" + strings.Join(filtering.RemovedToolTypes, ",")
		w.Header().Set("X-Bifrost-Removed-Tools", strings.Join(filtering.RemovedToolTypes, ","))
	}
	log.Printf("responses_route requested_model=%q effective_provider=%q capability_action=%q", resolved.Slug, resolved.Model.Provider, action)
	if resolved.Provider.CredentialMode == config.CredentialRequestPassthrough {
		routed, err = rewriteModel(routed, resolved.UpstreamModel)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
			return
		}
		h.proxyBifrost(w, req, chatGPTResponsesPath, routed)
		return
	}
	routed, err = rewriteModel(routed, resolved.Model.Provider+"/"+resolved.UpstreamModel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return
	}
	h.proxyBifrost(w, req, "/v1/responses", routed)
}

func (h *Handler) resolveRequestModel(body []byte) (config.ResolvedModel, error) {
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return config.ResolvedModel{}, err
	}
	resolved, ok := h.cfg.ResolveModel(envelope.Model)
	if !ok {
		return config.ResolvedModel{}, fmt.Errorf("model %q is not present in the router catalog", envelope.Model)
	}
	return resolved, nil
}

func rewriteModel(body []byte, model string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields["model"] = encoded
	return json.Marshal(fields)
}

func (h *Handler) proxyBifrost(w http.ResponseWriter, req *http.Request, path string, body []byte) {
	cloned := req.Clone(req.Context())
	cloned.URL.Path = path
	cloned.URL.RawPath = ""
	if body != nil {
		cloned.Body = io.NopCloser(bytes.NewReader(body))
		cloned.ContentLength = int64(len(body))
		cloned.GetBody = nil
		cloned.Header.Del("Content-Length")
	}
	h.bifrostProxy.ServeHTTP(w, cloned)
}

func (h *Handler) serveModels(w http.ResponseWriter, req *http.Request) {
	bifrostBody, bifrostStatus, err := h.fetch(req, h.bifrostURL, "/v1/models", false)
	if err != nil {
		writeError(w, http.StatusBadGateway, "catalog_unavailable", "Bifrost model catalog is unavailable")
		return
	}
	directBody, directStatus, directErr := h.fetch(req, h.chatGPTURL, h.chatGPTModelURL, true)
	if directErr != nil || directStatus < 200 || directStatus >= 300 {
		if bifrostStatus >= 200 && bifrostStatus < 300 {
			if decorated, decorateErr := catalog.DecorateCatalog(bifrostBody, h.cfg, h.metadataLookup); decorateErr == nil {
				bifrostBody = decorated
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(bifrostStatus)
		_, _ = w.Write(bifrostBody)
		return
	}
	merged, err := mergeCatalogs(directBody, bifrostBody, h.cfg, h.metadataLookup)
	if err != nil {
		writeError(w, http.StatusBadGateway, "catalog_invalid", "an upstream returned an invalid model catalog")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(merged)
}

func (h *Handler) fetch(original *http.Request, upstream *url.URL, path string, chatGPT bool) ([]byte, int, error) {
	target := *upstream
	target.Path = path
	target.RawQuery = original.URL.RawQuery
	req, err := http.NewRequestWithContext(original.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	if chatGPT {
		copyChatGPTHeaders(req.Header, original.Header)
	} else {
		req.Header = original.Header.Clone()
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return body, resp.StatusCode, err
}

func copyChatGPTHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if lower != "authorization" && lower != "chatgpt-account-id" && lower != "user-agent" && lower != "openai-beta" && !strings.HasPrefix(lower, "x-codex-") {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func mergeCatalogs(direct, bifrost []byte, cfg config.Config, lookup catalog.MetadataLookup) ([]byte, error) {
	var directCatalog map[string]json.RawMessage
	var bifrostCatalog map[string]json.RawMessage
	if err := json.Unmarshal(direct, &directCatalog); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(bifrost, &bifrostCatalog); err != nil {
		return nil, err
	}
	var directModels, bifrostModels []json.RawMessage
	if err := json.Unmarshal(directCatalog["models"], &directModels); err != nil {
		return nil, errors.New("direct catalog has no models array")
	}
	if err := json.Unmarshal(bifrostCatalog["models"], &bifrostModels); err != nil {
		return nil, errors.New("Bifrost catalog has no models array")
	}
	merged := make([]json.RawMessage, 0, len(directModels)+len(bifrostModels))
	for _, raw := range directModels {
		merged = append(merged, raw)
		var base map[string]any
		if json.Unmarshal(raw, &base) != nil {
			continue
		}
		identity := stringValue(base, "slug")
		if identity == "" {
			identity = stringValue(base, "id")
		}
		resolved, ok := cfg.ResolveModel(identity)
		if !ok || resolved.Variant != nil {
			continue
		}
		for _, candidate := range cfg.ResolvedModels() {
			if candidate.BaseSlug != resolved.BaseSlug || candidate.Variant == nil {
				continue
			}
			variant, err := json.Marshal(catalog.HydrateVariant(base, candidate, cfg.Instructions))
			if err != nil {
				return nil, err
			}
			merged = append(merged, variant)
		}
	}
	for _, raw := range bifrostModels {
		var identity struct {
			Slug string `json:"slug"`
		}
		if json.Unmarshal(raw, &identity) != nil {
			continue
		}
		resolved, ok := cfg.ResolveModel(identity.Slug)
		if ok && resolved.Provider.CredentialMode == config.CredentialRequestPassthrough {
			continue
		}
		merged = append(merged, raw)
	}
	for i, raw := range merged {
		var model map[string]any
		if json.Unmarshal(raw, &model) != nil {
			continue
		}
		identity := stringValue(model, "slug")
		if identity == "" {
			identity = stringValue(model, "id")
		}
		resolved, ok := cfg.ResolveModel(identity)
		if !ok {
			continue
		}
		catalog.DecorateModelWithMetadata(model, resolved, cfg, lookup)
		encoded, err := json.Marshal(model)
		if err != nil {
			return nil, err
		}
		merged[i] = encoded
	}
	models, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	directCatalog["models"] = models
	return json.Marshal(directCatalog)
}

func stringValue(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"type": "router_error", "code": code, "message": message,
	}})
}
