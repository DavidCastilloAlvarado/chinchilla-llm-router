// Command llm-router is an OpenAI v1-compatible LLM router.
//
// It exposes /v1/chat/completions, /v1/completions and /v1/models, routes
// requests to one or more upstream OpenAI / Azure OpenAI backends per
// logical model, and reroutes on immediate failures or when a backend does
// not start responding within the configured reroute timeout.
//
// Observability:
//   - structured JSON logs on stdout (never stderr, never prints)
//   - Prometheus metrics on GET /metrics
//   - every error is either reported to the client or logged; nothing is
//     silently swallowed
//
// Startup failures (config, router init, listen) are logged to stdout and
// the process exits with a non-zero status.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llm-router/internal/api"
	"llm-router/internal/config"
	"llm-router/internal/envfile"
	"llm-router/internal/metrics"
	"llm-router/internal/router"
)

// version is set at build time via -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	configPath := flag.String("config", os.Getenv("LLM_ROUTER_CONFIG"), "config.yaml")
	checkOnly := flag.Bool("check", false, "validate the config file and exit (no server started)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *configPath == "" {
		*configPath = "config.yaml"
	}
	if *showVersion {
		fmt.Println("llm-router " + version)
		return
	}

	// Structured JSON logs on stdout.
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// Load .env (if present) into the process environment before anything
	// reads env vars. .env values take priority over system environment
	// variables; when the file is absent the system environment is used
	// as-is. Config ${VAR} expansion then sees the merged view.
	if n, found, err := envfile.LoadDefault(); err != nil {
		log.Error("startup failed: cannot load .env", "path", envfile.DefaultPath, "error", err)
		os.Exit(1)
	} else if found {
		log.Info("loaded environment file", "path", envfile.DefaultPath, "vars", n)
	}

	// Prometheus metrics (dedicated registry, served at GET /metrics).
	m := metrics.New()

	cfg, err := config.Load(*configPath)
	if err != nil {
		// Critical startup failure: log to stdout and exit non-zero.
		log.Error("startup failed: cannot load config", "path", *configPath, "error", err)
		os.Exit(1)
	}
	if *checkOnly {
		log.Info("config ok", "path", *configPath, "models", len(cfg.Models), "credentials", len(cfg.Credentials))
		return
	}

	r, err := router.NewRouter(cfg, router.WithMetrics(m))
	if err != nil {
		log.Error("startup failed: cannot initialize router", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:         cfg.Server.Addr(),
		Handler:      api.New(r, cfg.Server.APIKey, log, api.WithMetrics(m)).Handler(),
		ReadTimeout:  cfg.Server.ReadTimeout.AsDuration(),
		WriteTimeout: cfg.Server.WriteTimeout.AsDuration(),
		IdleTimeout:  cfg.Server.IdleTimeout.AsDuration(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("llm-router listening",
			"addr", srv.Addr,
			"models", len(cfg.Models),
			"endpoints", []string{"/v1/chat/completions", "/v1/completions", "/v1/models", "/healthz", "/metrics"},
		)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down", "reason", "signal received")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			// Listen failure (e.g. port in use): critical startup failure.
			log.Error("startup failed: http server error", "addr", srv.Addr, "error", err)
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}
