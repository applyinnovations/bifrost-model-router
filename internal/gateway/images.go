package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// serveImageGeneration adapts the Images API used by Codex's built-in image
// tool to the hosted image_generation tool on the Codex Responses endpoint.
// Codex login tokens do not carry the api.model.images.request scope required
// by api.openai.com/v1/images/generations, so direct Images API passthrough
// cannot use the same credential as native Codex requests.
func (h *Handler) serveImageGeneration(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("x-bf-vk") == "" {
		writeError(w, http.StatusUnauthorized, "missing_bifrost_auth", "a Bifrost virtual key is required")
		return
	}
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
		writeError(w, http.StatusUnauthorized, "missing_openai_auth", "Codex OpenAI authentication is required")
		return
	}
	var imageReq struct {
		Model             string `json:"model"`
		Prompt            string `json:"prompt"`
		N                 int    `json:"n"`
		Size              string `json:"size"`
		Quality           string `json:"quality"`
		Background        string `json:"background"`
		OutputFormat      string `json:"output_format"`
		OutputCompression *int   `json:"output_compression"`
	}
	if err := json.NewDecoder(io.LimitReader(req.Body, 8<<20)).Decode(&imageReq); err != nil || imageReq.Model == "" || imageReq.Prompt == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "model and prompt are required")
		return
	}
	if imageReq.N > 1 || imageReq.N < 0 {
		writeError(w, http.StatusBadRequest, "unsupported_image_count", "only one image per request is supported")
		return
	}
	if h.cfg.HostedToolFallbackModel == "" {
		writeError(w, http.StatusServiceUnavailable, "image_fallback_unavailable", "no OpenAI Responses fallback model is configured")
		return
	}
	tool := map[string]any{"type": "image_generation", "model": imageReq.Model, "action": "generate"}
	for name, value := range map[string]string{
		"size": imageReq.Size, "quality": imageReq.Quality,
		"background": imageReq.Background, "output_format": imageReq.OutputFormat,
	} {
		if value != "" {
			tool[name] = value
		}
	}
	if imageReq.OutputCompression != nil {
		tool["output_compression"] = *imageReq.OutputCompression
	}
	responseBody, err := json.Marshal(map[string]any{
		"model":       h.cfg.HostedToolFallbackModel,
		"input":       []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": imageReq.Prompt}}}},
		"tools":       []any{tool},
		"tool_choice": map[string]string{"type": "image_generation"},
		"store":       false,
		"stream":      true,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "image_request_failed", "could not construct image request")
		return
	}
	forward := req.Clone(req.Context())
	forward.URL.Path = "/v1/responses"
	forward.Body = io.NopCloser(bytes.NewReader(responseBody))
	forward.ContentLength = int64(len(responseBody))
	forward.Header = req.Header.Clone()
	forward.Header.Del("Content-Length")
	forward.Header.Set("Content-Type", "application/json")
	// The Responses dispatcher owns model resolution and the request-scoped
	// credential policy. Capture its SSE output before shaping an Images reply.
	upstream := httptest.NewRecorder()
	h.serveResponses(upstream, forward)
	upstreamResp := upstream.Result()
	defer upstreamResp.Body.Close()
	if upstreamResp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", upstreamResp.Header.Get("Content-Type"))
		w.WriteHeader(upstreamResp.StatusCode)
		_, _ = io.Copy(w, upstreamResp.Body)
		return
	}
	encoded, revised := imageFromSSE(upstream.Body.Bytes())
	if encoded == "" {
		writeError(w, http.StatusBadGateway, "image_generation_failed", "the OpenAI Responses tool returned no image")
		return
	}
	data := map[string]any{"b64_json": encoded}
	if revised != "" {
		data["revised_prompt"] = revised
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"created": time.Now().Unix(), "data": []any{data}})
}

func imageFromSSE(body []byte) (encoded, revised string) {
	for _, block := range bytes.Split(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")), []byte("\n\n")) {
		for _, line := range bytes.Split(block, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			var event struct {
				Type string `json:"type"`
				Item struct {
					Type          string `json:"type"`
					Result        string `json:"result"`
					RevisedPrompt string `json:"revised_prompt"`
				} `json:"item"`
				Response struct {
					Output []struct {
						Type          string `json:"type"`
						Result        string `json:"result"`
						RevisedPrompt string `json:"revised_prompt"`
					} `json:"output"`
				} `json:"response"`
			}
			if json.Unmarshal(bytes.TrimPrefix(line, []byte("data: ")), &event) != nil {
				continue
			}
			if event.Item.Type == "image_generation_call" && event.Item.Result != "" {
				encoded, revised = event.Item.Result, event.Item.RevisedPrompt
			}
			for _, item := range event.Response.Output {
				if item.Type == "image_generation_call" && item.Result != "" {
					encoded, revised = item.Result, item.RevisedPrompt
				}
			}
		}
	}
	return encoded, revised
}
