// Package django provides an internal HTTP client for calling Django endpoints.
package django

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client calls Django internal APIs (e.g., CoA disconnect).
type Client struct {
	baseURL   string
	apiKey    string
	httpClient *http.Client
}

// NewClient creates a new Django internal API client.
func NewClient(baseURL, apiKey string) *Client {
	if baseURL == "" {
		baseURL = "http://flash-api:8000"
	}
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// DisconnectUser sends a CoA disconnect request to Django.
func (c *Client) DisconnectUser(ctx context.Context, username, nasIP string) error {
	if c.apiKey == "" {
		return fmt.Errorf("django internal api key not configured")
	}

	payload := map[string]string{
		"username":       username,
		"nas_ip_address": nasIP,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/internal/coa/disconnect", c.baseURL), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to build disconnect request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("disconnect request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("disconnect request returned status %d", resp.StatusCode)
	}
	return nil
}

// DisconnectUserByActiveSessions queries radacct for active sessions by username
// and issues CoA disconnect for each NAS IP found.
func (c *Client) DisconnectUserByActiveSessions(ctx context.Context, username string) error {
	// For simplicity, we query active sessions from Django or MySQL directly.
	// In this implementation, we rely on the Django endpoint to handle session lookup
	// if nas_ip_address is omitted or we can call the disconnect with a known NAS.
	// Since we don't know the NAS IPs from Go side without querying the DB,
	// we'll have the Go service query radacct for active sessions and then call disconnect.
	return fmt.Errorf("not implemented: use DisconnectUser with known nas_ip")
}
