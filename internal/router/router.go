// Package router implements the routing engine: logical models backed by
// multiple upstream backends with round-robin and fallback strategies,
// health/cooldown tracking and reroute timeouts.
//
// Timeout model
//
//   - timeout (overall): the total request budget. Exceeding it means the
//     service is out of service. It bounds every attempt and the whole
//     streaming body.
//   - reroute_timeout: the time allowed until the first response byte
//     (headers + first body byte for streaming, full body for
//     non-streaming). Exceeding it — or any error — triggers a reroute to
//     the next backend. It is NOT the out-of-service timeout.
package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"llm-router/internal/config"
	"llm-router/internal/metrics"
	"llm-router/internal/provider"
)

// Default values applied when the configuration leaves them unset.
const (
	DefaultRerouteTimeout = 10 * time.Second
	DefaultTimeout        = 120 * time.Second
	DefaultCooldown       = 30 * time.Second
	maxErrBody            = 1 << 20 // cap error bodies we retain (1 MiB)
)

// ErrAllBackendsFailed is returned when every backend of a logical model
// failed. LastErrBody carries the upstream error payload of the final
// attempt so the API layer can pass it through to the client.
type ErrAllBackendsFailed struct {
	Model       string
	Attempts    []Attempt
	LastErrBody []byte
}

func (e *ErrAllBackendsFailed) Error() string {
	return fmt.Sprintf("model %q: all %d backends failed", e.Model, len(e.Attempts))
}

func (e *ErrAllBackendsFailed) Unwrap() error {
	errs := make([]error, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		if a.Err != nil {
			errs = append(errs, fmt.Errorf("backend %s: %w", a.Backend, a.Err))
		}
	}
	return errors.Join(errs...)
}

// ErrRerouteTimeout is wrapped by attempts that exceed the reroute deadline
// (time until the first response byte).
var ErrRerouteTimeout = errors.New("reroute timeout exceeded")

// Attempt records one backend try for observability and error reporting.
type Attempt struct {
	Backend  string
	Err      error
	ErrBody  []byte
	Duration time.Duration
}

// Result is the outcome of a successful route: the raw upstream response.
// The caller owns Result.Resp.Body and must close it. For streaming
// requests the first body byte was consumed to enforce the reroute gate and
// is already re-prepended to Resp.Body, so Resp.Body is a complete stream;
// FirstChunk simply records the gated bytes for observability.
type Result struct {
	Resp       *http.Response
	Backend    string
	Attempts   []Attempt
	FirstChunk []byte
}

// Request is a normalized upstream request.
type Request struct {
	// Path is the upstream path, e.g. "/chat/completions" or "/completions".
	Path string
	// Body is the client request body; the router rewrites "model" per backend.
	Body map[string]any
	// Stream reports whether the client asked for SSE streaming.
	Stream bool
}

// health tracks the cooldown state of a single backend.
type health struct {
	mu          sync.Mutex
	cooldownEnd time.Time
	lastErr     error
	fails       int64
	hits        int64
}

func (h *health) markFailed(err error, cooldown time.Duration, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastErr = err
	h.fails++
	if cooldown > 0 {
		h.cooldownEnd = now.Add(cooldown)
	}
}

func (h *health) markHealthy() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cooldownEnd = time.Time{}
	h.lastErr = nil
}

// markServed records a successful request handled by this backend.
func (h *health) markServed() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hits++
}

// inCooldown reports whether the backend is still cooling down at now.
func (h *health) inCooldown(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return now.Before(h.cooldownEnd)
}

// Backend is one upstream target (credential + model/deployment).
type Backend struct {
	Name   string
	Model  string
	Client *provider.Client
	health *health
}

// Status is a point-in-time view of a backend, for the /healthz endpoint.
type Status struct {
	Name      string `json:"name"`
	Model     string `json:"model"`
	Kind      string `json:"kind"`
	Available bool   `json:"available"`
	LastError string `json:"last_error,omitempty"`
	Fails     int64  `json:"fails"`
	Hits      int64  `json:"hits"`
}

// Model is a logical model with its routing policy and backends.
type Model struct {
	cfg            config.LogicalModel
	backends       []*Backend
	rerouteTimeout time.Duration
	timeout        time.Duration
	cooldown       time.Duration
	rrIndex        atomic.Uint64
	log            *slog.Logger
	metrics        *metrics.Metrics
}

// NewModel builds a routable logical model. metrics may be nil to disable
// metrics collection.
func NewModel(cfg config.LogicalModel, defaults config.Defaults, creds map[string]config.Credential, mts *metrics.Metrics) (*Model, error) {
	m := &Model{
		cfg:            cfg,
		rerouteTimeout: defaults.RerouteTimeout.AsDuration(),
		timeout:        defaults.Timeout.AsDuration(),
		cooldown:       defaults.Cooldown.AsDuration(),
	}
	if cfg.Timeout != nil {
		m.timeout = cfg.Timeout.AsDuration()
	}
	if cfg.RerouteTimeout != nil {
		m.rerouteTimeout = cfg.RerouteTimeout.AsDuration()
	}
	if cfg.Cooldown != nil {
		m.cooldown = cfg.Cooldown.AsDuration()
	}
	if m.rerouteTimeout <= 0 {
		m.rerouteTimeout = DefaultRerouteTimeout
	}
	if m.timeout <= 0 {
		m.timeout = DefaultTimeout
	}
	if m.cooldown <= 0 {
		m.cooldown = DefaultCooldown
	}
	m.log = slog.Default()
	m.metrics = mts

	for i, b := range cfg.Backends {
		cred, ok := creds[b.Credential]
		if !ok {
			return nil, fmt.Errorf("model %q: backend[%d] references unknown credential %q", cfg.Name, i, b.Credential)
		}
		client, err := provider.New(cred)
		if err != nil {
			return nil, fmt.Errorf("model %q: backend[%d]: %w", cfg.Name, i, err)
		}
		backend := &Backend{
			Name:   fmt.Sprintf("%s@%s", b.Model, b.Credential),
			Model:  b.Model,
			Client: client,
			health: &health{},
		}
		m.backends = append(m.backends, backend)
		m.metrics.SetBackendUp(cfg.Name, backend.Name, true)
	}
	return m, nil
}

// Name returns the logical model name.
func (m *Model) Name() string { return m.cfg.Name }

// Domain returns the domain/app grouping.
func (m *Model) Domain() string { return m.cfg.Domain }

// Mode returns the routing mode.
func (m *Model) Mode() config.Mode { return m.cfg.Mode }

// Backends returns the backend names in order.
func (m *Model) Backends() []string {
	names := make([]string, len(m.backends))
	for i, b := range m.backends {
		names[i] = b.Name
	}
	return names
}

// Statuses returns a snapshot of backend health.
func (m *Model) Statuses() []Status {
	now := time.Now()
	out := make([]Status, 0, len(m.backends))
	for _, b := range m.backends {
		b.health.mu.Lock()
		st := Status{
			Name:      b.Name,
			Model:     b.Model,
			Kind:      b.Client.Kind(),
			Available: !now.Before(b.health.cooldownEnd),
			Fails:     b.health.fails,
			Hits:      b.health.hits,
		}
		if b.health.lastErr != nil {
			st.LastError = b.health.lastErr.Error()
		}
		b.health.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// Route executes the request against the model's backends according to the
// configured mode. On success the raw upstream response is returned; the
// caller owns Result.Resp.Body and must close it.
//
// The overall timeout bounds the whole request. On success the response body
// is tied to that context (so a runaway stream is cut off at the deadline),
// and the deadline timer is released when the body is closed.
func (m *Model) Route(ctx context.Context, req Request) (*Result, error) {
	start := time.Now()
	m.metrics.InflightInc()
	defer m.metrics.InflightDec()

	// Overall budget: the out-of-service timeout.
	ctx, cancel := context.WithTimeout(ctx, m.timeout)

	res, err := m.route(ctx, req)
	if err != nil {
		cancel()
		m.metrics.ObserveRequest(m.cfg.Name, string(m.cfg.Mode), req.Stream, "error", time.Since(start))
		return nil, err
	}
	m.metrics.ObserveRequest(m.cfg.Name, string(m.cfg.Mode), req.Stream, "success", time.Since(start))
	// On success do NOT cancel: the body reads depend on this context.
	// Wrap the body so closing it releases the deadline timer, while the
	// deadline itself still cuts off a stream that outlives the budget.
	res.Resp.Body = &cancelOnClose{ReadCloser: res.Resp.Body, cancel: cancel}
	return res, nil
}

func (m *Model) route(ctx context.Context, req Request) (*Result, error) {
	switch m.cfg.Mode {
	case config.ModeRoundRobin:
		return m.routeRoundRobin(ctx, req)
	default:
		return m.routeFallback(ctx, req)
	}
}

// setBackendUp syncs the metrics availability gauge with the backend's
// cooldown state.
func (m *Model) setBackendUp(b *Backend) {
	m.metrics.SetBackendUp(m.cfg.Name, b.Name, !b.health.inCooldown(time.Now()))
}

// rerouteReason classifies a failed attempt for the reroute metric.
func rerouteReason(err error) string {
	if errors.Is(err, ErrRerouteTimeout) {
		return "timeout"
	}
	return "error"
}

// cancelOnClose releases a context when the wrapped body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// routeFallback tries backends in order. Each attempt gets a reroute
// deadline (first response byte); on error or reroute timeout the next
// backend is tried. The overall ctx bounds the whole sequence.
func (m *Model) routeFallback(ctx context.Context, req Request) (*Result, error) {
	var attempts []Attempt
	var lastErrBody []byte
	for i, b := range m.backends {
		if ctx.Err() != nil {
			break
		}
		resp, first, err, errBody, dur := m.attempt(ctx, b, req)
		attempts = append(attempts, Attempt{Backend: b.Name, Err: err, ErrBody: errBody, Duration: dur})
		if errBody != nil {
			lastErrBody = errBody
		}
		if err == nil {
			b.health.markHealthy()
			b.health.markServed()
			m.setBackendUp(b)
			return &Result{Resp: resp, FirstChunk: first, Backend: b.Name, Attempts: attempts}, nil
		}
		m.log.Warn("backend attempt failed",
			"model", m.cfg.Name, "backend", b.Name, "stream", req.Stream, "error", err)
		closeQuietly(resp)
		b.health.markFailed(err, 0, time.Now()) // record; fallback always retries in order
		m.setBackendUp(b)
		if i < len(m.backends)-1 {
			m.metrics.ObserveReroute(m.cfg.Name, rerouteReason(err))
		}
	}
	return nil, &ErrAllBackendsFailed{Model: m.cfg.Name, Attempts: attempts, LastErrBody: lastErrBody}
}

// routeRoundRobin picks the next healthy backend in round-robin order and
// skips backends that are in cooldown (i.e. recently timed out or failed).
// If the picked backend fails, the request is rerouted to the next healthy
// backend (bounded by the overall timeout); the failing backend enters
// cooldown so subsequent requests skip it. Cooling-down backends are only
// used as a last resort when every backend is cooling down.
func (m *Model) routeRoundRobin(ctx context.Context, req Request) (*Result, error) {
	n := len(m.backends)
	start := int(m.rrIndex.Add(1)-1) % n

	// Candidate order: healthy backends in round-robin order first, then
	// cooling-down backends (best effort).
	healthy := make([]int, 0, n)
	cooling := make([]int, 0, n)
	for k := 0; k < n; k++ {
		idx := (start + k) % n
		if m.backends[idx].health.inCooldown(time.Now()) {
			cooling = append(cooling, idx)
		} else {
			healthy = append(healthy, idx)
		}
	}
	candidates := append(healthy, cooling...)

	var attempts []Attempt
	var lastErrBody []byte
	for i, idx := range candidates {
		b := m.backends[idx]
		if ctx.Err() != nil {
			break
		}
		resp, first, err, errBody, dur := m.attempt(ctx, b, req)
		attempts = append(attempts, Attempt{Backend: b.Name, Err: err, ErrBody: errBody, Duration: dur})
		if errBody != nil {
			lastErrBody = errBody
		}
		if err == nil {
			b.health.markHealthy()
			b.health.markServed()
			m.setBackendUp(b)
			return &Result{Resp: resp, FirstChunk: first, Backend: b.Name, Attempts: attempts}, nil
		}
		m.log.Warn("backend attempt failed",
			"model", m.cfg.Name, "backend", b.Name, "stream", req.Stream, "error", err)
		closeQuietly(resp)
		b.health.markFailed(err, m.cooldown, time.Now())
		m.setBackendUp(b)
		if i < len(candidates)-1 {
			m.metrics.ObserveReroute(m.cfg.Name, rerouteReason(err))
		}
	}
	return nil, &ErrAllBackendsFailed{Model: m.cfg.Name, Attempts: attempts, LastErrBody: lastErrBody}
}

// attempt calls one backend and enforces the reroute deadline.
//
// The attempt runs in a goroutine under a cancellable child of ctx so the
// reroute timer can abort it. For non-streaming requests the whole response
// (headers + body) must complete within the reroute deadline. For streaming
// requests only the headers + first body byte are gated; once the first
// byte arrives the body keeps flowing under the overall ctx.
//
// Returns (resp, firstChunk, err, errBody). On error, resp may be non-nil
// (its body is left open; the caller closes it) and errBody holds the
// upstream error payload when available.
func (m *Model) attempt(ctx context.Context, b *Backend, req Request) (resp *http.Response, first []byte, err error, errBody []byte, dur time.Duration) {
	start := time.Now()
	body := make(map[string]any, len(req.Body)+1)
	for k, v := range req.Body {
		body[k] = v
	}
	body["model"] = b.Model

	// attemptCtx is canceled explicitly on failure paths only. On success it
	// must stay alive: the response body (streaming) is tied to it and keeps
	// flowing under the overall context until it is closed or the overall
	// deadline fires.
	attemptCtx, cancel := context.WithCancel(ctx)

	type outcome struct {
		resp    *http.Response
		first   []byte
		err     error
		errBody []byte
	}
	ch := make(chan outcome, 1)

	go func() {
		o := outcome{}
		r, e := b.Client.Do(attemptCtx, req.Path, body)
		if e != nil {
			o.err = e
			if r != nil {
				o.errBody = readErrBody(r)
				o.resp = r // body left open for the caller to close
			}
			ch <- o
			return
		}
		if !req.Stream {
			// Non-streaming: the full body must arrive within the gate.
			data, rerr := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if rerr != nil {
				o.err = rerr
				ch <- o
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			o.resp = r
			ch <- o
			return
		}
		// Streaming: gate the first body byte. Any data counts as success;
		// the stream then flows under the overall ctx.
		buf := make([]byte, 1)
		n, rerr := r.Body.Read(buf)
		if n > 0 {
			r.Body = &prependReader{first: buf[:n], rest: r.Body}
			o.resp = r
			o.first = buf[:n]
			ch <- o
			return
		}
		_ = r.Body.Close()
		if rerr == nil {
			rerr = fmt.Errorf("backend %s returned an empty streaming body", b.Name)
		}
		o.err = rerr
		ch <- o
	}()

	timer := time.NewTimer(m.rerouteTimeout)
	defer timer.Stop()

	select {
	case o := <-ch:
		dur = time.Since(start)
		if o.err != nil {
			// Abort the in-flight request.
			cancel()
			m.metrics.ObserveAttempt(m.cfg.Name, b.Name, "failure", dur)
			return o.resp, o.first, o.err, o.errBody, dur
		}
		m.metrics.ObserveAttempt(m.cfg.Name, b.Name, "success", dur)
		// Success: the response body is tied to attemptCtx, so cancel must
		// not fire now. Transfer ownership of cancel to the body: closing
		// it releases the attempt context (the overall deadline still
		// bounds a runaway stream).
		if o.resp != nil && o.resp.Body != nil {
			o.resp.Body = &cancelOnClose{ReadCloser: o.resp.Body, cancel: cancel}
		} else {
			// No body to tie the context to; release it now.
			cancel()
		}
		return o.resp, o.first, nil, nil, dur
	case <-timer.C:
		cancel() // abort the in-flight attempt
		// Drain so the goroutine can finish and release the connection.
		go func() {
			if o := <-ch; o.resp != nil && o.resp.Body != nil {
				_ = o.resp.Body.Close()
			}
		}()
		dur = time.Since(start)
		m.metrics.ObserveAttempt(m.cfg.Name, b.Name, "timeout", dur)
		return nil, nil, fmt.Errorf("backend %s exceeded reroute timeout %s: %w", b.Name, m.rerouteTimeout, ErrRerouteTimeout), nil, dur
	}
}

// readErrBody captures the upstream error payload (bounded).
func readErrBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	_ = resp.Body.Close()
	if err != nil {
		return nil
	}
	return data
}

// prependReader serves the buffered first bytes, then delegates.
type prependReader struct {
	first []byte
	rest  io.ReadCloser
}

func (p *prependReader) Read(b []byte) (int, error) {
	if len(p.first) > 0 {
		n := copy(b, p.first)
		p.first = p.first[n:]
		return n, nil
	}
	return p.rest.Read(b)
}

func (p *prependReader) Close() error { return p.rest.Close() }

func closeQuietly(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrBody))
	_ = resp.Body.Close()
}

// Router is the registry of logical models.
type Router struct {
	mu       sync.RWMutex
	models   map[string]*Model
	byDomain map[string][]string
}

// Option configures the router.
type Option func(*routerOptions)

type routerOptions struct {
	metrics *metrics.Metrics
}

// WithMetrics enables Prometheus metrics collection on the given registry.
// Omit the option (or pass nil) to disable metrics.
func WithMetrics(m *metrics.Metrics) Option {
	return func(o *routerOptions) { o.metrics = m }
}

// NewRouter builds a Router from the validated configuration.
func NewRouter(cfg *config.Config, opts ...Option) (*Router, error) {
	o := routerOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	r := &Router{
		models:   make(map[string]*Model, len(cfg.Models)),
		byDomain: make(map[string][]string),
	}
	for i := range cfg.Models {
		m, err := NewModel(cfg.Models[i], cfg.Defaults, cfg.Credentials, o.metrics)
		if err != nil {
			return nil, err
		}
		r.models[m.Name()] = m
		r.byDomain[m.Domain()] = append(r.byDomain[m.Domain()], m.Name())
	}
	return r, nil
}

// Get returns the logical model with the given name.
func (r *Router) Get(name string) (*Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[name]
	return m, ok
}

// Names lists logical model names.
func (r *Router) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.models))
	for name := range r.models {
		out = append(out, name)
	}
	return out
}

// Domains lists domain/app groups with their models.
func (r *Router) Domains() map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string][]string, len(r.byDomain))
	for d, names := range r.byDomain {
		cp := make([]string, len(names))
		copy(cp, names)
		out[d] = cp
	}
	return out
}
