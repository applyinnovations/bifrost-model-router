package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type imageDiagnosticsContextKey struct{}

func imageDiagnosticsFor(ctx context.Context) *imageDiagnostics {
	diag, _ := ctx.Value(imageDiagnosticsContextKey{}).(*imageDiagnostics)
	return diag
}

// Image diagnostics deliberately exclude bodies, prompts, filenames, headers,
// URL credentials and query strings. response_hop identifies only the immediate
// peer we observed; a Server header cannot prove which deeper hop emitted it.
type imageDiagnostics struct {
	RequestID         string `json:"request_id"`
	Timestamp         string `json:"timestamp"`
	Operation         string `json:"operation"`
	Stage             string `json:"stage"`
	RequestedModel    string `json:"requested_model,omitempty"`
	SelectedModel     string `json:"selected_model,omitempty"`
	UpstreamModel     string `json:"upstream_model,omitempty"`
	Provider          string `json:"provider,omitempty"`
	Capability        string `json:"capability"`
	Backend           string `json:"backend,omitempty"`
	ReferenceImages   int    `json:"reference_images"`
	UpstreamMethod    string `json:"upstream_method,omitempty"`
	UpstreamURL       string `json:"upstream_url,omitempty"`
	ResponseHop       string `json:"response_hop,omitempty"`
	UpstreamStatus    int    `json:"upstream_status,omitempty"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	UpstreamServer    string `json:"upstream_server,omitempty"`
	Status            int    `json:"status"`
	ErrorCode         string `json:"error_code,omitempty"`
}

func newImageDiagnostics(edit bool) *imageDiagnostics {
	operation, capability := "generate", "image_generation"
	if edit {
		operation, capability = "edit", "image_edit"
	}
	return &imageDiagnostics{
		RequestID: rand.Text(), Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Operation: operation, Capability: capability, Stage: "authentication",
	}
}

func (d *imageDiagnostics) writeError(w http.ResponseWriter, status int, code, message string) {
	d.Status, d.ErrorCode = status, code
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"type": "router_error", "code": code, "message": message, "diagnostics": d,
	}})
}

func (d *imageDiagnostics) log() {
	encoded, _ := json.Marshal(d)
	log.Printf("images_route %s", encoded)
}

func imageUpstreamURL(base *url.URL) string {
	target := *base
	target.Path = strings.TrimSuffix(target.Path, "/") + chatGPTResponsesPath
	if target.RawPath != "" {
		target.RawPath = strings.TrimSuffix(target.RawPath, "/") + chatGPTResponsesPath
	}
	return imageURLForDiagnostics(&target)
}

func imageURLForDiagnostics(upstream *url.URL) string {
	target := *upstream
	target.User, target.RawQuery, target.Fragment, target.RawFragment = nil, "", "", ""
	target.ForceQuery = false
	return target.String()
}

// Upstream identifiers are opaque metadata, not arbitrary header contents.
func safeImageMetadata(value string) string {
	if len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:/-", r)) {
			return ""
		}
	}
	return value
}

func imageHTTPFailure(status int) string {
	return fmt.Sprintf("the image backend returned HTTP %d; use the request_id to correlate router and Bifrost diagnostics", status)
}
