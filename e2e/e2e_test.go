// Package e2e runs the router end-to-end against a real OpenAI-compatible
// upstream (a local vLLM server by default).
//
// The tests are skipped automatically when the upstream is not reachable or
// no API key is configured, so `go test ./...` stays green in environments
// without the endpoint.
//
// Configuration (from the environment or a .env file in the working
// directory; .env takes priority):
//
//	E2E_BASE_URL  default http://192.168.18.200:1235/v1
//	E2E_API_KEY   required; tests skip when unset (never hardcode keys)
//	E2E_MODEL     default qwen3.8-27b
//
// Run explicitly:
//
//	go test -v -timeout 5m ./e2e/
package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"llm-router/internal/api"
	"llm-router/internal/config"
	"llm-router/internal/envfile"
	"llm-router/internal/router"
)

const (
	defaultBaseURL = "http://192.168.18.200:1235/v1"
	defaultModel   = "qwen3.8-27b"
)

// TestMain loads .env (if present) so E2E_* variables can live there
// instead of being exported or hardcoded. `go test` runs the binary in the
// package dir, so look there first, then in the repo root.
func TestMain(m *testing.M) {
	for _, path := range []string{envfile.DefaultPath, "../" + envfile.DefaultPath} {
		if _, err := envfile.Load(path); err == nil {
			break
		} else if !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "e2e: cannot load", path, ":", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// skipIfNoAPIKey skips the test when E2E_API_KEY is not set (via .env or the
// environment). Keys are never hardcoded in the source tree.
func skipIfNoAPIKey(t *testing.T, apiKey string) {
	t.Helper()
	if apiKey == "" {
		t.Skip("E2E_API_KEY not set (put it in .env or the environment); skipping e2e")
	}
}

// skipIfUpstreamDown pings the upstream and skips the test when it is not
// reachable, so the suite stays green without the local endpoint.
func skipIfUpstreamDown(t *testing.T, baseURL, apiKey string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		t.Skipf("building probe request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("upstream %s not reachable, skipping e2e: %v", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("upstream %s returned %d, skipping e2e", baseURL, resp.StatusCode)
	}
}

// newE2EServer wires the router in front of the real upstream and returns a
// live HTTP server exposing the OpenAI-compatible API.
func newE2EServer(t *testing.T, baseURL, apiKey, upstreamModel string) *httptest.Server {
	t.Helper()

	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration(90 * time.Second),
			RerouteTimeout: config.Duration(30 * time.Second),
		},
		Credentials: map[string]config.Credential{
			"vllm": {Type: "openai", APIKey: apiKey, BaseURL: baseURL},
		},
		Models: []config.LogicalModel{{
			Name:   "e2e-chat",
			Domain: "e2e",
			Mode:   config.ModeFallback,
			Backends: []config.Backend{
				{Credential: "vllm", Model: upstreamModel},
			},
		}},
	}
	r, err := router.NewRouter(cfg)
	if err != nil {
		t.Fatalf("building router: %v", err)
	}
	srv := api.New(r, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// newE2ERoundRobinServer wires the router in front of the real upstream with
// a single round-robin logical model backed by numBackends identical
// backends (same upstream model, distinct credentials so each backend has a
// distinct name). This lets the test observe the round-robin distribution via
// the per-backend hit counters in /healthz.
func newE2ERoundRobinServer(t *testing.T, baseURL, apiKey, upstreamModel string, numBackends int) *httptest.Server {
	t.Helper()

	creds := make(map[string]config.Credential, numBackends)
	backends := make([]config.Backend, 0, numBackends)
	for i := 1; i <= numBackends; i++ {
		name := fmt.Sprintf("vllm-%d", i)
		creds[name] = config.Credential{Type: "openai", APIKey: apiKey, BaseURL: baseURL}
		backends = append(backends, config.Backend{Credential: name, Model: upstreamModel})
	}

	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration(90 * time.Second),
			RerouteTimeout: config.Duration(30 * time.Second),
		},
		Credentials: creds,
		Models: []config.LogicalModel{{
			Name:     "e2e-rr",
			Domain:   "e2e",
			Mode:     config.ModeRoundRobin,
			Backends: backends,
		}},
	}
	r, err := router.NewRouter(cfg)
	if err != nil {
		t.Fatalf("building router: %v", err)
	}
	srv := api.New(r, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// chatRequest builds a small, fast chat completion request for the given
// logical model. Thinking is disabled via chat_template_kwargs (vLLM/qwen)
// to keep the test quick.
func chatRequest(model string, stream bool) map[string]any {
	return map[string]any{
		"model":  model,
		"stream": stream,
		"messages": []any{
			map[string]any{"role": "user", "content": "Reply with exactly: pong"},
		},
		"max_tokens":           32,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
}

func doChat(t *testing.T, ts *httptest.Server, body map[string]any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", &buf)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestE2E_NonStreaming(t *testing.T) {
	baseURL := envOr("E2E_BASE_URL", defaultBaseURL)
	apiKey := envOr("E2E_API_KEY", "")
	skipIfNoAPIKey(t, apiKey)
	upstreamModel := envOr("E2E_MODEL", defaultModel)
	skipIfUpstreamDown(t, baseURL, apiKey)

	ts := newE2EServer(t, baseURL, apiKey, upstreamModel)

	resp := doChat(t, ts, chatRequest("e2e-chat", false))
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body: %s", resp.StatusCode, data)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshaling response: %v\nbody: %s", err, data)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1\nbody: %s", len(out.Choices), data)
	}
	// The router rewrites the response model to the logical name.
	if out.Model != "e2e-chat" {
		t.Errorf("model = %q, want e2e-chat", out.Model)
	}
	if out.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", out.Choices[0].Message.Role)
	}
	if strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		t.Errorf("empty content\nbody: %s", data)
	}
	t.Logf("non-streaming OK: model=%s content=%q finish=%s",
		out.Model, out.Choices[0].Message.Content, out.Choices[0].FinishReason)
}

func TestE2E_Streaming(t *testing.T) {
	baseURL := envOr("E2E_BASE_URL", defaultBaseURL)
	apiKey := envOr("E2E_API_KEY", "")
	skipIfNoAPIKey(t, apiKey)
	upstreamModel := envOr("E2E_MODEL", defaultModel)
	skipIfUpstreamDown(t, baseURL, apiKey)

	ts := newE2EServer(t, baseURL, apiKey, upstreamModel)

	resp := doChat(t, ts, chatRequest("e2e-chat", true))
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body: %s", resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	var (
		events  int
		sawDone bool
		content strings.Builder
		models  = map[string]bool{}
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var ev struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("unmarshaling SSE event: %v\nline: %s", err, line)
		}
		events++
		models[ev.Model] = true
		for _, c := range ev.Choices {
			content.WriteString(c.Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading SSE stream: %v", err)
	}

	if events == 0 {
		t.Fatal("no SSE events received")
	}
	if !sawDone {
		t.Error("stream did not end with [DONE]")
	}
	// Every event's model must be rewritten to the logical name.
	for m := range models {
		if m != "e2e-chat" {
			t.Errorf("event model = %q, want e2e-chat", m)
		}
	}
	if strings.TrimSpace(content.String()) == "" {
		t.Error("streamed content is empty")
	}
	t.Logf("streaming OK: events=%d content=%q", events, content.String())
}

func TestE2E_ModelsEndpoint(t *testing.T) {
	baseURL := envOr("E2E_BASE_URL", defaultBaseURL)
	apiKey := envOr("E2E_API_KEY", "")
	skipIfNoAPIKey(t, apiKey)
	upstreamModel := envOr("E2E_MODEL", defaultModel)
	skipIfUpstreamDown(t, baseURL, apiKey)

	ts := newE2EServer(t, baseURL, apiKey, upstreamModel)

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshaling: %v\nbody: %s", err, data)
	}
	found := false
	for _, m := range out.Data {
		if m.ID == "e2e-chat" {
			found = true
		}
	}
	if !found {
		t.Fatalf("logical model e2e-chat not listed: %s", data)
	}
	t.Logf("models endpoint OK: %s", data)
}

// TestE2E_RoundRobin_Distributes verifies that a round-robin logical model
// spreads requests across all of its backends. All backends point at the same
// real upstream (same model), but each uses a distinct credential so the
// router tracks them separately; the per-backend hit counters in /healthz
// reveal the distribution.
func TestE2E_RoundRobin_Distributes(t *testing.T) {
	baseURL := envOr("E2E_BASE_URL", defaultBaseURL)
	apiKey := envOr("E2E_API_KEY", "")
	skipIfNoAPIKey(t, apiKey)
	upstreamModel := envOr("E2E_MODEL", defaultModel)
	skipIfUpstreamDown(t, baseURL, apiKey)

	const (
		numBackends = 4
		numRequests = 8
	)
	ts := newE2ERoundRobinServer(t, baseURL, apiKey, upstreamModel, numBackends)

	// Send numRequests non-streaming requests; all must succeed.
	for i := 0; i < numRequests; i++ {
		resp := doChat(t, ts, chatRequest("e2e-rr", false))
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("request %d: status = %d, body: %s", i+1, resp.StatusCode, data)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	// Read the per-backend hit counters from /healthz.
	hz, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer hz.Body.Close()
	data, err := io.ReadAll(hz.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Models map[string]struct {
			Backends []struct {
				Name  string `json:"name"`
				Hits  int64  `json:"hits"`
				Fails int64  `json:"fails"`
			} `json:"backends"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshaling /healthz: %v\nbody: %s", err, data)
	}
	model, ok := out.Models["e2e-rr"]
	if !ok {
		t.Fatalf("model e2e-rr not in /healthz: %s", data)
	}
	if len(model.Backends) != numBackends {
		t.Fatalf("backends = %d, want %d\nbody: %s", len(model.Backends), numBackends, data)
	}

	var total int64
	dist := make(map[string]int64, len(model.Backends))
	for _, b := range model.Backends {
		total += b.Hits
		dist[b.Name] = b.Hits
		if b.Hits < 1 {
			t.Errorf("backend %s hits = %d, want >= 1 (round-robin should use every backend)", b.Name, b.Hits)
		}
		if b.Fails > 0 {
			t.Errorf("backend %s fails = %d, want 0", b.Name, b.Fails)
		}
	}
	if total != numRequests {
		t.Errorf("total hits = %d, want %d", total, numRequests)
	}
	t.Logf("round-robin distribution over %d backends: %v", numBackends, dist)
}

// thinkingPrompt is a long prompt that reliably triggers substantial
// reasoning (several seconds of thinking) on qwen3 thinking models.
const thinkingPrompt = `Explain step by step why the product of any even integer and any odd integer is always even.
Then prove that the sum of two even integers is even.
Then prove that if n^2 is even then n is even, using proof by contradiction.
Show all work, define your terms, and conclude with a single summary paragraph.`

// newE2EThinkingServer is like newE2EServer but with generous timeouts:
// thinking models can take a long time before a non-streaming response
// arrives (headers and body come together), so the reroute gate must be wide
// enough to cover a full thinking response.
func newE2EThinkingServer(t *testing.T, baseURL, apiKey, upstreamModel string) *httptest.Server {
	t.Helper()

	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration(240 * time.Second),
			RerouteTimeout: config.Duration(120 * time.Second),
		},
		Credentials: map[string]config.Credential{
			"vllm": {Type: "openai", APIKey: apiKey, BaseURL: baseURL},
		},
		Models: []config.LogicalModel{{
			Name:   "e2e-think",
			Domain: "e2e",
			Mode:   config.ModeFallback,
			Backends: []config.Backend{
				{Credential: "vllm", Model: upstreamModel},
			},
		}},
	}
	r, err := router.NewRouter(cfg)
	if err != nil {
		t.Fatalf("building router: %v", err)
	}
	srv := api.New(r, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// thinkingRequest builds a long-prompt chat completion request with thinking
// enabled (vLLM/qwen chat_template_kwargs).
func thinkingRequest(model string, stream bool) map[string]any {
	return map[string]any{
		"model":  model,
		"stream": stream,
		"messages": []any{
			map[string]any{"role": "user", "content": thinkingPrompt},
		},
		"max_tokens":           2000,
		"chat_template_kwargs": map[string]any{"enable_thinking": true},
	}
}

// reasoningText returns the reasoning text of a message, checking both the
// vLLM "reasoning" field and the "reasoning_content" variant used by some
// upstreams.
func reasoningText(m map[string]any) string {
	if s, ok := m["reasoning"].(string); ok {
		return s
	}
	if s, ok := m["reasoning_content"].(string); ok {
		return s
	}
	return ""
}

// TestE2E_Thinking_NonStreaming sends a long prompt with thinking enabled and
// verifies the full response (reasoning + content) passes through the router
// within the generous thinking timeouts.
func TestE2E_Thinking_NonStreaming(t *testing.T) {
	baseURL := envOr("E2E_BASE_URL", defaultBaseURL)
	apiKey := envOr("E2E_API_KEY", "")
	skipIfNoAPIKey(t, apiKey)
	upstreamModel := envOr("E2E_MODEL", defaultModel)
	skipIfUpstreamDown(t, baseURL, apiKey)

	ts := newE2EThinkingServer(t, baseURL, apiKey, upstreamModel)

	resp := doChat(t, ts, thinkingRequest("e2e-think", false))
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body: %s", resp.StatusCode, data)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      map[string]any `json:"message"`
			FinishReason string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshaling response: %v\nbody: %s", err, data)
	}
	if out.Model != "e2e-think" {
		t.Errorf("model = %q, want e2e-think", out.Model)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1\nbody: %s", len(out.Choices), data)
	}
	msg := out.Choices[0].Message
	reasoning := reasoningText(msg)
	if len(reasoning) < 100 {
		t.Errorf("reasoning = %d chars, want substantial thinking (>=100)\nmessage: %v", len(reasoning), msg)
	}
	content, _ := msg["content"].(string)
	if strings.TrimSpace(content) == "" {
		t.Errorf("empty content\nbody: %s", data)
	}
	t.Logf("thinking non-streaming OK: reasoning=%d chars content=%d chars finish=%s",
		len(reasoning), len(content), out.Choices[0].FinishReason)
}

// TestE2E_Thinking_Streaming sends a long prompt with thinking enabled and
// verifies reasoning deltas and content deltas both pass through the SSE
// stream.
func TestE2E_Thinking_Streaming(t *testing.T) {
	baseURL := envOr("E2E_BASE_URL", defaultBaseURL)
	apiKey := envOr("E2E_API_KEY", "")
	skipIfNoAPIKey(t, apiKey)
	upstreamModel := envOr("E2E_MODEL", defaultModel)
	skipIfUpstreamDown(t, baseURL, apiKey)

	ts := newE2EThinkingServer(t, baseURL, apiKey, upstreamModel)

	resp := doChat(t, ts, thinkingRequest("e2e-think", true))
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body: %s", resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	var (
		events       int
		sawDone      bool
		reasoning    strings.Builder
		content      strings.Builder
		finishReason string
		models       = map[string]bool{}
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var ev struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("unmarshaling SSE event: %v\nline: %s", err, line)
		}
		events++
		models[ev.Model] = true
		for _, c := range ev.Choices {
			reasoning.WriteString(c.Delta.Reasoning)
			reasoning.WriteString(c.Delta.ReasoningContent)
			content.WriteString(c.Delta.Content)
			if c.FinishReason != "" {
				finishReason = c.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading SSE stream: %v", err)
	}

	if events == 0 {
		t.Fatal("no SSE events received")
	}
	if !sawDone {
		t.Error("stream did not end with [DONE]")
	}
	for m := range models {
		if m != "e2e-think" {
			t.Errorf("event model = %q, want e2e-think", m)
		}
	}
	if reasoning.Len() < 100 {
		t.Errorf("streamed reasoning = %d chars, want substantial thinking (>=100)", reasoning.Len())
	}
	if strings.TrimSpace(content.String()) == "" {
		t.Error("streamed content is empty")
	}
	if finishReason == "" {
		t.Error("no finish_reason seen in stream")
	}
	t.Logf("thinking streaming OK: events=%d reasoning=%d chars content=%d chars finish=%s",
		events, reasoning.Len(), content.Len(), finishReason)
}
