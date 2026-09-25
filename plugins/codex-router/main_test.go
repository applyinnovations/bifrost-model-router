package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

func initTestPlugin(t *testing.T) {
	t.Helper()
	err := Init(map[string]any{
		"version":                    1,
		"hosted_tool_fallback_model": "openai/luna",
		"providers": map[string]any{
			"openai":     map[string]any{"credential_mode": "request_passthrough", "responses_mode": "native"},
			"other":      map[string]any{"credential_mode": "bifrost", "responses_mode": "chat_polyfill", "discover_models": true},
			"openrouter": map[string]any{"credential_mode": "bifrost", "responses_mode": "chat_polyfill", "discover_models": true},
		},
		"models": map[string]any{
			"openai/a":                             map[string]any{"codex": map[string]any{}},
			"openai/luna":                          map[string]any{"codex": map[string]any{}},
			"other/b":                              map[string]any{"codex": map[string]any{}},
			"openrouter/stealth/space-bunny-alpha": map[string]any{"codex": map[string]any{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreAuthBridgesOpenRouterWebSearchWithoutOpenAIFallback(t *testing.T) {
	initTestPlugin(t)
	req := &schemas.HTTPRequest{Method: "POST", Path: "/v1/responses", Headers: map[string]string{
		"Authorization": "Bearer openai-canary",
		"x-bf-vk":       "sk-bf-canary",
	}, Body: []byte(`{"model":"openrouter/stealth/space-bunny-alpha","input":"hello","tools":[{"type":"web_search"}]}`)}
	resp, err := HTTPTransportPreAuthHook(nil, req)
	if err != nil || resp != nil {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	var envelope struct {
		Model string `json:"model"`
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(req.Body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Model != "openrouter/stealth/space-bunny-alpha" || len(envelope.Tools) != 1 || envelope.Tools[0].Type != "openrouter:web_search" {
		t.Fatalf("request = %#v", envelope)
	}
	if _, ok := req.Headers["Authorization"]; ok || req.Headers["x-bf-direct-key"] != "" {
		t.Fatalf("OpenAI credentials were retained: %#v", req.Headers)
	}
}

func TestPreAuthHostedToolFallsBackWithForwardedOpenAIAuth(t *testing.T) {
	initTestPlugin(t)
	body := []byte(`{"model":"other/b","input":"hello","tools":[{"type":"web_search"}]}`)
	req := &schemas.HTTPRequest{Method: "POST", Path: "/v1/responses", Headers: map[string]string{
		"Authorization": "Bearer openai-canary",
		"x-bf-vk":       "sk-bf-canary",
	}, Body: body}
	resp, err := HTTPTransportPreAuthHook(nil, req)
	if err != nil || resp != nil {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(req.Body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Model != "openai/luna" {
		t.Fatalf("model = %q", envelope.Model)
	}
	if req.Headers["Authorization"] != "Bearer openai-canary" || req.Headers["x-bf-direct-key"] != "true" {
		t.Fatalf("forwarded auth was not selected: %#v", req.Headers)
	}
}

func TestPreAuthHostedToolUsesDefaultFallbackWhenUnset(t *testing.T) {
	err := Init(map[string]any{
		"version": 1,
		"providers": map[string]any{
			"openai": map[string]any{"credential_mode": "request_passthrough", "responses_mode": "native", "discover_models": true},
			"other":  map[string]any{"credential_mode": "bifrost", "responses_mode": "chat_polyfill", "discover_models": true},
		},
		"models": map[string]any{
			"openai/gpt-5.6-sol": map[string]any{"codex": map[string]any{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &schemas.HTTPRequest{Method: "POST", Path: "/v1/responses", Headers: map[string]string{
		"Authorization": "Bearer openai-canary",
		"x-bf-vk":       "sk-bf-canary",
	}, Body: []byte(`{"model":"other/new-model","input":"hello","tools":[{"type":"web_search"}]}`)}
	resp, err := HTTPTransportPreAuthHook(nil, req)
	if err != nil || resp != nil {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(req.Body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Model != "openai/gpt-6-luna" || req.Headers["x-bf-direct-key"] != "true" {
		t.Fatalf("fallback model = %q, direct key = %q", envelope.Model, req.Headers["x-bf-direct-key"])
	}
}

func TestPreAuthNamespaceStaysOnManagedProvider(t *testing.T) {
	initTestPlugin(t)
	body := []byte(`{"model":"other/b","input":"hello","tools":[{"type":"namespace","name":"codex","tools":[{"type":"function","name":"shell"}]}]}`)
	req := &schemas.HTTPRequest{Method: "POST", Path: "/v1/responses", Headers: map[string]string{
		"Authorization": "Bearer openai-canary",
		"x-bf-vk":       "sk-bf-canary",
	}, Body: body}
	resp, err := HTTPTransportPreAuthHook(nil, req)
	if err != nil || resp != nil {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if string(req.Body) != string(body) {
		t.Fatalf("namespace request was rerouted: %s", req.Body)
	}
	if len(req.Headers) != 1 || req.Headers["x-bf-vk"] == "" {
		t.Fatalf("managed-provider credential policy not applied: %#v", req.Headers)
	}
}

func requestBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{"model": model, "input": "hello"})
	return body
}

func TestPreAuthRoutesCredentials(t *testing.T) {
	initTestPlugin(t)
	t.Run("openai", func(t *testing.T) {
		req := &schemas.HTTPRequest{Method: "POST", Path: "/v1/responses", Headers: map[string]string{"Authorization": "Bearer openai-canary", "x-bf-vk": "sk-bf-canary"}, Body: requestBody("openai/a")}
		resp, err := HTTPTransportPreAuthHook(nil, req)
		if err != nil || resp != nil {
			t.Fatalf("resp=%v err=%v", resp, err)
		}
		if req.Headers["Authorization"] == "" || req.Headers["x-bf-direct-key"] != "true" {
			t.Fatalf("headers = %#v", req.Headers)
		}
	})
	t.Run("openai requires Bifrost virtual key", func(t *testing.T) {
		req := &schemas.HTTPRequest{Method: "POST", Path: "/v1/responses", Headers: map[string]string{"Authorization": "Bearer openai-canary"}, Body: requestBody("openai/a")}
		resp, err := HTTPTransportPreAuthHook(nil, req)
		if err != nil || resp == nil || resp.StatusCode != 401 {
			t.Fatalf("resp=%v err=%v", resp, err)
		}
		if req.Headers["x-bf-direct-key"] != "" {
			t.Fatalf("direct key enabled without gateway auth: %#v", req.Headers)
		}
	})
	t.Run("bifrost", func(t *testing.T) {
		req := &schemas.HTTPRequest{Method: "POST", Path: "/v1/responses", Headers: map[string]string{"Authorization": "Bearer openai-canary", "x-bf-direct-key": "true"}, Body: requestBody("other/b")}
		resp, err := HTTPTransportPreAuthHook(nil, req)
		if err != nil || resp != nil {
			t.Fatalf("resp=%v err=%v", resp, err)
		}
		if len(req.Headers) != 0 {
			t.Fatalf("credential leaked: %#v", req.Headers)
		}
	})
}

func TestPreAuthRoutesChatGPTPassthroughCredentials(t *testing.T) {
	initTestPlugin(t)
	req := &schemas.HTTPRequest{
		Method: "POST",
		Path:   "/chatgpt_passthrough/backend-api/codex/responses",
		Headers: map[string]string{
			"Authorization": "Bearer openai-canary",
			"x-bf-vk":       "sk-bf-canary",
		},
		Body: requestBody("a"),
	}
	resp, err := HTTPTransportPreAuthHook(nil, req)
	if err != nil || resp != nil {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if req.Headers["Authorization"] == "" || req.Headers["x-bf-direct-key"] != "true" {
		t.Fatalf("headers = %#v", req.Headers)
	}
}

func TestPreLLMSelectsPolyfill(t *testing.T) {
	initTestPlugin(t)
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(time.Minute))
	defer ctx.Cancel()
	req := &schemas.BifrostRequest{RequestType: schemas.ResponsesStreamRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: "other", Model: "b"}}
	_, short, err := PreLLMHook(ctx, req)
	if err != nil || short != nil {
		t.Fatalf("short=%v err=%v", short, err)
	}
	if got, _ := ctx.Value(schemas.BifrostContextKeyChangeRequestType).(schemas.RequestType); got != schemas.ChatCompletionRequest {
		t.Fatalf("change request type = %q", got)
	}
}

func TestPreLLMUsesOpenRouterNativeResponsesForBridgedWebSearch(t *testing.T) {
	initTestPlugin(t)
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(time.Minute))
	defer ctx.Cancel()
	req := &schemas.BifrostRequest{RequestType: schemas.ResponsesStreamRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{
		Provider: schemas.OpenRouter,
		Model:    "stealth/space-bunny-alpha",
		Params: &schemas.ResponsesParameters{Tools: []schemas.ResponsesTool{{
			Type: schemas.ResponsesToolType("openrouter:web_search"),
		}}},
	}}
	_, short, err := PreLLMHook(ctx, req)
	if err != nil || short != nil {
		t.Fatalf("short=%v err=%v", short, err)
	}
	if changed, _ := ctx.Value(schemas.BifrostContextKeyChangeRequestType).(schemas.RequestType); changed != "" {
		t.Fatalf("request was converted to %q", changed)
	}
}

func TestPreLLMSelectsPolyfillForDiscoveredModel(t *testing.T) {
	initTestPlugin(t)
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(time.Minute))
	defer ctx.Cancel()
	req := &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: "other", Model: "new-model"}}
	_, short, err := PreLLMHook(ctx, req)
	if err != nil || short != nil {
		t.Fatalf("short=%v err=%v", short, err)
	}
	if got, _ := ctx.Value(schemas.BifrostContextKeyChangeRequestType).(schemas.RequestType); got != schemas.ChatCompletionRequest {
		t.Fatalf("change request type = %q", got)
	}
}

func TestPreLLMSelectsPolyfillForDiscoveredModelWithNestedUpstreamID(t *testing.T) {
	initTestPlugin(t)
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(time.Minute))
	defer ctx.Cancel()
	req := &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: "other", Model: "vendor/reasoner"}}
	got, short, err := PreLLMHook(ctx, req)
	if err != nil || short != nil {
		t.Fatalf("short=%v err=%v", short, err)
	}
	provider, model, _ := got.GetRequestFields()
	if provider != "other" || model != "vendor/reasoner" {
		t.Fatalf("resolved request = provider %q, model %q", provider, model)
	}
	if got.RequestType != schemas.ResponsesRequest {
		t.Fatalf("request type = %q", got.RequestType)
	}
	if got.ResponsesRequest == nil {
		t.Fatal("responses request is nil")
	}
	if got.ResponsesRequest.Provider != "other" || got.ResponsesRequest.Model != "vendor/reasoner" {
		t.Fatalf("upstream request = provider %q, model %q", got.ResponsesRequest.Provider, got.ResponsesRequest.Model)
	}
	if changed, _ := ctx.Value(schemas.BifrostContextKeyChangeRequestType).(schemas.RequestType); changed != schemas.ChatCompletionRequest {
		t.Fatalf("change request type = %q", changed)
	}
}

func TestPreLLMRejectsUnconfiguredSeparatedProvider(t *testing.T) {
	initTestPlugin(t)
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(time.Minute))
	defer ctx.Cancel()
	req := &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: "unconfigured", Model: "other/new-model"}}
	_, short, err := PreLLMHook(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if short == nil || short.Error == nil || short.Error.StatusCode == nil || *short.Error.StatusCode != 400 {
		t.Fatalf("short circuit = %#v", short)
	}
}

func TestPostHookHydratesCodexCatalog(t *testing.T) {
	initTestPlugin(t)
	originalLookup := metadataLookup
	metadataLookup = func(_, _ string) (string, int64, bool) { return "", 0, false }
	t.Cleanup(func() { metadataLookup = originalLookup })
	req := &schemas.HTTPRequest{Method: "GET", Path: "/v1/models", Query: map[string]string{"client_version": "test"}}
	resp := &schemas.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[{"id":"openai/a"}]}`)}
	if err := HTTPTransportPostHook(nil, req, resp); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["models"]; !ok {
		t.Fatalf("not a Codex catalog: %s", resp.Body)
	}
}
