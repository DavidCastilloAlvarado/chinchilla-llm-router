// Package config loads and validates the router configuration.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode is the routing strategy of a logical model.
type Mode string

const (
	// ModeRoundRobin distributes requests across healthy backends.
	ModeRoundRobin Mode = "round_robin"
	// ModeFallback tries backends in order, rerouting on error or reroute timeout.
	ModeFallback Mode = "fallback"
)

// Credential is a reusable upstream credential (OpenAI or Azure OpenAI).
type Credential struct {
	// Type is "openai" or "azure".
	Type string `yaml:"type"`
	// APIKey is the API key. Supports ${ENV_VAR} expansion.
	APIKey string `yaml:"api_key"`
	// BaseURL is the OpenAI-compatible base URL (default https://api.openai.com/v1).
	BaseURL string `yaml:"base_url"`
	// Endpoint is the Azure OpenAI resource endpoint, e.g.
	// https://<resource>.openai.azure.com (required when type=azure).
	Endpoint string `yaml:"endpoint"`
	// APIVersion is the Azure OpenAI api-version (default 2024-10-21).
	APIVersion string `yaml:"api_version"`
}

// Backend is one upstream model/deployment backing a logical model.
type Backend struct {
	// Credential references a key in the credentials map.
	Credential string `yaml:"credential"`
	// Model is the upstream model id (OpenAI) or deployment name (Azure).
	Model string `yaml:"model"`
	// APIVersion optionally overrides the credential's Azure api-version.
	APIVersion string `yaml:"api_version"`
}

// LogicalModel is a named model exposed by the router, grouped by domain/app.
type LogicalModel struct {
	// Name is the model id clients request (e.g. "chat-fast").
	Name string `yaml:"name"`
	// Domain groups logical models by domain/app (e.g. "web", "data-pipeline").
	Domain string `yaml:"domain"`
	// Mode selects the routing strategy: round_robin or fallback.
	Mode Mode `yaml:"mode"`
	// Timeout is the overall request budget; exceeding it means out-of-service.
	Timeout *Duration `yaml:"timeout"`
	// RerouteTimeout is the time allowed before the first response byte;
	// exceeding it triggers a reroute to the next backend (fallback mode).
	RerouteTimeout *Duration `yaml:"reroute_timeout"`
	// Cooldown is how long a backend is skipped after a failure (round_robin).
	Cooldown *Duration `yaml:"cooldown"`
	// Backends is the ordered list of upstream backends.
	Backends []Backend `yaml:"backends"`
}

// Server holds HTTP server settings.
type Server struct {
	Host         string   `yaml:"host"`
	Port         int      `yaml:"port"`
	ReadTimeout  Duration `yaml:"read_timeout"`
	WriteTimeout Duration `yaml:"write_timeout"`
	IdleTimeout  Duration `yaml:"idle_timeout"`
	// APIKey, when set, requires clients to send "Authorization: Bearer <APIKey>".
	APIKey string `yaml:"api_key"`
}

// Addr returns the listen address (host:port) with defaults applied.
func (s Server) Addr() string {
	host := s.Host
	if host == "" {
		host = "0.0.0.0"
	}
	port := s.Port
	if port == 0 {
		port = 8080
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// Defaults are applied to logical models that do not override them.
type Defaults struct {
	Timeout        Duration `yaml:"timeout"`
	RerouteTimeout Duration `yaml:"reroute_timeout"`
	Cooldown       Duration `yaml:"cooldown"`
}

// Config is the root configuration document.
type Config struct {
	Server      Server                `yaml:"server"`
	Defaults    Defaults              `yaml:"defaults"`
	Credentials map[string]Credential `yaml:"credentials"`
	Models      []LogicalModel        `yaml:"models"`
}

// Duration is a time.Duration that (un)marshals from YAML strings like "30s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		var dur time.Duration
		if err2 := value.Decode(&dur); err2 != nil {
			return fmt.Errorf("invalid duration: %w", err2)
		}
		*d = Duration(dur)
		return nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(dur)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// AsDuration converts to time.Duration.
func (d Duration) AsDuration() time.Duration { return time.Duration(d) }

// Load reads, expands environment variables and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	data = []byte(ExpandEnv(string(data)))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ExpandEnv replaces ${VAR} and $VAR occurrences using the environment.
func ExpandEnv(s string) string {
	return os.Expand(s, func(k string) string {
		if k == "" {
			return ""
		}
		return os.Getenv(k)
	})
}

func (c *Config) validate() error {
	if len(c.Credentials) == 0 {
		return fmt.Errorf("at least one credential must be defined")
	}
	for name, cred := range c.Credentials {
		switch cred.Type {
		case "openai":
			if cred.APIKey == "" {
				return fmt.Errorf("credential %q: api_key is required", name)
			}
		case "azure":
			if cred.APIKey == "" {
				return fmt.Errorf("credential %q: api_key is required", name)
			}
			if cred.Endpoint == "" {
				return fmt.Errorf("credential %q: endpoint is required for azure credentials", name)
			}
			if !strings.HasPrefix(cred.Endpoint, "http://") && !strings.HasPrefix(cred.Endpoint, "https://") {
				return fmt.Errorf("credential %q: endpoint must be a full URL", name)
			}
		default:
			return fmt.Errorf("credential %q: unknown type %q (want openai or azure)", name, cred.Type)
		}
	}

	if len(c.Models) == 0 {
		return fmt.Errorf("at least one logical model must be defined")
	}
	seen := make(map[string]bool, len(c.Models))
	for i := range c.Models {
		m := &c.Models[i]
		if m.Name == "" {
			return fmt.Errorf("model[%d]: name is required", i)
		}
		if seen[m.Name] {
			return fmt.Errorf("duplicate logical model %q", m.Name)
		}
		seen[m.Name] = true
		if m.Domain == "" {
			return fmt.Errorf("model %q: domain is required", m.Name)
		}
		switch m.Mode {
		case ModeRoundRobin, ModeFallback:
		case "":
			m.Mode = ModeFallback
		default:
			return fmt.Errorf("model %q: unknown mode %q (want round_robin or fallback)", m.Name, m.Mode)
		}
		if len(m.Backends) == 0 {
			return fmt.Errorf("model %q: at least one backend is required", m.Name)
		}
		for j := range m.Backends {
			b := &m.Backends[j]
			if b.Model == "" {
				return fmt.Errorf("model %q: backend[%d] model is required", m.Name, j)
			}
			if _, ok := c.Credentials[b.Credential]; !ok {
				return fmt.Errorf("model %q: backend[%d] references unknown credential %q", m.Name, j, b.Credential)
			}
		}
	}
	return nil
}
