package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-router/internal/api"
	"llm-router/internal/config"
	"llm-router/internal/metrics"
	"llm-router/internal/router"
	"llm-router/internal/testutil"
)

type testEnv struct {
	server *httptest.Server
	fakes  []*testutil.Fake
}

// buildTestConfig builds a router config with one logical model "logical"
// whose backends point at the given fakes, in order.
func buildTestConfig(t *testing.T, mode config.Mode, fakes ...*testutil.Fake) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration(3 * time.Second),
			RerouteTimeout: config.Duration(300 * time.Millisecond),
			Cooldown:       config.Duration(500 * time.Millisecond),
		},
		Credentials: map[string]config.Credential{},
		Models: []config.LogicalModel{{
			Name:   "logical",
			Domain: "test",
			Mode:   mode,
		}},
	}
	for i, f := range fakes {
		cred := fmt.Sprintf("cred-%d", i)
		cfg.Credentials[cred] = config.Credential{Type: "openai", APIKey: "test", BaseURL: f.URL()}
		cfg.Models[0].Backends = append(cfg.Models[0].Backends,
			config.Backend{Credential: cred, Model: fmt.Sprintf("model-%d", i)})
	}
	return cfg
}

// newTestAPI builds a router with one logical model "logical" over the given
// fakes and serves it through the API.
func newTestAPI(t *testing.T, mode config.Mode, apiKey string, fakes ...*testutil.Fake) *testEnv {
	t.Helper()
	r, err := router.NewRouter(buildTestConfig(t, mode, fakes...))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	srv := api.New(r, apiKey, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{server: ts, fakes: fakes}
}

func doJSON(t *testing.T, url string, headers map[string]string, body any) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(data)
}

func auth(t *testing.T, env *testEnv, key string) map[string]string {
	t.Helper()
	if key == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + key}
}

func TestChatCompletions_NonStreaming(t *testing.T) {
	f := testutil.NewFake(t)
	f.SetBehavior("model-0", testutil.Behavior{Body: testutil.ChatBody("model-0", "hello world")})
	env := newTestAPI(t, config.ModeFallback, "secret", f)

	resp, body := doJSON(t, env.server.URL+"/v1/chat/completions", auth(t, env, "secret"), map[string]any{
		"model":    "logical",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, body)
	}
	if out["model"] != "logical" {
		t.Errorf("model = %v, want logical (rewritten from backend name)", out["model"])
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", out["choices"])
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello world" {
		t.Errorf("content = %v", msg["content"])
	}
	// The upstream must have received the backend model name.
	if got := f.LastModel("/chat/completions"); got != "model-0" {
		t.Errorf("upstream model = %q", got)
	}
}

func TestChatCompletions_Streaming(t *testing.T) {
	f := testutil.NewFake(t)
	f.SetBehavior("model-0", testutil.Behavior{
		Events: []string{
			testutil.ChatEvent("model-0", "hel"),
			testutil.ChatEvent("model-0", "lo"),
		},
	})
	env := newTestAPI(t, config.ModeFallback, "secret", f)

	reqBody := map[string]any{
		"model":    "logical",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	resp, body := doJSON(t, env.server.URL+"/v1/chat/completions", auth(t, env, "secret"), reqBody)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}

	lines := strings.Split(body, "\n")
	var dataLines []string
	for _, l := range lines {
		if strings.HasPrefix(l, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(l, "data: "))
		}
	}
	if len(dataLines) != 3 {
		t.Fatalf("data lines = %d, want 3 (2 events + [DONE]): %v", len(dataLines), dataLines)
	}
	for i := 0; i < 2; i++ {
		var ev map[string]any
		if err := json.Unmarshal([]byte(dataLines[i]), &ev); err != nil {
			t.Fatalf("event %d not JSON: %v: %s", i, err, dataLines[i])
		}
		if ev["model"] != "logical" {
			t.Errorf("event %d model = %v, want logical", i, ev["model"])
		}
	}
	if dataLines[2] != "[DONE]" {
		t.Errorf("last data line = %q, want [DONE]", dataLines[2])
	}
	if !strings.Contains(body, "hel") || !strings.Contains(body, "lo") {
		t.Errorf("body missing deltas: %s", body)
	}
}

func TestCompletions_LegacyEndpoint(t *testing.T) {
	f := testutil.NewFake(t)
	f.SetBehavior("model-0", testutil.Behavior{Body: testutil.CompletionBody("model-0", "legacy text")})
	env := newTestAPI(t, config.ModeFallback, "secret", f)

	resp, body := doJSON(t, env.server.URL+"/v1/completions", auth(t, env, "secret"), map[string]any{
		"model":  "logical",
		"prompt": "say hi",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "logical" {
		t.Errorf("model = %v", out["model"])
	}
	if got := f.LastModel("/completions"); got != "model-0" {
		t.Errorf("upstream model = %q", got)
	}
}

func TestUnknownModel(t *testing.T) {
	f := testutil.NewFake(t)
	env := newTestAPI(t, config.ModeFallback, "secret", f)
	resp, body := doJSON(t, env.server.URL+"/v1/chat/completions", auth(t, env, "secret"), map[string]any{
		"model":    "nope",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Error.Type != "model_not_found" || !strings.Contains(out.Error.Message, "nope") {
		t.Errorf("error = %+v", out.Error)
	}
}

func TestAuth(t *testing.T) {
	f := testutil.NewFake(t)
	f.SetBehavior("model-0", testutil.Behavior{Body: testutil.ChatBody("model-0", "ok")})
	env := newTestAPI(t, config.ModeFallback, "secret", f)

	body := map[string]any{"model": "logical", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	// No key.
	resp, _ := doJSON(t, env.server.URL+"/v1/chat/completions", nil, body)
	if resp.StatusCode != 401 {
		t.Errorf("no key: status = %d, want 401", resp.StatusCode)
	}
	// Wrong key.
	resp, _ = doJSON(t, env.server.URL+"/v1/chat/completions", auth(t, env, "wrong"), body)
	if resp.StatusCode != 401 {
		t.Errorf("wrong key: status = %d, want 401", resp.StatusCode)
	}
	// Correct key.
	resp, _ = doJSON(t, env.server.URL+"/v1/chat/completions", auth(t, env, "secret"), body)
	if resp.StatusCode != 200 {
		t.Errorf("correct key: status = %d, want 200", resp.StatusCode)
	}
}

func TestAuth_Disabled(t *testing.T) {
	f := testutil.NewFake(t)
	f.SetBehavior("model-0", testutil.Behavior{Body: testutil.ChatBody("model-0", "ok")})
	env := newTestAPI(t, config.ModeFallback, "", f)
	resp, _ := doJSON(t, env.server.URL+"/v1/chat/completions", nil,
		map[string]any{"model": "logical", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 when auth is disabled", resp.StatusCode)
	}
}

func TestModelsEndpoint(t *testing.T) {
	f := testutil.NewFake(t)
	env := newTestAPI(t, config.ModeFallback, "secret", f)

	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "logical" {
		t.Errorf("models = %+v", out)
	}
}

func TestHealthEndpoint(t *testing.T) {
	f := testutil.NewFake(t)
	env := newTestAPI(t, config.ModeFallback, "secret", f)

	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Status string `json:"status"`
		Models map[string]struct {
			Domain   string `json:"domain"`
			Backends []struct {
				Name      string `json:"name"`
				Available bool   `json:"available"`
			} `json:"backends"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("invalid health JSON: %v: %s", err, data)
	}
	if out.Status != "ok" {
		t.Errorf("status = %q", out.Status)
	}
	m, ok := out.Models["logical"]
	if !ok {
		t.Fatalf("model missing: %s", data)
	}
	if m.Domain != "test" || len(m.Backends) != 1 || !m.Backends[0].Available {
		t.Errorf("model info = %+v", m)
	}
}

func TestAllBackendsFail_PassesUpstreamError(t *testing.T) {
	f1 := testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{
		Status: 429,
		Body:   `{"error":{"message":"rate limited by upstream","type":"rate_limit_error","code":"rate_limited"}}`,
	})
	env := newTestAPI(t, config.ModeFallback, "secret", f1)

	resp, body := doJSON(t, env.server.URL+"/v1/chat/completions", auth(t, env, "secret"),
		map[string]any{"model": "logical", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "rate limited by upstream") {
		t.Errorf("upstream error body not passed through: %s", body)
	}
}

func TestEndToEnd_FallbackReroute(t *testing.T) {
	f1, f2 := testutil.NewFake(t), testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{Status: 503, Body: `{"error":{"message":"unavailable"}}`})
	f2.SetBehavior("model-1", testutil.Behavior{Body: testutil.ChatBody("model-1", "recovered")})
	env := newTestAPI(t, config.ModeFallback, "secret", f1, f2)

	resp, body := doJSON(t, env.server.URL+"/v1/chat/completions", auth(t, env, "secret"),
		map[string]any{"model": "logical", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	if out["model"] != "logical" {
		t.Errorf("model = %v", out["model"])
	}
	if !strings.Contains(body, "recovered") {
		t.Errorf("body = %s", body)
	}
}

func TestBadJSON(t *testing.T) {
	f := testutil.NewFake(t)
	env := newTestAPI(t, config.ModeFallback, "secret", f)

	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/v1/chat/completions",
		strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestClientCancellation(t *testing.T) {
	f := testutil.NewFake(t)
	// Slow first byte so the attempt is still in flight when the client
	// cancels.
	f.SetBehavior("model-0", testutil.Behavior{
		Delay: 500 * time.Millisecond,
		Body:  testutil.ChatBody("model-0", "too late"),
	})
	env := newTestAPI(t, config.ModeFallback, "secret", f)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, env.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"logical","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	client := &http.Client{Timeout: 2 * time.Second}
	_, err := client.Do(req)
	if err == nil {
		t.Fatal("expected an error after client cancellation")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	f := testutil.NewFake(t)
	f.SetBehavior("model-0", testutil.Behavior{Body: testutil.ChatBody("model-0", "ok")})

	mts := metrics.New()
	r, err := router.NewRouter(buildTestConfig(t, config.ModeFallback, f), router.WithMetrics(mts))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	ts := httptest.NewServer(api.New(r, "", nil, api.WithMetrics(mts)).Handler())
	t.Cleanup(ts.Close)

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(data)
	}

	// Before any traffic: the endpoint is up and exposes static gauges.
	code, body := get("/metrics")
	if code != 200 {
		t.Fatalf("GET /metrics: status = %d, want 200", code)
	}
	if !strings.Contains(body, "llm_router_backend_up") || !strings.Contains(body, "llm_router_inflight_requests") {
		t.Errorf("metrics body missing expected series:\n%s", body)
	}

	// Generate one successful request.
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"logical","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chat: status = %d, want 200", resp.StatusCode)
	}

	code, body = get("/metrics")
	if code != 200 {
		t.Fatalf("GET /metrics after request: status = %d, want 200", code)
	}
	for _, want := range []string{
		`llm_router_requests_total{mode="fallback",model="logical",status="success",stream="false"} 1`,
		`llm_router_backend_attempts_total{backend="model-0@cred-0",model="logical",result="success"} 1`,
		"llm_router_request_duration_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\n%s", want, body)
		}
	}
}

// TestThinkingPassthrough verifies that reasoning ("thinking") fields survive
// the router's pass-through in both modes and that thinking request
// parameters (vLLM's chat_template_kwargs) reach the upstream unchanged.
func TestThinkingPassthrough(t *testing.T) {
	const reasoning = "step 1: consider parity. step 2: conclude even."

	t.Run("non-streaming", func(t *testing.T) {
		f := testutil.NewFake(t)
		f.SetBehavior("model-0", testutil.Behavior{Body: fmt.Sprintf(`{
			"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"model-0",
			"choices":[{"index":0,"message":{"role":"assistant","content":"the answer","reasoning":%q},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":20,"total_tokens":25}
		}`, reasoning)})
		env := newTestAPI(t, config.ModeFallback, "secret", f)

		resp, body := doJSON(t, env.server.URL+"/v1/chat/completions", auth(t, env, "secret"), map[string]any{
			"model":                "logical",
			"messages":             []any{map[string]any{"role": "user", "content": "prove it"}},
			"chat_template_kwargs": map[string]any{"enable_thinking": true},
		})
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("invalid JSON: %v: %s", err, body)
		}
		if out["model"] != "logical" {
			t.Errorf("model = %v, want logical", out["model"])
		}
		msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg["reasoning"] != reasoning {
			t.Errorf("reasoning = %v, want pass-through of upstream value", msg["reasoning"])
		}
		if msg["content"] != "the answer" {
			t.Errorf("content = %v", msg["content"])
		}
		// The thinking flag must reach the upstream unchanged.
		if _, ok := f.LastRequest("/chat/completions")["chat_template_kwargs"]; !ok {
			t.Errorf("chat_template_kwargs not forwarded to upstream: %v", f.LastRequest("/chat/completions"))
		}
	})

	t.Run("streaming", func(t *testing.T) {
		f := testutil.NewFake(t)
		f.SetBehavior("model-0", testutil.Behavior{Events: []string{
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"model-0","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"model-0","choices":[{"index":0,"delta":{"reasoning":"step 1: "},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"model-0","choices":[{"index":0,"delta":{"reasoning":"conclude even."},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"model-0","choices":[{"index":0,"delta":{"content":"the answer"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"model-0","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		}})
		env := newTestAPI(t, config.ModeFallback, "secret", f)

		resp, body := doJSON(t, env.server.URL+"/v1/chat/completions", auth(t, env, "secret"), map[string]any{
			"model":                "logical",
			"stream":               true,
			"messages":             []any{map[string]any{"role": "user", "content": "prove it"}},
			"chat_template_kwargs": map[string]any{"enable_thinking": true},
		})
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		for _, want := range []string{
			`"reasoning":"step 1: "`,
			`"reasoning":"conclude even."`,
			`"content":"the answer"`,
			`"model":"logical"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("stream body missing %q\n%s", want, body)
			}
		}
	})
}

func TestMetricsEndpoint_Disabled(t *testing.T) {
	f := testutil.NewFake(t)
	f.SetBehavior("model-0", testutil.Behavior{Body: testutil.ChatBody("model-0", "ok")})
	r, err := router.NewRouter(buildTestConfig(t, config.ModeFallback, f))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	ts := httptest.NewServer(api.New(r, "", nil).Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("GET /metrics without metrics enabled: status = %d, want 404", resp.StatusCode)
	}
}
