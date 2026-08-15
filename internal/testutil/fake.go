// Package testutil provides a fake OpenAI-compatible upstream for tests.
package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Behavior configures how the fake upstream answers for one model.
type Behavior struct {
	// Delay before the first response byte (headers).
	Delay time.Duration
	// Status is the HTTP status to return (0 => 200).
	Status int
	// Body is the JSON body: the completion payload when Status==0 and the
	// request is not streaming, or the error payload when Status!=0.
	Body string
	// Events are the SSE data payloads (JSON strings) for streaming.
	Events []string
	// EventDelay is the delay between streaming events.
	EventDelay time.Duration
	// Drop closes the connection without sending a response.
	Drop bool
}

// Fake is a fake OpenAI-compatible upstream (also usable as an Azure
// endpoint, since the SDK rewrites the path before it reaches us).
type Fake struct {
	*httptest.Server

	mu          sync.Mutex
	behavior    map[string]Behavior
	hits        map[string]int
	lastModel   map[string]string // path -> model field of last request
	lastRequest map[string]map[string]any
	lastURL     map[string]string // path -> full request URL (with query)
	lastAuth    map[string]string // path -> Authorization header of last request
	lastAPIKey  map[string]string // path -> Api-Key header of last request
}

// NewFake starts a fake upstream. It is closed automatically by t.Cleanup.
func NewFake(t *testing.T) *Fake {
	t.Helper()
	f := &Fake{
		behavior:    map[string]Behavior{},
		hits:        map[string]int{},
		lastModel:   map[string]string{},
		lastRequest: map[string]map[string]any{},
		lastURL:     map[string]string{},
		lastAuth:    map[string]string{},
		lastAPIKey:  map[string]string{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

// URL returns the base URL to use as an OpenAI-compatible base_url.
func (f *Fake) URL() string { return f.Server.URL + "/" }

// SetBehavior configures the response for a model.
func (f *Fake) SetBehavior(model string, b Behavior) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behavior[model] = b
}

// Hits returns how many requests a model received.
func (f *Fake) Hits(model string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[model]
}

// LastModel returns the "model" field of the last request seen on path.
func (f *Fake) LastModel(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastModel[path]
}

// LastRequest returns the last request body seen on path.
func (f *Fake) LastRequest(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRequest[path]
}

// LastURL returns the full request URL (path + query) last seen on path.
func (f *Fake) LastURL(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastURL[path]
}

// LastAuth returns the Authorization header of the last request seen on path.
func (f *Fake) LastAuth(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth[path]
}

// LastAPIKey returns the Api-Key header of the last request seen on path.
func (f *Fake) LastAPIKey(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAPIKey[path]
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &req)

	f.mu.Lock()
	f.hits[req.Model]++
	f.lastModel[r.URL.Path] = req.Model
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.lastRequest[r.URL.Path] = body
	f.lastURL[r.URL.Path] = r.URL.String()
	f.lastAuth[r.URL.Path] = r.Header.Get("Authorization")
	f.lastAPIKey[r.URL.Path] = r.Header.Get("Api-Key")
	b, ok := f.behavior[req.Model]
	f.mu.Unlock()

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`)
		return
	}

	if b.Drop {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
		return
	}

	if b.Delay > 0 {
		time.Sleep(b.Delay)
	}

	if b.Status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(b.Status)
		fmt.Fprint(w, b.Body)
		return
	}

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i, ev := range b.Events {
			fmt.Fprintf(w, "data: %s\n\n", ev)
			flusher.Flush()
			if i < len(b.Events)-1 && b.EventDelay > 0 {
				time.Sleep(b.EventDelay)
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, b.Body)
}

// ChatBody builds a minimal OpenAI chat.completion JSON body.
func ChatBody(model, content string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`, model, content)
}

// CompletionBody builds a minimal OpenAI text_completion JSON body.
func CompletionBody(model, text string) string {
	return fmt.Sprintf(`{"id":"cmpl-1","object":"text_completion","created":1700000000,"model":%q,"choices":[{"text":%q,"index":0,"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`, model, text)
}

// ChatEvent builds a minimal streaming chat.completion.chunk JSON body.
func ChatEvent(model, delta string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":%q,"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, model, delta)
}
