package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testImageReference struct {
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

const imageRegressionPrompt = "Create six 3D Rep wordmark treatments; preserve the layout, Dynamic Island and glowing Start button."

func referencePNG(t *testing.T, width, height int, tint uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	img.SetNRGBA(0, 0, color.NRGBA{R: tint, A: 255})
	var body bytes.Buffer
	if err := png.Encode(&body, img); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func imageJSONRequest(t *testing.T, path string, body any) *http.Request {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	setImageCredentials(req)
	return req
}

func setImageCredentials(req *http.Request) {
	req.Header.Set("Authorization", "Bearer private-auth-canary")
	req.Header.Set("x-bf-vk", "private-virtual-key-canary")
}

func imageMultipartRequest(t *testing.T, imageField string, images [][]byte, fields map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, data := range images {
		// Filename and the default application/octet-stream Content-Type must
		// not control MIME detection, payload conversion or diagnostic output.
		part, err := writer.CreateFormFile(imageField, "private-reference-filename.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	setImageCredentials(req)
	return req
}

func writeImageSSE(w http.ResponseWriter, encoded string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"image_generation_call\",\"result\":\""+encoded+"\"}]}}\n\n")
}

func dispatchErrorDiagnostics(t *testing.T, resp *httptest.ResponseRecorder, code string) dispatchDiagnostics {
	t.Helper()
	var result struct {
		Error struct {
			Code        string              `json:"code"`
			Diagnostics dispatchDiagnostics `json:"diagnostics"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid error JSON: %v", err)
	}
	if result.Error.Code != code {
		t.Fatalf("error code = %q, want %q", result.Error.Code, code)
	}
	diag := result.Error.Diagnostics
	if diag.RequestID == "" || diag.RequestID != resp.Header().Get("X-Bifrost-Request-ID") || diag.Status != resp.Code || diag.ErrorCode != code {
		t.Fatalf("inconsistent diagnostics: %+v", diag)
	}
	if _, err := time.Parse(time.RFC3339Nano, diag.Timestamp); err != nil {
		t.Fatalf("invalid incident timestamp: %q", diag.Timestamp)
	}
	return diag
}

func TestImagesDispatchWithAndWithoutReferences(t *testing.T) {
	// The original macOS screenshot is unavailable in this checkout. Use a
	// valid PNG of the reported dimensions and exactly the same prompt in both
	// operations. Assert the bytes survive conversion without re-encoding.
	first := referencePNG(t, 1206, 2622, 31)
	second := referencePNG(t, 1206, 2622, 79)
	encoded := base64.StdEncoding.EncodeToString(first)
	var wantImages [][]byte
	var wantAction, receivedID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/prefix"+chatGPTResponsesPath || req.URL.RawQuery != "" {
			t.Errorf("image route = %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer private-auth-canary" || req.Header.Get("x-bf-vk") != "private-virtual-key-canary" {
			t.Error("image route lost request-scoped credentials")
		}
		if req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("forwarded Content-Type = %q", req.Header.Get("Content-Type"))
		}
		receivedID = req.Header.Get("X-Request-ID")
		var body struct {
			Model string `json:"model"`
			Input []struct {
				Role    string `json:"role"`
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL string `json:"image_url"`
					Detail   string `json:"detail"`
				} `json:"content"`
			} `json:"input"`
			Tools      []map[string]any  `json:"tools"`
			ToolChoice map[string]string `json:"tool_choice"`
			Store      bool              `json:"store"`
			Stream     bool              `json:"stream"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("invalid forwarded JSON: %v", err)
			return
		}
		if body.Model != "luna-upstream" || len(body.Input) != 1 || len(body.Input[0].Content) != 1+len(wantImages) || body.Input[0].Role != "user" || body.Store || !body.Stream || body.ToolChoice["type"] != "image_generation" {
			t.Errorf("invalid image dispatch envelope")
			return
		}
		content := body.Input[0].Content
		if content[0].Type != "input_text" || content[0].Text != imageRegressionPrompt {
			t.Error("comparison prompt changed during routing")
		}
		for i, expected := range wantImages {
			if content[i+1].Type != "input_image" || content[i+1].Detail != "auto" || !strings.HasPrefix(content[i+1].ImageURL, "data:image/png;base64,") {
				t.Errorf("reference %d is not a PNG input_image", i)
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(content[i+1].ImageURL, "data:image/png;base64,"))
			if err != nil || !bytes.Equal(decoded, expected) {
				t.Errorf("reference %d bytes or ordering changed", i)
			}
		}
		if len(body.Tools) != 1 {
			t.Error("missing image tool")
			return
		}
		tool := body.Tools[0]
		for name, value := range map[string]any{
			"type": "image_generation", "model": "gpt-image-2", "action": wantAction,
			"size": "auto", "quality": "high", "background": "opaque", "output_format": "png", "output_compression": float64(90),
		} {
			if tool[name] != value {
				t.Errorf("tool %s = %v, want %v", name, tool[name], value)
			}
		}
		writeImageSSE(w, encoded)
	}))
	defer upstream.Close()
	cfg := testConfig(t)
	model := cfg.Models["openai/luna"]
	model.UpstreamModel = "luna-upstream"
	cfg.Models["openai/luna"] = model
	if err := cfg.ApplyDefaultsAndValidate(); err != nil {
		t.Fatal(err)
	}
	handler, err := New(cfg, upstream.URL+"/prefix", upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{"model": "gpt-image-2", "prompt": imageRegressionPrompt, "size": "auto", "quality": "high", "background": "opaque", "output_format": "png", "output_compression": "90"}
	jsonBody := func(images []testImageReference) map[string]any {
		return map[string]any{"model": "gpt-image-2", "prompt": imageRegressionPrompt, "size": "auto", "quality": "high", "background": "opaque", "output_format": "png", "output_compression": 90, "images": images}
	}
	cases := []struct {
		name   string
		req    *http.Request
		images [][]byte
		action string
	}{
		{"text only", imageJSONRequest(t, "/v1/images/generations", jsonBody(nil)), nil, "generate"},
		{"single multipart reference", imageMultipartRequest(t, "image", [][]byte{first}, fields), [][]byte{first}, "edit"},
		{"multiple multipart references", imageMultipartRequest(t, "image[]", [][]byte{first, second}, fields), [][]byte{first, second}, "edit"},
		{"JSON reference", imageJSONRequest(t, "/v1/images/edits", jsonBody([]testImageReference{{ImageURL: "data:image/png;base64," + encoded}})), [][]byte{first}, "edit"},
	}
	seenIDs := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantImages, wantAction = tc.images, tc.action
			tc.req.Header.Set("X-Request-ID", "private-client-id")
			tc.req.URL.RawQuery = "private-query-canary=secret"
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, tc.req)
			var result struct {
				Data []struct {
					Base64 string `json:"b64_json"`
				} `json:"data"`
			}
			if resp.Code != http.StatusOK || json.Unmarshal(resp.Body.Bytes(), &result) != nil || len(result.Data) != 1 || result.Data[0].Base64 != encoded {
				t.Fatalf("image dispatch failed, status=%d", resp.Code)
			}
			id := resp.Header().Get("X-Request-ID")
			if id == "" || id == "private-client-id" || id != receivedID || seenIDs[id] {
				t.Errorf("request IDs were not generated and correlated")
			}
			seenIDs[id] = true
		})
	}
}

func TestImagesRejectUnavailableBackend(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	for _, model := range []string{"", "missing/context-123k", "managed/text-model"} {
		t.Run(model, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.ImageGenerationModel = model
			handler, err := New(cfg, upstream.URL, upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			for _, edit := range []bool{false, true} {
				req := imageJSONRequest(t, "/v1/images/generations", map[string]any{"model": "gpt-image-2", "prompt": imageRegressionPrompt})
				if edit {
					req = imageMultipartRequest(t, "image", [][]byte{referencePNG(t, 2, 2, 1)}, map[string]string{"model": "gpt-image-2", "prompt": imageRegressionPrompt})
				}
				resp := httptest.NewRecorder()
				handler.ServeHTTP(resp, req)
				if resp.Code != http.StatusServiceUnavailable {
					t.Fatalf("status = %d", resp.Code)
				}
				diag := dispatchErrorDiagnostics(t, resp, "image_operation_unsupported")
				if diag.Stage != "backend_selection" || diag.UpstreamURL != "" || diag.UpstreamStatus != 0 {
					t.Errorf("unsupported operation claimed upstream dispatch: %+v", diag)
				}
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("unsupported backend received %d requests", calls.Load())
	}
}

func TestImagesRejectInvalidOrUnsupportedReferences(t *testing.T) {
	data := referencePNG(t, 2, 2, 1)
	oversized := make([]byte, maxReferenceImageBytes+1)
	copy(oversized, data)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	fields := map[string]string{"model": "gpt-image-2", "prompt": imageRegressionPrompt}
	cases := []struct {
		name string
		req  *http.Request
		code string
	}{
		{"missing image", imageMultipartRequest(t, "image", nil, fields), "invalid_reference_images"},
		{"invalid image bytes", imageMultipartRequest(t, "image", [][]byte{[]byte("private-invalid-image")}, fields), "invalid_image_request"},
		{"oversized image", imageMultipartRequest(t, "image", [][]byte{oversized}, fields), "invalid_image_request"},
		{"unexpected upload", imageMultipartRequest(t, "unexpected", [][]byte{data}, fields), "invalid_image_request"},
		{"too many images", imageMultipartRequest(t, "image[]", bytesToImages(data, 17), fields), "invalid_image_request"},
		{"mask", imageMultipartRequest(t, "mask", [][]byte{data}, fields), "image_mask_unsupported"},
		{"streaming", imageMultipartRequest(t, "image", [][]byte{data}, map[string]string{"model": "gpt-image-2", "prompt": imageRegressionPrompt, "stream": "true"}), "image_response_format_unsupported"},
		{"file ID", imageJSONRequest(t, "/v1/images/edits", map[string]any{"model": "gpt-image-2", "prompt": imageRegressionPrompt, "images": []testImageReference{{FileID: "file-private"}}}), "image_file_reference_unsupported"},
		{"local path", imageJSONRequest(t, "/v1/images/edits", map[string]any{"model": "gpt-image-2", "prompt": imageRegressionPrompt, "images": []testImageReference{{ImageURL: "/private/local-reference.png"}}}), "invalid_reference_image"},
		{"MIME mismatch", imageJSONRequest(t, "/v1/images/edits", map[string]any{"model": "gpt-image-2", "prompt": imageRegressionPrompt, "images": []testImageReference{{ImageURL: strings.Replace(dataURL, "image/png", "image/jpeg", 1)}}}), "invalid_reference_image"},
		{"references on generate", imageJSONRequest(t, "/v1/images/generations", map[string]any{"model": "gpt-image-2", "prompt": imageRegressionPrompt, "images": []testImageReference{{ImageURL: dataURL}}}), "image_operation_unsupported"},
		{"tool paths are not wire uploads", imageJSONRequest(t, "/v1/images/generations", map[string]any{"model": "gpt-image-2", "prompt": imageRegressionPrompt, "referenced_image_paths": []string{"/private/local-reference.png"}}), "invalid_image_request"},
	}
	var calls atomic.Int32
	handler, err := New(testConfig(t), "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	handler.bifrostProxy.Transport = imageRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected dispatch")
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, tc.req)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d", resp.Code)
			}
			dispatchErrorDiagnostics(t, resp, tc.code)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid image request dispatched %d times", calls.Load())
	}
}

func bytesToImages(data []byte, count int) [][]byte {
	images := make([][]byte, count)
	for i := range images {
		images[i] = data
	}
	return images
}

type imageRoundTripper func(*http.Request) (*http.Response, error)

func (rt imageRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return rt(req) }

func TestImageFailureDiagnosticsAndPrivacy(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	var receivedID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		receivedID = req.Header.Get("X-Request-ID")
		w.Header().Set("Server", "nginx/1.27.5")
		w.Header().Set("X-Request-ID", "upstream-incident-id")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<html><title>404 Not Found</title>nginx/1.27.5 private-upstream-body-canary</html>")
	}))
	defer upstream.Close()
	handler, err := New(testConfig(t), upstream.URL+"/prefix?private-upstream-query=secret", upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	data := referencePNG(t, 1206, 2622, 11)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, imageMultipartRequest(t, "image", [][]byte{data}, map[string]string{"model": "gpt-image-2", "prompt": imageRegressionPrompt}))
	if resp.Code != http.StatusNotFound || resp.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("upstream HTML error not normalized: status=%d", resp.Code)
	}
	diag := dispatchErrorDiagnostics(t, resp, "image_upstream_error")
	if diag.RequestID != receivedID || diag.Stage != "upstream_http" || diag.RequestedModel != "gpt-image-2" || diag.SelectedModel != "openai/luna" || diag.UpstreamModel != "luna" || diag.Provider != "openai" || diag.Capability != "image_edit" || diag.Backend != "native_responses_image_tool" || diag.ReferenceImages != 1 || diag.UpstreamMethod != http.MethodPost || diag.UpstreamURL != upstream.URL+"/prefix"+chatGPTResponsesPath || diag.ResponseHop != "bifrost" || diag.UpstreamStatus != 404 || diag.UpstreamRequestID != "upstream-incident-id" || diag.UpstreamServer != "nginx/1.27.5" {
		t.Fatalf("incomplete failure diagnostics: %+v", diag)
	}
	if !strings.Contains(logs.String(), `"request_id":"`+diag.RequestID+`"`) || !strings.Contains(logs.String(), `"timestamp":"`+diag.Timestamp+`"`) {
		t.Error("incident is not correlated with routing log")
	}
	for _, private := range []string{imageRegressionPrompt, "private-auth-canary", "private-virtual-key-canary", "private-reference-filename", "private-upstream-body-canary", "private-upstream-query", base64.StdEncoding.EncodeToString(data)} {
		if strings.Contains(logs.String(), private) || strings.Contains(resp.Body.String(), private) {
			t.Errorf("diagnostics leaked private request or upstream content")
		}
	}
}

func TestImageTransportFailureDoesNotClaimUpstreamResponse(t *testing.T) {
	handler, err := New(testConfig(t), "http://user:private-url-password@127.0.0.1:1/prefix?private-query=secret", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	handler.bifrostProxy.Transport = imageRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport failed with private-transport-details")
	})
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, imageJSONRequest(t, "/v1/images/generations", map[string]any{"model": "gpt-image-2", "prompt": imageRegressionPrompt}))
	diag := dispatchErrorDiagnostics(t, resp, "image_upstream_unavailable")
	if resp.Code != http.StatusBadGateway || diag.Stage != "upstream_transport" || diag.ResponseHop != "" || diag.UpstreamStatus != 0 || diag.UpstreamURL != "http://127.0.0.1:1/prefix"+chatGPTResponsesPath {
		t.Fatalf("transport failure attributed to a response: %+v", diag)
	}
	if strings.Contains(resp.Body.String(), "private-") {
		t.Error("transport diagnostics leaked private details")
	}
}

func TestImageNoResultDiagnostics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"private-response-error\"}}}\n\n")
	}))
	defer upstream.Close()
	handler, err := New(testConfig(t), upstream.URL, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, imageJSONRequest(t, "/v1/images/generations", map[string]any{"model": "gpt-image-2", "prompt": imageRegressionPrompt}))
	diag := dispatchErrorDiagnostics(t, resp, "image_generation_failed")
	if resp.Code != http.StatusBadGateway || diag.Stage != "response_decode" || diag.UpstreamStatus != 200 || diag.ResponseHop != "bifrost" || strings.Contains(resp.Body.String(), "private-response-error") {
		t.Fatalf("invalid response diagnostics: %+v", diag)
	}
}

func TestImageDiagnosticURLPreservesEndpointAndRemovesSecrets(t *testing.T) {
	base, err := url.Parse("https://user:private-password@example.test/prefix%2Fsegment/?private-query=secret#private-fragment")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://example.test/prefix%2Fsegment" + chatGPTResponsesPath
	if got := imageUpstreamURL(base); got != want {
		t.Fatalf("diagnostic endpoint = %q, want %q", got, want)
	}
	if base.User == nil || base.RawQuery == "" || base.Fragment == "" {
		t.Error("diagnostic sanitization changed the configured upstream")
	}
}
