package router_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"llm-router/internal/config"
	"llm-router/internal/router"
	"llm-router/internal/testutil"
)

// buildRouter creates a router with one logical model "logical" whose
// backends point at the given fakes, in order.
func buildRouter(t *testing.T, mode config.Mode, fakes ...*testutil.Fake) *router.Router {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration(3 * time.Second),
			RerouteTimeout: config.Duration(200 * time.Millisecond),
			Cooldown:       config.Duration(400 * time.Millisecond),
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
	r, err := router.NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func routeOnce(t *testing.T, r *router.Router, stream bool) (*router.Result, error) {
	t.Helper()
	m, ok := r.Get("logical")
	if !ok {
		t.Fatal("model not found")
	}
	body := map[string]any{
		"model":    "logical",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if stream {
		body["stream"] = true
	}
	return m.Route(context.Background(), router.Request{Path: "/chat/completions", Body: body, Stream: stream})
}

func readBody(t *testing.T, res *router.Result) string {
	t.Helper()
	data, err := io.ReadAll(res.Resp.Body)
	_ = res.Resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

func TestFallback_ReroutesOnUpstreamError(t *testing.T) {
	f1, f2 := testutil.NewFake(t), testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{
		Status: 500,
		Body:   `{"error":{"message":"boom","type":"server_error","code":"boom"}}`,
	})
	f2.SetBehavior("model-1", testutil.Behavior{Body: testutil.ChatBody("model-1", "hello")})

	r := buildRouter(t, config.ModeFallback, f1, f2)
	res, err := routeOnce(t, r, false)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if res.Backend != "model-1@cred-1" {
		t.Errorf("backend = %q, want model-1@cred-1", res.Backend)
	}
	if len(res.Attempts) != 2 {
		t.Errorf("attempts = %d, want 2", len(res.Attempts))
	}
	if res.Attempts[0].Err == nil {
		t.Error("first attempt should have an error")
	}
	if len(res.Attempts[0].ErrBody) == 0 {
		t.Error("first attempt should carry the upstream error body")
	}
	body := readBody(t, res)
	if !strings.Contains(body, "hello") {
		t.Errorf("body = %q", body)
	}
	if f1.Hits("model-0") != 1 || f2.Hits("model-1") != 1 {
		t.Errorf("hits = %d, %d", f1.Hits("model-0"), f2.Hits("model-1"))
	}
}

func TestFallback_ReroutesOnSlowFirstByte(t *testing.T) {
	f1, f2 := testutil.NewFake(t), testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{
		Delay: 500 * time.Millisecond,
		Body:  testutil.ChatBody("model-0", "slow"),
	})
	f2.SetBehavior("model-1", testutil.Behavior{Body: testutil.ChatBody("model-1", "fast")})

	r := buildRouter(t, config.ModeFallback, f1, f2)
	start := time.Now()
	res, err := routeOnce(t, r, false)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if res.Backend != "model-1@cred-1" {
		t.Errorf("backend = %q, want model-1@cred-1", res.Backend)
	}
	if elapsed > 800*time.Millisecond {
		t.Errorf("route took %v, expected fast reroute at ~200ms", elapsed)
	}
	body := readBody(t, res)
	if !strings.Contains(body, "fast") {
		t.Errorf("body = %q", body)
	}
}

func TestFallback_ReroutesOnSlowStream(t *testing.T) {
	f1, f2 := testutil.NewFake(t), testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{
		Delay:  500 * time.Millisecond,
		Events: []string{testutil.ChatEvent("model-0", "slow")},
	})
	f2.SetBehavior("model-1", testutil.Behavior{Events: []string{testutil.ChatEvent("model-1", "fast")}})

	r := buildRouter(t, config.ModeFallback, f1, f2)
	res, err := routeOnce(t, r, true)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if res.Backend != "model-1@cred-1" {
		t.Errorf("backend = %q, want model-1@cred-1", res.Backend)
	}
	if len(res.FirstChunk) == 0 {
		t.Error("FirstChunk should hold the gated first byte")
	}
	body := readBody(t, res)
	if !strings.Contains(body, "fast") || strings.Contains(body, "slow") {
		t.Errorf("body = %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream should end with [DONE]: %q", body)
	}
}

func TestFallback_AllFail(t *testing.T) {
	f1, f2 := testutil.NewFake(t), testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{Status: 500, Body: `{"error":{"message":"one"}}`})
	f2.SetBehavior("model-1", testutil.Behavior{Drop: true})

	r := buildRouter(t, config.ModeFallback, f1, f2)
	_, err := routeOnce(t, r, false)
	if err == nil {
		t.Fatal("expected error")
	}
	var all *router.ErrAllBackendsFailed
	if !errors.As(err, &all) {
		t.Fatalf("error type = %T", err)
	}
	if all.Model != "logical" || len(all.Attempts) != 2 {
		t.Errorf("all = %+v", all)
	}
	if !strings.Contains(string(all.LastErrBody), "one") {
		t.Errorf("LastErrBody = %q", all.LastErrBody)
	}
}

func TestFallback_ModelRewritten(t *testing.T) {
	f1 := testutil.NewFake(t)
	f1.SetBehavior("backend-model", testutil.Behavior{Body: testutil.ChatBody("backend-model", "ok")})

	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration(3 * time.Second),
			RerouteTimeout: config.Duration(200 * time.Millisecond),
		},
		Credentials: map[string]config.Credential{
			"c": {Type: "openai", APIKey: "test", BaseURL: f1.URL()},
		},
		Models: []config.LogicalModel{{
			Name:   "logical",
			Domain: "test",
			Mode:   config.ModeFallback,
			Backends: []config.Backend{
				{Credential: "c", Model: "backend-model"},
			},
		}},
	}
	r, err := router.NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := routeOnce(t, r, false)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	readBody(t, res)

	got := f1.LastModel("/chat/completions")
	if got != "backend-model" {
		t.Errorf("upstream model = %q, want backend-model (logical name must not leak)", got)
	}
}

func TestRoundRobin_Distributes(t *testing.T) {
	f1, f2 := testutil.NewFake(t), testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{Body: testutil.ChatBody("model-0", "a")})
	f2.SetBehavior("model-1", testutil.Behavior{Body: testutil.ChatBody("model-1", "b")})

	r := buildRouter(t, config.ModeRoundRobin, f1, f2)
	for i := 0; i < 4; i++ {
		res, err := routeOnce(t, r, false)
		if err != nil {
			t.Fatalf("Route %d: %v", i, err)
		}
		readBody(t, res)
	}
	if f1.Hits("model-0") != 2 || f2.Hits("model-1") != 2 {
		t.Errorf("hits = %d, %d, want 2, 2", f1.Hits("model-0"), f2.Hits("model-1"))
	}
}

func TestRoundRobin_SkipsCoolingBackend(t *testing.T) {
	f1, f2 := testutil.NewFake(t), testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{Status: 500, Body: `{"error":{"message":"boom"}}`})
	f2.SetBehavior("model-1", testutil.Behavior{Body: testutil.ChatBody("model-1", "ok")})

	r := buildRouter(t, config.ModeRoundRobin, f1, f2)

	// First request: round-robin may pick either backend. If it picks the
	// failing one, it reroutes to the healthy one; the failing backend then
	// enters cooldown.
	res, err := routeOnce(t, r, false)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	readBody(t, res)

	// Ensure backend 0 is cooling down (it may or may not have been hit yet).
	if f1.Hits("model-0") == 0 {
		// Force a failure so it enters cooldown.
		res, err = routeOnce(t, r, false)
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		readBody(t, res)
	}

	// Next requests must skip the cooling backend entirely.
	for i := 0; i < 4; i++ {
		res, err := routeOnce(t, r, false)
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		if res.Backend != "model-1@cred-1" {
			t.Fatalf("request %d served by %q, want model-1@cred-1 (cooling backend must be skipped)", i, res.Backend)
		}
		readBody(t, res)
	}
}

func TestRoundRobin_AllCoolingStillServes(t *testing.T) {
	f1 := testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{Status: 500, Body: `{"error":{"message":"boom"}}`})

	r := buildRouter(t, config.ModeRoundRobin, f1)
	// First request fails and puts the only backend in cooldown.
	if _, err := routeOnce(t, r, false); err == nil {
		t.Fatal("expected failure")
	}

	// Second request: the only backend is cooling down, but it is still the
	// last resort. Make it healthy now and verify the request is served.
	f1.SetBehavior("model-0", testutil.Behavior{Body: testutil.ChatBody("model-0", "recovered")})
	res, err := routeOnce(t, r, false)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	body := readBody(t, res)
	if !strings.Contains(body, "recovered") {
		t.Errorf("body = %q", body)
	}
}

func TestOverallTimeoutBoundsSlowStream(t *testing.T) {
	f1 := testutil.NewFake(t)
	// The first byte arrives quickly (passes the reroute gate), then the
	// stream stalls far beyond the overall timeout.
	f1.SetBehavior("model-0", testutil.Behavior{
		Events: []string{
			testutil.ChatEvent("model-0", "x"),
			testutil.ChatEvent("model-0", "y"),
		},
		EventDelay: 800 * time.Millisecond,
	})

	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration(500 * time.Millisecond),
			RerouteTimeout: config.Duration(200 * time.Millisecond),
		},
		Credentials: map[string]config.Credential{
			"c": {Type: "openai", APIKey: "test", BaseURL: f1.URL()},
		},
		Models: []config.LogicalModel{{
			Name:   "logical",
			Domain: "test",
			Mode:   config.ModeFallback,
			Backends: []config.Backend{
				{Credential: "c", Model: "model-0"},
			},
		}},
	}
	r, err := router.NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := r.Get("logical")

	res, err := m.Route(context.Background(), router.Request{
		Path:   "/chat/completions",
		Body:   map[string]any{"model": "logical", "stream": true},
		Stream: true,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	start := time.Now()
	data, err := io.ReadAll(res.Resp.Body)
	_ = res.Resp.Body.Close()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected the overall timeout to cut the stream, got full body %q", data)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("stream was cut after %v, want ~500ms", elapsed)
	}
}

func TestAzure_PathRewrite(t *testing.T) {
	f := testutil.NewFake(t)
	f.SetBehavior("gpt-4o-deploy", testutil.Behavior{Body: testutil.ChatBody("gpt-4o-deploy", "ok")})

	cfg := &config.Config{
		Defaults: config.Defaults{
			Timeout:        config.Duration(3 * time.Second),
			RerouteTimeout: config.Duration(200 * time.Millisecond),
		},
		Credentials: map[string]config.Credential{
			"az": {Type: "azure", Endpoint: f.URL(), APIKey: "az-key", APIVersion: "2024-10-21"},
		},
		Models: []config.LogicalModel{{
			Name:   "logical",
			Domain: "test",
			Mode:   config.ModeFallback,
			Backends: []config.Backend{
				{Credential: "az", Model: "gpt-4o-deploy"},
			},
		}},
	}
	r, err := router.NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := routeOnce(t, r, false)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	readBody(t, res)

	const azPath = "/openai/deployments/gpt-4o-deploy/chat/completions"
	urlStr := f.LastURL(azPath)
	if urlStr == "" {
		t.Fatal("no request recorded")
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != azPath {
		t.Errorf("path = %q, want %q", u.Path, azPath)
	}
	if v := u.Query().Get("api-version"); v != "2024-10-21" {
		t.Errorf("api-version = %q", v)
	}
	// Azure authenticates with the Api-Key header (not Authorization).
	if got := f.LastAuth(azPath); got != "" {
		t.Errorf("Authorization header should be absent for Azure, got %q", got)
	}
	if got := f.LastAPIKey(azPath); got != "az-key" {
		t.Errorf("Api-Key = %q, want az-key", got)
	}
	if got := f.LastModel(azPath); got != "gpt-4o-deploy" {
		t.Errorf("model = %q", got)
	}
}

func TestStreaming_FirstChunkPreserved(t *testing.T) {
	f := testutil.NewFake(t)
	f.SetBehavior("model-0", testutil.Behavior{Events: []string{testutil.ChatEvent("model-0", "hi")}})

	r := buildRouter(t, config.ModeFallback, f)
	res, err := routeOnce(t, r, true)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	body := readBody(t, res)
	// The body must be a complete SSE stream: it starts with "data: " and
	// contains the first chunk's payload exactly once.
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("body should start with 'data: ', got %q", body[:min(16, len(body))])
	}
	if c := strings.Count(body, "hi"); c != 1 {
		t.Errorf("payload 'hi' appears %d times, want 1 (no duplication of the gated byte): %q", c, body)
	}
}

func TestStatuses(t *testing.T) {
	f1 := testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{Status: 500, Body: `{"error":{"message":"boom"}}`})

	r := buildRouter(t, config.ModeRoundRobin, f1)
	m, _ := r.Get("logical")

	st := m.Statuses()
	if len(st) != 1 || st[0].Available != true {
		t.Fatalf("initial status = %+v", st)
	}
	if _, err := routeOnce(t, r, false); err == nil {
		t.Fatal("expected failure")
	}
	st = m.Statuses()
	if st[0].Available {
		t.Errorf("backend should be cooling down after failure: %+v", st)
	}
	if st[0].Fails == 0 {
		t.Errorf("fails = 0, want > 0")
	}
	if st[0].LastError == "" {
		t.Error("last_error should be set")
	}
}
