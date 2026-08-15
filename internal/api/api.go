// Package api exposes the OpenAI v1-compatible HTTP surface of the router:
//
//	POST /v1/chat/completions
//	POST /v1/completions
//	GET  /v1/models
//	GET  /healthz
//	GET  /metrics (when metrics are enabled)
//
// Responses are passed through from the upstream with the "model" field
// rewritten to the logical model name, so clients see a stable identity
// regardless of which backend served the request.
package api

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"llm-router/internal/metrics"
	"llm-router/internal/router"
)

const maxRequestBody = 10 << 20 // 10 MiB

// Server is the HTTP frontend of the router.
type Server struct {
	router  *router.Router
	apiKey  string
	log     *slog.Logger
	metrics *metrics.Metrics
}

// Option configures the API server.
type Option func(*Server)

// WithMetrics registers a GET /metrics endpoint backed by the given
// registry. Omit the option to disable metrics.
func WithMetrics(m *metrics.Metrics) Option {
	return func(s *Server) { s.metrics = m }
}

// New builds an API server. apiKey may be empty to disable auth.
func New(r *router.Router, apiKey string, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{router: r, apiKey: apiKey, log: log}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Handler returns the root http.Handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		s.handleCompletions(w, r, "/chat/completions")
	})
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) {
		s.handleCompletions(w, r, "/completions")
	})
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /", s.handleIndex)
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics.Handler())
	}
	return mux
}

func (s *Server) authorized(r *http.Request) bool {
	if s.apiKey == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	key := strings.TrimPrefix(auth, prefix)
	return subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) == 1
}

func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	start := time.Now()

	if !s.authorized(r) {
		s.writeOpenAIError(w, http.StatusUnauthorized, "invalid API key", "invalid_api_key")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		s.log.Warn("failed to read request body", "error", err)
		s.writeOpenAIError(w, http.StatusBadRequest, "could not read request body", "invalid_request_error")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.writeOpenAIError(w, http.StatusBadRequest, "request body must be a JSON object: "+err.Error(), "invalid_request_error")
		return
	}

	modelName, _ := payload["model"].(string)
	if modelName == "" {
		s.writeOpenAIError(w, http.StatusBadRequest, "'model' is required", "invalid_request_error")
		return
	}

	m, ok := s.router.Get(modelName)
	if !ok {
		s.writeOpenAIError(w, http.StatusNotFound,
			fmt.Sprintf("model %q is not configured on this router", modelName), "model_not_found")
		return
	}

	stream, _ := payload["stream"].(bool)

	result, err := m.Route(r.Context(), router.Request{Path: upstreamPath, Body: payload, Stream: stream})
	if err != nil {
		if errors.Is(r.Context().Err(), context.Canceled) {
			return // client went away; nothing to write
		}
		s.log.Warn("route failed",
			"model", modelName, "stream", stream, "error", err, "duration", time.Since(start))
		s.writeRouteError(w, err)
		return
	}
	defer result.Resp.Body.Close()

	s.log.Info("routed",
		"model", modelName, "backend", result.Backend, "stream", stream,
		"attempts", len(result.Attempts), "duration", time.Since(start))

	if stream {
		s.writeSSE(w, result, modelName)
		return
	}
	s.writeJSON(w, result, modelName)
}

// writeJSON passes through a non-streaming response, rewriting "model".
func (s *Server) writeJSON(w http.ResponseWriter, result *router.Result, logicalModel string) {
	data, err := io.ReadAll(result.Resp.Body)
	if err != nil {
		s.log.Error("failed to read upstream response body", "model", logicalModel, "backend", result.Backend, "error", err)
		s.writeOpenAIError(w, http.StatusBadGateway, "upstream response read failed: "+err.Error(), "server_error")
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		// Not JSON (unexpected): pass through unchanged.
		w.Header().Set("Content-Type", "application/json")
		if _, werr := w.Write(data); werr != nil && !errors.Is(werr, http.ErrAbortHandler) {
			s.log.Warn("failed to write upstream response to client", "model", logicalModel, "error", werr)
		}
		return
	}
	obj["model"] = logicalModel
	out, err := json.Marshal(obj)
	if err != nil {
		s.log.Error("failed to re-encode upstream response", "model", logicalModel, "error", err)
		s.writeOpenAIError(w, http.StatusBadGateway, "upstream response re-encode failed", "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, werr := w.Write(out); werr != nil && !errors.Is(werr, http.ErrAbortHandler) {
		s.log.Warn("failed to write response to client", "model", logicalModel, "error", werr)
	}
}

// writeSSE streams the upstream SSE body to the client, rewriting the
// "model" field of every event.
func (s *Server) writeSSE(w http.ResponseWriter, result *router.Result, logicalModel string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeOpenAIError(w, http.StatusInternalServerError, "streaming unsupported", "server_error")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	scanner := bufio.NewScanner(result.Resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if rewritten, ok := rewriteSSEData(line, logicalModel); ok {
			line = rewritten
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			// Client disconnected mid-stream. Log it so the truncation is
			// visible, then stop writing.
			s.log.Warn("failed to write stream to client", "model", logicalModel, "backend", result.Backend, "error", err)
			return
		}
		// Flush at event boundaries (blank line) and after data lines.
		if line == "" || strings.HasPrefix(line, "data:") {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		// The upstream stream broke mid-flight (or the overall deadline
		// fired). Tell the client so the truncation is visible.
		s.log.Warn("upstream stream interrupted", "model", logicalModel, "backend", result.Backend, "error", err)
		if _, werr := fmt.Fprintf(w, "data: {\"error\":{\"message\":\"stream interrupted: %s\",\"type\":\"server_error\"}}\n\n", sanitizeMsg(err)); werr != nil && !errors.Is(werr, http.ErrAbortHandler) {
			s.log.Warn("failed to write stream-interruption notice to client", "model", logicalModel, "error", werr)
		}
		flusher.Flush()
	}
}

// rewriteSSEData rewrites the "model" field of an SSE data line. It returns
// (line, false) when the line is not a JSON event (comments, [DONE], ...).
func rewriteSSEData(line, logicalModel string) (string, bool) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(line), "data:")
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" || trimmed == "[DONE]" {
		return line, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return line, false
	}
	obj["model"] = logicalModel
	out, err := json.Marshal(obj)
	if err != nil {
		return line, false
	}
	return "data: " + string(out), true
}

// sanitizeMsg keeps a single line of error text for embedding in JSON.
func sanitizeMsg(err error) string {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	msg = strings.ReplaceAll(msg, "\"", "'")
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return msg
}

// writeRouteError converts a routing failure into an OpenAI-shaped error.
// When the upstream produced an error payload it is passed through so the
// client sees the original message.
func (s *Server) writeRouteError(w http.ResponseWriter, err error) {
	var all *router.ErrAllBackendsFailed
	if errors.As(err, &all) {
		if len(all.LastErrBody) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			if _, werr := w.Write(all.LastErrBody); werr != nil && !errors.Is(werr, http.ErrAbortHandler) {
				s.log.Warn("failed to write upstream error body to client", "model", all.Model, "error", werr)
			}
			return
		}
		s.writeOpenAIError(w, http.StatusServiceUnavailable, all.Error(), "server_error")
		return
	}
	s.writeOpenAIError(w, http.StatusBadGateway, err.Error(), "server_error")
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.writeOpenAIError(w, http.StatusUnauthorized, "invalid API key", "invalid_api_key")
		return
	}
	names := s.router.Names()
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]any{
			"id":       name,
			"object":   "model",
			"created":  0,
			"owned_by": "llm-router",
		})
	}
	s.writeJSONBody(w, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"status": "ok", "models": map[string]any{}}
	for _, name := range s.router.Names() {
		m, _ := s.router.Get(name)
		out["models"].(map[string]any)[name] = map[string]any{
			"domain":   m.Domain(),
			"mode":     string(m.Mode()),
			"backends": m.Statuses(),
		}
	}
	s.writeJSONBody(w, out)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.writeOpenAIError(w, http.StatusNotFound, "not found", "invalid_request_error")
		return
	}
	endpoints := []string{
		"POST /v1/chat/completions",
		"POST /v1/completions",
		"GET  /v1/models",
		"GET  /healthz",
	}
	if s.metrics != nil {
		endpoints = append(endpoints, "GET  /metrics")
	}
	s.writeJSONBody(w, map[string]any{
		"service":   "llm-router",
		"domains":   s.router.Domains(),
		"endpoints": endpoints,
	})
}

// writeOpenAIError writes an OpenAI-shaped error response. Terminal write
// failures are logged so they are not hidden.
func (s *Server) writeOpenAIError(w http.ResponseWriter, status int, message, typ string) {
	body := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    typ,
			"param":   nil,
			"code":    nil,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil && !errors.Is(err, http.ErrAbortHandler) {
		s.log.Warn("failed to write error response to client", "status", status, "error", err)
	}
}

// writeJSONBody writes an arbitrary JSON body with HTTP 200. Terminal write
// failures are logged so they are not hidden.
func (s *Server) writeJSONBody(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, http.ErrAbortHandler) {
		s.log.Warn("failed to write JSON response to client", "error", err)
	}
}
