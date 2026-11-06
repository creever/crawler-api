// Package adapters defines the per-site adapter interface used to push
// approved SEO changes to the target website, plus the adapter registry.
package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Change is one approved SEO change to push to the site.
type Change struct {
	Kind  string // title | meta_description | category_text
	Page  string
	Value string
}

// Adapter pushes changes to a specific site/CMS.
type Adapter interface {
	// Name returns the adapter identifier (e.g. "cipomarket").
	Name() string
	// Apply pushes one change to the site. endpoint and apiKey come from the
	// project configuration.
	Apply(ctx context.Context, endpoint, apiKey string, change Change) error
}

// Get returns the adapter for the given type name, or an error if unknown.
func Get(adapterType string) (Adapter, error) {
	switch adapterType {
	case "cipomarket":
		return CipomarketAdapter{}, nil
	default:
		return nil, fmt.Errorf("unknown site adapter %q", adapterType)
	}
}

// CipomarketAdapter writes category descriptions and title/meta overrides
// to the cipomarket.hu webshop admin API.
type CipomarketAdapter struct{}

func (CipomarketAdapter) Name() string { return "cipomarket" }

// Apply POSTs the change as JSON to the webshop's override endpoint.
// Expected endpoint shape: {base}/seo-overrides with a Bearer API key.
func (CipomarketAdapter) Apply(ctx context.Context, endpoint, apiKey string, change Change) error {
	if endpoint == "" {
		return fmt.Errorf("cipomarket adapter: no endpoint configured")
	}
	body, err := json.Marshal(map[string]string{
		"kind":  change.Kind,
		"page":  change.Page,
		"value": change.Value,
	})
	if err != nil {
		return fmt.Errorf("marshal change: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("apply request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("site returned HTTP %s: %s", resp.Status, truncateStr(string(b), 200))
	}
	return nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
