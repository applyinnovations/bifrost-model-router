package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRewriteModelPreservesNativeFields(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"opaque native fields", ` { "model" : "openai/sol", "input":[{"image_url":"data:image/png;base64,aGVsbG8=","model":"nested"}], "future":{"integer":9007199254740993,"escaped":"\u003c\/\u003e"} } `, ` { "model" : "sol", "input":[{"image_url":"data:image/png;base64,aGVsbG8=","model":"nested"}], "future":{"integer":9007199254740993,"escaped":"\u003c\/\u003e"} } `},
		{"escaped and duplicate model keys", `{"model":null,"mo\u0064el":"openai/sol","input":"hello"}`, `{"model":"sol","mo\u0064el":"sol","input":"hello"}`},
		{"case-insensitive model key", `{"MODEL":"openai/sol","input":"hello"}`, `{"MODEL":"sol","input":"hello","model":"sol"}`},
		{"model after other fields", `{"input":"hello","model":"openai/sol"}`, `{"input":"hello","model":"sol"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			got, err := rewriteModel(body, "sol")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("only model values may change: got %q", got)
			}
			if string(body) != tt.body {
				t.Fatal("input buffer mutated")
			}
		})
	}
	for _, body := range []string{`null`, `[]`, `{}`, `{"model":"sol"} trailing`, `{"model":"sol","input":}`} {
		if _, err := rewriteModel([]byte(body), "sol"); err == nil {
			t.Errorf("invalid request accepted: %q", body)
		}
	}
}

func TestReadRequestBodyBoundaries(t *testing.T) {
	const limit = 32
	for _, length := range []int64{-1, 0, limit, limit + 1} {
		for _, size := range []int{limit - 1, limit, limit + 1} {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(strings.Repeat("x", size)))
			req.ContentLength = length
			got, err := readRequestBody(httptest.NewRecorder(), req, limit)
			if length > limit || size > limit {
				var e *http.MaxBytesError
				if !errors.As(err, &e) {
					t.Fatalf("length=%d size=%d error=%v", length, size, err)
				}
				continue
			}
			if err != nil || len(got) != size {
				t.Fatalf("length=%d size=%d bytes=%d err=%v", length, size, len(got), err)
			}
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", io.MultiReader(strings.NewReader("x"), failedReader{}))
	if _, err := readRequestBody(httptest.NewRecorder(), req, limit); err == nil {
		t.Fatal("body read failure ignored")
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }

func TestResponsesRejectOversizedDeclaredBody(t *testing.T) {
	h, err := New(testConfig(t), "http://synthetic.invalid", "http://synthetic.invalid")
	if err != nil {
		t.Fatal(err)
	}
	h.bifrostProxy.Transport = imageRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("oversized request reached upstream")
		return nil, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
	req.ContentLength = maxResponsesRequestBytes + 1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), `"code":"request_too_large"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}
