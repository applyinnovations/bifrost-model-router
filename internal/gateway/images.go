package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/applyinnovations/bifrost-model-router/internal/config"
)

const (
	maxImageRequestBytes = 64 << 20
	// Base64 plus its data-URL prefix must fit the Responses image_url limit.
	maxReferenceImageBytes = 14 << 20
	maxReferenceImages     = 16
)

type imageReference struct {
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

type imageRequest struct {
	Model             string           `json:"model"`
	Prompt            string           `json:"prompt"`
	N                 int              `json:"n"`
	Size              string           `json:"size"`
	Quality           string           `json:"quality"`
	Background        string           `json:"background"`
	OutputFormat      string           `json:"output_format"`
	OutputCompression *int             `json:"output_compression"`
	InputFidelity     string           `json:"input_fidelity"`
	Moderation        string           `json:"moderation"`
	ResponseFormat    string           `json:"response_format"`
	Stream            bool             `json:"stream"`
	PartialImages     int              `json:"partial_images"`
	User              string           `json:"user"`
	Images            []imageReference `json:"images"`
	Mask              json.RawMessage  `json:"mask"`
}

// Both Images operations use the hosted tool on the configured native OpenAI
// Responses model. Codex login tokens lack the scope for direct Images API
// passthrough. Reference bytes arrive in the HTTP upload, never as local paths
// to be opened inside the gateway container.
func (h *Handler) serveImageGeneration(w http.ResponseWriter, req *http.Request) {
	h.serveImages(w, req, false)
}

func (h *Handler) serveImageEdit(w http.ResponseWriter, req *http.Request) {
	h.serveImages(w, req, true)
}

func (h *Handler) serveImages(w http.ResponseWriter, req *http.Request, edit bool) {
	diag := newImageDiagnostics(edit)
	defer diag.log()
	w.Header().Set("X-Request-ID", diag.RequestID)
	w.Header().Set("Cache-Control", "no-store")
	if req.Header.Get("x-bf-vk") == "" {
		diag.writeError(w, http.StatusUnauthorized, "missing_bifrost_auth", "a Bifrost virtual key is required")
		return
	}
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
		diag.writeError(w, http.StatusUnauthorized, "missing_openai_auth", "Codex OpenAI authentication is required")
		return
	}
	diag.Stage = "request_parse"
	req.Body = http.MaxBytesReader(w, req.Body, maxImageRequestBytes)
	imageReq, parseErr := parseImageRequest(req, edit, diag)
	if parseErr != nil {
		status := http.StatusBadRequest
		var limitErr *http.MaxBytesError
		if errors.As(parseErr, &limitErr) {
			status = http.StatusRequestEntityTooLarge
		}
		diag.writeError(w, status, "invalid_image_request", "expected a valid Images request within the upload limits")
		return
	}
	diag.RequestedModel = imageReq.Model
	diag.ReferenceImages = len(imageReq.Images)
	if imageReq.Model == "" || strings.TrimSpace(imageReq.Prompt) == "" {
		diag.writeError(w, http.StatusBadRequest, "invalid_request", "model and prompt are required")
		return
	}
	if imageReq.N > 1 || imageReq.N < 0 {
		diag.writeError(w, http.StatusBadRequest, "unsupported_image_count", "only one image per request is supported")
		return
	}
	if len(imageReq.Mask) > 0 && string(imageReq.Mask) != "null" {
		diag.writeError(w, http.StatusBadRequest, "image_mask_unsupported", "masked image edits are not supported by this bridge")
		return
	}
	if imageReq.Stream || imageReq.PartialImages != 0 || imageReq.ResponseFormat != "" && imageReq.ResponseFormat != "b64_json" {
		diag.writeError(w, http.StatusBadRequest, "image_response_format_unsupported", "this bridge returns one complete base64 image in JSON")
		return
	}
	if edit && len(imageReq.Images) == 0 || len(imageReq.Images) > maxReferenceImages {
		diag.writeError(w, http.StatusBadRequest, "invalid_reference_images", "image edits require between one and sixteen reference images")
		return
	}
	if !edit && len(imageReq.Images) > 0 {
		diag.writeError(w, http.StatusBadRequest, "image_operation_unsupported", "reference images must be sent to /v1/images/edits")
		return
	}
	diag.Stage = "reference_validation"
	for _, ref := range imageReq.Images {
		if ref.FileID != "" {
			diag.writeError(w, http.StatusBadRequest, "image_file_reference_unsupported", "upload image bytes or supply image_url; Files API references are not supported by this bridge")
			return
		}
		if !validImageURL(ref.ImageURL) {
			diag.writeError(w, http.StatusBadRequest, "invalid_reference_image", "reference images must be PNG, JPEG or WebP data URLs, or HTTPS URLs")
			return
		}
	}
	diag.Stage = "backend_selection"
	resolved, ok := h.cfg.ResolveModel(h.cfg.ImageGenerationModel)
	if h.cfg.ImageGenerationModel == "" || !ok || resolved.Model.Provider != "openai" || resolved.Provider.CredentialMode != config.CredentialRequestPassthrough || resolved.Model.ResponsesMode != config.ResponsesNative {
		diag.writeError(w, http.StatusServiceUnavailable, "image_operation_unsupported", "no compatible native OpenAI Responses image backend is configured")
		return
	}
	diag.SelectedModel, diag.UpstreamModel, diag.Provider = resolved.Slug, resolved.UpstreamModel, resolved.Model.Provider
	diag.Backend = "native_responses_image_tool"
	diag.Stage = "payload_conversion"
	tool := map[string]any{"type": "image_generation", "model": imageReq.Model, "action": diag.Operation}
	for name, value := range map[string]string{
		"size": imageReq.Size, "quality": imageReq.Quality,
		"background": imageReq.Background, "output_format": imageReq.OutputFormat,
		"input_fidelity": imageReq.InputFidelity, "moderation": imageReq.Moderation,
	} {
		if value != "" {
			tool[name] = value
		}
	}
	if imageReq.OutputCompression != nil {
		tool["output_compression"] = *imageReq.OutputCompression
	}
	content := []any{map[string]any{"type": "input_text", "text": imageReq.Prompt}}
	for _, ref := range imageReq.Images {
		content = append(content, map[string]any{"type": "input_image", "image_url": ref.ImageURL, "detail": "auto"})
	}
	envelope := map[string]any{
		"model": resolved.Slug,
		"input": []any{map[string]any{"role": "user", "content": content}},
		"tools": []any{tool}, "tool_choice": map[string]string{"type": "image_generation"},
		"store": false, "stream": true,
	}
	if imageReq.User != "" {
		envelope["user"] = imageReq.User
	}
	responseBody, err := json.Marshal(envelope)
	if err != nil {
		diag.writeError(w, http.StatusInternalServerError, "image_request_failed", "could not construct image request")
		return
	}
	forward := req.Clone(context.WithValue(req.Context(), imageDiagnosticsContextKey{}, diag))
	forward.URL.Path, forward.URL.RawPath, forward.URL.RawQuery = "/v1/responses", "", ""
	forward.Body = io.NopCloser(bytes.NewReader(responseBody))
	forward.ContentLength = int64(len(responseBody))
	forward.Header = req.Header.Clone()
	forward.Header.Del("Content-Length")
	forward.Header.Set("Content-Type", "application/json")
	forward.Header.Set("X-Request-ID", diag.RequestID)
	diag.Stage = "upstream_http"
	diag.UpstreamMethod, diag.UpstreamURL = http.MethodPost, imageUpstreamURL(h.bifrostURL)
	// The Responses dispatcher owns model rewriting and request-scoped auth.
	upstream := httptest.NewRecorder()
	h.serveResponses(upstream, forward)
	upstreamResp := upstream.Result()
	defer upstreamResp.Body.Close()
	if upstreamResp.StatusCode != http.StatusOK {
		// Never expose raw upstream error bodies: they may contain HTML,
		// prompts, image data, credentials or other private request details.
		if diag.Stage == "upstream_transport" {
			diag.writeError(w, http.StatusBadGateway, "image_upstream_unavailable", "could not reach the image backend")
		} else {
			diag.writeError(w, upstreamResp.StatusCode, "image_upstream_error", imageHTTPFailure(upstreamResp.StatusCode))
		}
		return
	}
	diag.Stage = "response_decode"
	encoded, revised := imageFromSSE(upstream.Body.Bytes())
	if encoded == "" {
		diag.writeError(w, http.StatusBadGateway, "image_generation_failed", "the OpenAI Responses tool returned no image")
		return
	}
	data := map[string]any{"b64_json": encoded}
	if revised != "" {
		data["revised_prompt"] = revised
	}
	diag.Stage, diag.Status = "complete", http.StatusOK
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"created": time.Now().Unix(), "data": []any{data}})
}

func parseImageRequest(req *http.Request, edit bool, diag *imageDiagnostics) (imageRequest, error) {
	var result imageRequest
	mediaType, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil && req.Header.Get("Content-Type") != "" {
		return result, err
	}
	if edit && mediaType == "multipart/form-data" {
		reader, err := req.MultipartReader()
		if err != nil {
			return result, err
		}
		fields := map[string]any{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return result, err
			}
			name := part.FormName()
			if name == "image" || name == "image[]" {
				diag.Stage = "reference_upload"
				data, err := io.ReadAll(io.LimitReader(part, maxReferenceImageBytes+1))
				if err != nil {
					return result, err
				}
				if len(data) > maxReferenceImageBytes || len(result.Images) >= maxReferenceImages {
					return result, errors.New("reference image limit exceeded")
				}
				imageURL, err := referenceDataURL(data)
				if err != nil {
					return result, err
				}
				result.Images = append(result.Images, imageReference{ImageURL: imageURL})
				diag.ReferenceImages = len(result.Images)
				diag.Stage = "request_parse"
				continue
			}
			if part.FileName() != "" && name != "mask" {
				return result, errors.New("unexpected file field")
			}
			if _, exists := fields[name]; exists {
				return result, errors.New("duplicate field")
			}
			if name == "mask" {
				fields[name] = true
				continue
			}
			data, err := io.ReadAll(io.LimitReader(part, (64<<10)+1))
			if err != nil {
				return result, err
			}
			if len(data) > 64<<10 {
				return result, errors.New("field limit exceeded")
			}
			value := string(data)
			switch name {
			case "n", "output_compression", "partial_images":
				number, err := strconv.Atoi(value)
				if err != nil {
					return result, err
				}
				fields[name] = number
			case "stream":
				boolean, err := strconv.ParseBool(value)
				if err != nil {
					return result, err
				}
				fields[name] = boolean
			default:
				fields[name] = value
			}
		}
		images := result.Images
		data, err := json.Marshal(fields)
		if err != nil {
			return result, err
		}
		if err := decodeImageJSON(bytes.NewReader(data), &result); err != nil {
			return result, err
		}
		result.Images = images
		return result, nil
	}
	if mediaType != "" && mediaType != "application/json" {
		return result, errors.New("unsupported content type")
	}
	err = decodeImageJSON(req.Body, &result)
	return result, err
}

func decodeImageJSON(reader io.Reader, result *imageRequest) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("expected one JSON object")
	} else if err != io.EOF {
		return err
	}
	return nil
}

func referenceDataURL(data []byte) (string, error) {
	if len(data) > maxReferenceImageBytes {
		return "", errors.New("reference image limit exceeded")
	}
	mediaType := http.DetectContentType(data)
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp":
		return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	default:
		return "", errors.New("unsupported reference image format")
	}
}

func validImageURL(value string) bool {
	if strings.HasPrefix(value, "data:") {
		prefix, data, ok := strings.Cut(value, ",")
		if !ok || len(data) > base64.StdEncoding.EncodedLen(maxReferenceImageBytes) {
			return false
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return false
		}
		encoded, err := referenceDataURL(decoded)
		return err == nil && strings.HasPrefix(encoded, prefix+",")
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
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
