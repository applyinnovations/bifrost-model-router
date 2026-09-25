package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/applyinnovations/bifrost-model-router/internal/config"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{
		Version:                 1,
		HostedToolFallbackModel: "openai/luna",
		Providers: map[string]config.ProviderProfile{
			"openai":     {CredentialMode: config.CredentialRequestPassthrough, ResponsesMode: config.ResponsesNative, DiscoverModels: true},
			"managed":    {CredentialMode: config.CredentialBifrost, ResponsesMode: config.ResponsesChatPolyfill, DiscoverModels: true},
			"openrouter": {CredentialMode: config.CredentialBifrost, ResponsesMode: config.ResponsesChatPolyfill, DiscoverModels: true},
		},
		Models: map[string]config.ModelProfile{
			"openai/sol":                           {Aliases: []string{"sol"}, Codex: config.CodexProfile{ContextWindow: 272000, MaxContextWindow: 872000}, ContextVariants: []config.ContextVariant{{ContextWindow: 872000}}},
			"openai/luna":                          {Aliases: []string{"luna"}, Codex: config.CodexProfile{}},
			"managed/text-model":                   {Aliases: []string{"text-model"}, Codex: config.CodexProfile{}},
			"openrouter/stealth/space-bunny-alpha": {Codex: config.CodexProfile{}},
		},
	}
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestResponsesDispatch(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantPath  string
		wantModel string
		wantTool  string
	}{
		{"OpenAI native", `{"model":"openai/sol","input":"hi"}`, chatGPTResponsesPath, "sol", ""},
		{"OpenAI context variant", `{"model":"sol-872k","input":"hi"}`, chatGPTResponsesPath, "sol", ""},
		{"managed provider", `{"model":"managed/text-model","input":"hi"}`, "/v1/responses", "managed/text-model", ""},
		{"new managed model", `{"model":"managed/new-model","input":"hi"}`, "/v1/responses", "managed/new-model", ""},
		{"new OpenAI model", `{"model":"new-openai-model","input":"hi"}`, chatGPTResponsesPath, "new-openai-model", ""},
		{"hosted tool fallback", `{"model":"managed/text-model","input":"hi","tools":[{"type":"web_search"}]}`, chatGPTResponsesPath, "luna", "web_search"},
		{"hosted image tool fallback", `{"model":"managed/text-model","input":"draw a mark","tools":[{"type":"image_generation"}]}`, chatGPTResponsesPath, "luna", "image_generation"},
		{"unsupported hosted tool fallback", `{"model":"openrouter/stealth/space-bunny-alpha","input":"hi","tools":[{"type":"file_search"}]}`, chatGPTResponsesPath, "luna", "file_search"},
		{"OpenRouter native web search bridge", `{"model":"openrouter/stealth/space-bunny-alpha","input":"what model are you?","tools":[{"type":"web_search"}]}`, "/v1/responses", "openrouter/stealth/space-bunny-alpha", "openrouter:web_search"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != test.wantPath {
					t.Errorf("path = %q, want %q", req.URL.Path, test.wantPath)
				}
				if req.Header.Get("Authorization") != "Bearer test" || req.Header.Get("x-bf-vk") != "sk-bf-test" {
					t.Errorf("routing headers were not preserved: %#v", req.Header)
				}
				body, _ := io.ReadAll(req.Body)
				var envelope struct {
					Model string `json:"model"`
					Tools []struct {
						Type string `json:"type"`
					} `json:"tools"`
				}
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Model != test.wantModel {
					t.Errorf("model = %q, want %q", envelope.Model, test.wantModel)
				}
				if test.wantTool != "" && (len(envelope.Tools) != 1 || envelope.Tools[0].Type != test.wantTool) {
					t.Errorf("tools = %#v, want %q", envelope.Tools, test.wantTool)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: done\n\n"))
			}))
			defer upstream.Close()
			handler, err := New(testConfig(t), upstream.URL, upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", io.NopCloser(stringsReader(test.body)))
			req.Header.Set("Authorization", "Bearer test")
			req.Header.Set("x-bf-vk", "sk-bf-test")
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
			}
		})
	}
}

func TestOpenRouterWebSearchResultAndRoutingLogPassThrough(t *testing.T) {
	const searchEvent = `data: {"type":"response.output_item.done","item":{"type":"web_search_call","status":"completed"}}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", req.URL.Path)
		}
		body, _ := io.ReadAll(req.Body)
		if !bytes.Contains(body, []byte(`"model":"openrouter/stealth/space-bunny-alpha"`)) ||
			!bytes.Contains(body, []byte(`"type":"openrouter:web_search"`)) {
			t.Errorf("bridged request = %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(searchEvent))
	}))
	defer upstream.Close()
	handler, err := New(testConfig(t), upstream.URL, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", stringsReader(`{"model":"openrouter/stealth/space-bunny-alpha","input":"find current news","tools":[{"type":"web_search"}]}`))
	req.Header.Set("Authorization", "Bearer secret-openai-token")
	req.Header.Set("x-bf-vk", "secret-virtual-key")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Body.String() != searchEvent {
		t.Fatalf("status = %d body=%q", resp.Code, resp.Body.String())
	}
	logText := logs.String()
	for _, want := range []string{
		`requested_model="openrouter/stealth/space-bunny-alpha"`,
		`effective_provider="openrouter"`,
		`fallback_reason="openrouter_native_web_search"`,
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("routing log %q does not contain %q", logText, want)
		}
	}
	for _, private := range []string{"find current news", "secret-openai-token", "secret-virtual-key"} {
		if strings.Contains(logText, private) {
			t.Errorf("routing log contains private value %q: %s", private, logText)
		}
	}
}

func TestStandaloneImageGenerationUsesHostedToolBridge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != chatGPTResponsesPath {
			t.Errorf("path = %q", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "Bearer test" || req.Header.Get("x-bf-vk") != "sk-bf-test" {
			t.Error("request-scoped credentials were not preserved")
		}
		var body struct {
			Model string `json:"model"`
			Input []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
			Tools []struct {
				Type  string `json:"type"`
				Model string `json:"model"`
				Size  string `json:"size"`
			} `json:"tools"`
			Store  bool `json:"store"`
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "luna" || len(body.Input) != 1 || len(body.Input[0].Content) != 1 || body.Input[0].Content[0].Text != "draw a mark" || len(body.Tools) != 1 || body.Tools[0].Type != "image_generation" || body.Tools[0].Model != "gpt-image-2" || body.Tools[0].Size != "1024x1024" || body.Store || !body.Stream {
			t.Errorf("image bridge request = %#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\"}]}}\n\n"))
	}))
	defer upstream.Close()
	handler, err := New(testConfig(t), upstream.URL, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", stringsReader(`{"model":"gpt-image-2","prompt":"draw a mark","size":"1024x1024"}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("x-bf-vk", "sk-bf-test")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"b64_json":"aW1hZ2U="`) {
		t.Fatalf("status = %d body = %s", resp.Code, resp.Body.String())
	}
}

func TestStandaloneImageGenerationRequiresBothCredentials(t *testing.T) {
	handler, err := New(testConfig(t), "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, virtualKey, bearer, wantCode string
	}{
		{"missing virtual key", "", "Bearer test", "missing_bifrost_auth"},
		{"missing Codex login", "sk-bf-test", "", "missing_openai_auth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", stringsReader(`{"model":"gpt-image-2","prompt":"draw a mark"}`))
			req.Header.Set("x-bf-vk", tc.virtualKey)
			req.Header.Set("Authorization", tc.bearer)
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusUnauthorized || !strings.Contains(resp.Body.String(), tc.wantCode) {
				t.Fatalf("status = %d body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}

func TestModelsPreserveDirectMetadataAndAppendManagedModels(t *testing.T) {
	bifrost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/models" {
			t.Fatalf("Bifrost path = %q", req.URL.Path)
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"openai/sol","display_name":"stale"},{"slug":"managed/text-model","display_name":"Managed Text"}]}`))
	}))
	defer bifrost.Close()
	chatGPT := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/backend-api/codex/models" || req.URL.Query().Get("client_version") != "test" {
			t.Fatalf("ChatGPT request = %s", req.URL.String())
		}
		if req.Header.Get("Authorization") != "Bearer test" || req.Header.Get("x-bf-vk") != "" {
			t.Fatalf("ChatGPT headers = %#v", req.Header)
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"sol","display_name":"Direct Sol","context_window":872000}],"recommended_model":"sol"}`))
	}))
	defer chatGPT.Close()
	handler, err := New(testConfig(t), bifrost.URL, chatGPT.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler.metadataLookup = func(provider, upstream string) (string, int64, bool) {
		names := map[string]string{
			"openai/sol":         "OpenAI: Sol",
			"managed/text-model": "Publisher: Text Model",
		}
		name, ok := names[provider+"/"+upstream]
		context := int64(0)
		if provider == "managed" && upstream == "text-model" {
			context = 1000000
		}
		return name, context, ok
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=test", nil)
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("x-bf-vk", "sk-bf-test")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	var catalog struct {
		Models []struct {
			Slug          string `json:"slug"`
			DisplayName   string `json:"display_name"`
			ContextWindow int    `json:"context_window"`
		} `json:"models"`
		RecommendedModel string `json:"recommended_model"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 3 || catalog.Models[0].Slug != "sol" || catalog.Models[0].DisplayName != "Sol" || catalog.Models[0].ContextWindow != 872000 || catalog.Models[1].Slug != "sol-872k" || catalog.Models[1].DisplayName != "Sol (872K)" || catalog.Models[2].Slug != "managed/text-model" || catalog.Models[2].DisplayName != "Text Model (Managed)" || catalog.Models[2].ContextWindow != 1000000 || catalog.RecommendedModel != "sol" {
		t.Fatalf("merged catalog = %#v", catalog)
	}
}

func TestModelsEditorializesManagedFallbackWhenDirectCatalogFails(t *testing.T) {
	bifrost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"slug":"managed/text-model","display_name":"text-model [Managed]"}]}`))
	}))
	defer bifrost.Close()
	chatGPT := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer chatGPT.Close()
	handler, err := New(testConfig(t), bifrost.URL, chatGPT.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler.metadataLookup = func(provider, upstream string) (string, int64, bool) {
		return "Publisher: Text Model", 1000000, provider == "managed" && upstream == "text-model"
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=test", nil)
	req.Header.Set("x-bf-vk", "sk-bf-test")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"display_name":"Text Model (Managed)"`) {
		t.Fatalf("status = %d body = %s", resp.Code, resp.Body.String())
	}
}

func TestResponsesRejectsUnknownContextVariant(t *testing.T) {
	handler, err := New(testConfig(t), "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", stringsReader(`{"model":"sol-512k","input":"hi"}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "unresolved_model") {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func stringsReader(value string) *reader { return &reader{value: value} }

type reader struct{ value string }

func (r *reader) Read(p []byte) (int, error) {
	if r.value == "" {
		return 0, io.EOF
	}
	n := copy(p, r.value)
	r.value = r.value[n:]
	return n, nil
}
