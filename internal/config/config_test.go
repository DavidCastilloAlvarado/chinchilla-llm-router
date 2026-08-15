package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadValidConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("AZURE_OPENAI_ENDPOINT", "https://res.cognitiveservices.azure.com")
	t.Setenv("AZURE_OPENAI_API_KEY", "az-key")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  host: 127.0.0.1
  port: 9090
  api_key: ${ROUTER_KEY}
defaults:
  timeout: 60s
  reroute_timeout: 5s
  cooldown: 15s
credentials:
  openai-main:
    type: openai
    api_key: ${OPENAI_API_KEY}
  azure-foundry:
    type: azure
    endpoint: ${AZURE_OPENAI_ENDPOINT}
    api_key: ${AZURE_OPENAI_API_KEY}
    api_version: "2024-10-21"
models:
  - name: chat-fast
    domain: chat
    mode: fallback
    backends:
      - credential: openai-main
        model: gpt-4o-mini
      - credential: azure-foundry
        model: gpt-4o-mini-deployment
  - name: embed
    domain: embeddings
    mode: round_robin
    timeout: 30s
    backends:
      - credential: openai-main
        model: text-embedding-3-small
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 9090 {
		t.Errorf("server = %+v", cfg.Server)
	}
	if cfg.Server.Addr() != "127.0.0.1:9090" {
		t.Errorf("Addr() = %q", cfg.Server.Addr())
	}
	if cfg.Defaults.Timeout.AsDuration() != 60*time.Second {
		t.Errorf("timeout = %v", cfg.Defaults.Timeout.AsDuration())
	}
	if cfg.Defaults.RerouteTimeout.AsDuration() != 5*time.Second {
		t.Errorf("reroute_timeout = %v", cfg.Defaults.RerouteTimeout.AsDuration())
	}
	if got := cfg.Credentials["openai-main"].APIKey; got != "sk-test" {
		t.Errorf("env expansion failed: %q", got)
	}
	if len(cfg.Models) != 2 {
		t.Fatalf("models = %d", len(cfg.Models))
	}
	if cfg.Models[0].Mode != ModeFallback {
		t.Errorf("mode = %q", cfg.Models[0].Mode)
	}
	if cfg.Models[1].Mode != ModeRoundRobin {
		t.Errorf("mode = %q", cfg.Models[1].Mode)
	}
	if cfg.Models[1].Timeout == nil || cfg.Models[1].Timeout.AsDuration() != 30*time.Second {
		t.Errorf("per-model timeout = %v", cfg.Models[1].Timeout)
	}
}

func TestLoadDefaultsAndModeDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
credentials:
  c:
    type: openai
    api_key: k
models:
  - name: m
    domain: d
    backends:
      - credential: c
        model: gpt-4o
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Models[0].Mode != ModeFallback {
		t.Errorf("default mode = %q, want fallback", cfg.Models[0].Mode)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := map[string]string{
		"no credentials": `
models:
  - name: m
    domain: d
    backends:
      - credential: c
        model: x
`,
		"no models": `
credentials:
  c:
    type: openai
    api_key: k
`,
		"missing api key": `
credentials:
  c:
    type: openai
models:
  - name: m
    domain: d
    backends:
      - credential: c
        model: x
`,
		"azure missing endpoint": `
credentials:
  c:
    type: azure
    api_key: k
models:
  - name: m
    domain: d
    backends:
      - credential: c
        model: x
`,
		"azure endpoint not a URL": `
credentials:
  c:
    type: azure
    api_key: k
    endpoint: not-a-url
models:
  - name: m
    domain: d
    backends:
      - credential: c
        model: x
`,
		"unknown credential type": `
credentials:
  c:
    type: google
    api_key: k
models:
  - name: m
    domain: d
    backends:
      - credential: c
        model: x
`,
		"duplicate model": `
credentials:
  c:
    type: openai
    api_key: k
models:
  - name: m
    domain: d
    backends:
      - credential: c
        model: x
  - name: m
    domain: d
    backends:
      - credential: c
        model: y
`,
		"missing domain": `
credentials:
  c:
    type: openai
    api_key: k
models:
  - name: m
    backends:
      - credential: c
        model: x
`,
		"unknown mode": `
credentials:
  c:
    type: openai
    api_key: k
models:
  - name: m
    domain: d
    mode: chaos
    backends:
      - credential: c
        model: x
`,
		"unknown credential ref": `
credentials:
  c:
    type: openai
    api_key: k
models:
  - name: m
    domain: d
    backends:
      - credential: missing
        model: x
`,
		"no backends": `
credentials:
  c:
    type: openai
    api_key: k
models:
  - name: m
    domain: d
`,
		"bad duration": `
defaults:
  timeout: never
credentials:
  c:
    type: openai
    api_key: k
models:
  - name: m
    domain: d
    backends:
      - credential: c
        model: x
`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestServerAddrDefaults(t *testing.T) {
	var s Server
	if got := s.Addr(); got != "0.0.0.0:8080" {
		t.Errorf("Addr() = %q", got)
	}
	s.Host = "127.0.0.1"
	s.Port = 9999
	if got := s.Addr(); got != "127.0.0.1:9999" {
		t.Errorf("Addr() = %q", got)
	}
}
