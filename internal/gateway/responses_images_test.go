package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestOpenAISolResponsesPreserveReferenceInputsAndNativeOutput(t *testing.T) {
	// Keep the prompt and all other request fields fixed; only input_image is
	// added for the reference case. This route must not invoke the Images bridge.
	png := referencePNG(t, 1206, 2622, 31)
	encoded := base64.StdEncoding.EncodeToString(png)
	for _, stream := range []bool{false, true} {
		for _, reference := range []bool{false, true} {
			name := "JSON"
			if stream {
				name = "SSE"
			}
			if reference {
				name += "/with_reference"
			} else {
				name += "/without_reference"
			}
			t.Run(name, func(t *testing.T) {
				content := []any{map[string]any{"type": "input_text", "text": imageRegressionPrompt}}
				if reference {
					content = append(content, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + encoded, "detail": "original"})
				}
				body := map[string]any{
					"model": "openai/gpt-5.6-sol",
					"input": []any{map[string]any{"role": "user", "content": content}},
					"tools": []any{
						map[string]any{"type": "image_generation", "model": "gpt-image-2", "action": "auto", "background": "opaque"},
						map[string]any{"type": "function", "name": "local_function", "parameters": map[string]any{"type": "object"}},
					},
					"tool_choice": map[string]any{"type": "image_generation"},
					"reasoning":   map[string]any{"effort": "medium"},
					"store":       false, "stream": stream,
					"future_native_field": map[string]any{"private_metadata": "private-native-metadata"},
				}
				responseJSON, err := json.Marshal(map[string]any{
					"id": "native-response-id", "object": "response", "model": "gpt-5.6-sol",
					"output":                []any{map[string]any{"type": "image_generation_call", "result": encoded}},
					"future_upstream_field": "preserved",
				})
				if err != nil {
					t.Fatal(err)
				}
				responseBody, contentType := string(responseJSON), "application/json"
				if stream {
					responseBody = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + string(responseJSON) + "}\n\n"
					contentType = "text/event-stream"
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Method != http.MethodPost || req.URL.Path != "/prefix"+chatGPTResponsesPath {
						t.Errorf("native request route = %s %s", req.Method, req.URL.Path)
					}
					if req.Header.Get("Authorization") != "Bearer private-auth-canary" || req.Header.Get("x-bf-vk") != "private-virtual-key-canary" || req.Header.Get("ChatGPT-Account-ID") != "private-account-canary" {
						t.Error("native credentials or account identity changed")
					}
					var actual map[string]any
					if err := json.NewDecoder(req.Body).Decode(&actual); err != nil {
						t.Errorf("forwarded native request is invalid: %v", err)
						return
					}
					// Only canonical model resolution may change this native body.
					actual["model"] = "openai/" + actual["model"].(string)
					if !reflect.DeepEqual(actual, body) {
						t.Error("native payload changed beyond resolving the model; reference/tool/options must pass through")
					}
					w.Header().Set("Content-Type", contentType)
					w.Header().Set("X-Request-ID", "native-upstream-id")
					_, _ = io.WriteString(w, responseBody)
				}))
				defer upstream.Close()
				cfg := testConfig(t)
				// Native requests must work independently of the image bridge's
				// fallback model. Sol remains selected even with no bridge backend.
				cfg.ImageGenerationModel = ""
				handler, err := New(cfg, upstream.URL+"/prefix", upstream.URL)
				if err != nil {
					t.Fatal(err)
				}
				req := imageJSONRequest(t, "/v1/responses", body)
				req.Header.Set("ChatGPT-Account-ID", "private-account-canary")
				resp := httptest.NewRecorder()
				handler.ServeHTTP(resp, req)
				if resp.Code != http.StatusOK || resp.Body.String() != responseBody || resp.Header().Get("Content-Type") != contentType || resp.Header().Get("X-Request-ID") != "native-upstream-id" || resp.Header().Get("X-Bifrost-Removed-Tools") != "" {
					t.Errorf("native response was converted or tool removed: status=%d", resp.Code)
				}
			})
		}
	}
}

func TestOpenAISolResponsesHTTPFailureDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	encoded := base64.StdEncoding.EncodeToString(referencePNG(t, 1206, 2622, 31))
	var receivedID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/prefix"+chatGPTResponsesPath {
			t.Errorf("native failure used wrong path: %q", req.URL.Path)
		}
		receivedID = req.Header.Get("X-Request-ID")
		w.Header().Set("X-Request-ID", "native-incident-id")
		w.Header().Set("Server", "nginx/1.27.5")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<html><title>404 Not Found</title>private-native-error-body "+encoded+"</html>")
	}))
	defer upstream.Close()
	handler, err := New(testConfig(t), upstream.URL+"/prefix?private-query-canary=secret", upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []bool{false, true} {
		content := []any{map[string]any{"type": "input_text", "text": imageRegressionPrompt}}
		if reference {
			content = append(content, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + encoded})
		}
		req := imageJSONRequest(t, "/v1/responses", map[string]any{
			"model": "openai/gpt-5.6-sol", "input": []any{map[string]any{"role": "user", "content": content}},
			"tools": []any{map[string]any{"type": "image_generation"}},
		})
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusNotFound || resp.Header().Get("Content-Type") != "application/json" || resp.Header().Get("X-Request-ID") != "native-incident-id" {
			t.Fatalf("native HTML failure not normalized: status=%d", resp.Code)
		}
		diag := dispatchErrorDiagnostics(t, resp, "responses_upstream_error")
		if diag.RequestID != receivedID || diag.RequestMethod != "POST" || diag.RequestPath != "/v1/responses" || diag.Operation != "responses" || diag.Stage != "upstream_http" || diag.SelectedModel != "openai/gpt-5.6-sol" || diag.RequestedModel != "openai/gpt-5.6-sol" || diag.UpstreamModel != "gpt-5.6-sol" || diag.Provider != "openai" || diag.Capability != "native" || diag.Backend != "native_responses_passthrough" || diag.UpstreamMethod != "POST" || diag.UpstreamURL != upstream.URL+"/prefix"+chatGPTResponsesPath || diag.UpstreamStatus != 404 || diag.ResponseHop != "bifrost" || diag.UpstreamRequestID != "native-incident-id" || diag.UpstreamServer != "nginx/1.27.5" {
			t.Fatalf("incomplete native route diagnostics: %+v", diag)
		}
		if !strings.Contains(logs.String(), `responses_dispatch {"request_id":"`+diag.RequestID+`"`) {
			t.Error("native request ID not correlated with log")
		}
		for _, private := range []string{imageRegressionPrompt, encoded, "private-native-error-body", "private-auth-canary", "private-virtual-key-canary", "private-query-canary"} {
			if strings.Contains(resp.Body.String(), private) || strings.Contains(logs.String(), private) {
				t.Error("native diagnostic leaked private request or error content")
			}
		}
	}
}

func TestResponsesPreserveNativeJSONErrors(t *testing.T) {
	responseBody := `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"private-native-json-error"},"future_error_field":true}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "native-rate-limit-id")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()
	handler, err := New(testConfig(t), upstream.URL, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, imageJSONRequest(t, "/v1/responses", map[string]any{"model": "openai/gpt-5.6-sol", "input": imageRegressionPrompt}))
	if resp.Code != http.StatusTooManyRequests || resp.Body.String() != responseBody || resp.Header().Get("X-Request-ID") != "native-rate-limit-id" || resp.Header().Get("X-Bifrost-Request-ID") == "" {
		t.Error("native JSON error was changed by dispatch diagnostics")
	}
}

func TestResponsesTransportFailureDiagnostics(t *testing.T) {
	handler, err := New(testConfig(t), "http://user:private-password@127.0.0.1:1/prefix?private-query=secret", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	handler.bifrostProxy.Transport = imageRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private-transport-error")
	})
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, imageJSONRequest(t, "/v1/responses", map[string]any{"model": "openai/gpt-5.6-sol", "input": imageRegressionPrompt}))
	diag := dispatchErrorDiagnostics(t, resp, "upstream_unavailable")
	if resp.Code != http.StatusBadGateway || diag.Stage != "upstream_transport" || diag.UpstreamStatus != 0 || diag.ResponseHop != "" || diag.RequestPath != "/v1/responses" || diag.UpstreamURL != "http://127.0.0.1:1/prefix"+chatGPTResponsesPath || strings.Contains(resp.Body.String(), "private-") {
		t.Fatalf("incorrect native transport failure diagnostics: %+v", diag)
	}
}
