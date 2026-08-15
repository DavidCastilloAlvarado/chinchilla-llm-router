// Package provider wraps the official openai-go client for OpenAI and
// Azure OpenAI (Foundry) upstreams.
package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"

	"llm-router/internal/config"
)

// DefaultAzureAPIVersion is used when a credential does not pin an api-version.
const DefaultAzureAPIVersion = "2024-10-21"

// DefaultOpenAIBaseURL is the standard OpenAI API base URL.
const DefaultOpenAIBaseURL = "https://api.openai.com/v1"

// Client talks to one upstream credential (OpenAI or Azure OpenAI).
//
// The client is a thin, retry-free transport: failover, rerouting and
// timeouts are the router's responsibility, so SDK-level retries are
// disabled (WithMaxRetries(0)) to keep the semantics deterministic.
type Client struct {
	client openai.Client
	kind   string // "openai" | "azure"
}

// New builds a Client for the given credential.
func New(cred config.Credential) (*Client, error) {
	switch cred.Type {
	case "openai":
		base := cred.BaseURL
		if base == "" {
			base = DefaultOpenAIBaseURL
		}
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		c := openai.NewClient(
			option.WithAPIKey(cred.APIKey),
			option.WithBaseURL(base),
			option.WithMaxRetries(0),
		)
		return &Client{client: c, kind: "openai"}, nil
	case "azure":
		apiVersion := cred.APIVersion
		if apiVersion == "" {
			apiVersion = DefaultAzureAPIVersion
		}
		c := openai.NewClient(
			azure.WithEndpoint(cred.Endpoint, apiVersion),
			azure.WithAPIKey(cred.APIKey),
			option.WithMaxRetries(0),
		)
		return &Client{client: c, kind: "azure"}, nil
	default:
		return nil, fmt.Errorf("unknown credential type %q (want openai or azure)", cred.Type)
	}
}

// Kind returns "openai" or "azure".
func (c *Client) Kind() string { return c.kind }

// Do issues a raw JSON POST to path (e.g. "/chat/completions" or
// "/completions") against the upstream and returns the raw HTTP response.
//
// The caller owns resp.Body and must close it. When the upstream answers
// with status >= 400, err is non-nil (an openai-go API error) but resp is
// still populated and its body contains the upstream error payload, so it
// can be passed through to the caller unchanged.
func (c *Client) Do(ctx context.Context, path string, body map[string]any) (resp *http.Response, err error) {
	var raw *http.Response
	err = c.client.Execute(ctx, http.MethodPost, path, body, nil,
		option.WithResponseInto(&raw))
	if raw == nil {
		return nil, err
	}
	return raw, err
}
