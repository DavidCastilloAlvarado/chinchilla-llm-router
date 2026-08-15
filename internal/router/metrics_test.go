package router_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"llm-router/internal/config"
	"llm-router/internal/metrics"
	"llm-router/internal/router"
	"llm-router/internal/testutil"
)

// buildMetricsRouter is like buildRouter but wires in a metrics registry.
func buildMetricsRouter(t *testing.T, mode config.Mode, mts *metrics.Metrics, fakes ...*testutil.Fake) *router.Router {
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
		cred := "cred-" + itoa(i)
		cfg.Credentials[cred] = config.Credential{Type: "openai", APIKey: "test", BaseURL: f.URL()}
		cfg.Models[0].Backends = append(cfg.Models[0].Backends,
			config.Backend{Credential: cred, Model: "model-" + itoa(i)})
	}
	r, err := router.NewRouter(cfg, router.WithMetrics(mts))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func itoa(i int) string {
	return string(rune('0' + i))
}

// metricVal reads a single-value counter or gauge via the testutil helper.
func metricVal(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	return promtestutil.ToFloat64(c)
}

// TestMetrics_RerouteCountsAndBackendUp verifies that a failed first backend
// in round-robin mode is counted (attempt failure, reroute, backend down)
// and the successful request is counted.
func TestMetrics_RerouteCountsAndBackendUp(t *testing.T) {
	f1, f2 := testutil.NewFake(t), testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{
		Status: 500,
		Body:   `{"error":{"message":"boom","type":"server_error","code":"boom"}}`,
	})
	f2.SetBehavior("model-1", testutil.Behavior{Body: testutil.ChatBody("model-1", "hello")})

	mts := metrics.New()
	r := buildMetricsRouter(t, config.ModeRoundRobin, mts, f1, f2)
	res, err := routeOnce(t, r, false)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	readBody(t, res)

	check := func(got, want float64, what string) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	check(metricVal(t, mts.RequestsTotal.WithLabelValues("logical", "round_robin", "false", "success")), 1, "requests_total{success}")
	check(metricVal(t, mts.RequestsTotal.WithLabelValues("logical", "round_robin", "false", "error")), 0, "requests_total{error}")
	check(metricVal(t, mts.AttemptsTotal.WithLabelValues("logical", "model-0@cred-0", "failure")), 1, "attempts_total{model-0,failure}")
	check(metricVal(t, mts.AttemptsTotal.WithLabelValues("logical", "model-1@cred-1", "success")), 1, "attempts_total{model-1,success}")
	check(metricVal(t, mts.ReroutesTotal.WithLabelValues("logical", "error")), 1, "reroutes_total{error}")
	check(metricVal(t, mts.BackendUp.WithLabelValues("logical", "model-0@cred-0")), 0, "backend_up{model-0}")
	check(metricVal(t, mts.BackendUp.WithLabelValues("logical", "model-1@cred-1")), 1, "backend_up{model-1}")
	check(metricVal(t, mts.Inflight), 0, "inflight after request")

	// The duration histogram must have observed at least one sample.
	mfs, err := mts.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() != "llm_router_request_duration_seconds" {
			continue
		}
		found = true
		if len(mf.GetMetric()) != 1 {
			t.Fatalf("duration histogram series = %d, want 1", len(mf.GetMetric()))
		}
		if n := mf.GetMetric()[0].GetHistogram().GetSampleCount(); n < 1 {
			t.Errorf("duration histogram samples = %d, want >= 1", n)
		}
	}
	if !found {
		t.Error("llm_router_request_duration_seconds not exposed")
	}
}

// TestMetrics_RerouteTimeoutReason verifies that a reroute caused by the
// reroute timeout is counted with reason="timeout".
func TestMetrics_RerouteTimeoutReason(t *testing.T) {
	f1, f2 := testutil.NewFake(t), testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{
		Delay: 300 * time.Millisecond, // exceeds the 200ms reroute timeout
		Body:  testutil.ChatBody("model-0", "slow"),
	})
	f2.SetBehavior("model-1", testutil.Behavior{Body: testutil.ChatBody("model-1", "fast")})

	mts := metrics.New()
	r := buildMetricsRouter(t, config.ModeFallback, mts, f1, f2)
	res, err := routeOnce(t, r, false)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	readBody(t, res)

	if v := metricVal(t, mts.ReroutesTotal.WithLabelValues("logical", "timeout")); v != 1 {
		t.Errorf("reroutes_total{timeout} = %v, want 1", v)
	}
	if v := metricVal(t, mts.ReroutesTotal.WithLabelValues("logical", "error")); v != 0 {
		t.Errorf("reroutes_total{error} = %v, want 0", v)
	}
	if v := metricVal(t, mts.RequestsTotal.WithLabelValues("logical", "fallback", "false", "success")); v != 1 {
		t.Errorf("requests_total{success} = %v, want 1", v)
	}
}

// TestMetrics_AllBackendsFail verifies that a fully failed request is
// counted as an error.
func TestMetrics_AllBackendsFail(t *testing.T) {
	f1 := testutil.NewFake(t)
	f1.SetBehavior("model-0", testutil.Behavior{
		Status: 500,
		Body:   `{"error":{"message":"boom","type":"server_error","code":"boom"}}`,
	})

	mts := metrics.New()
	r := buildMetricsRouter(t, config.ModeFallback, mts, f1)
	if _, err := routeOnce(t, r, false); err == nil {
		t.Fatal("expected route failure")
	}

	if v := metricVal(t, mts.RequestsTotal.WithLabelValues("logical", "fallback", "false", "error")); v != 1 {
		t.Errorf("requests_total{error} = %v, want 1", v)
	}
	if v := metricVal(t, mts.AttemptsTotal.WithLabelValues("logical", "model-0@cred-0", "failure")); v != 1 {
		t.Errorf("attempts_total{failure} = %v, want 1", v)
	}
}
