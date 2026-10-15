package tsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const baseURL = "https://api.tailscale.com/api/v2"

// Client is a minimal Tailscale API client for user seat management.
type Client struct {
	apiKey  string
	tailnet string
	http    *http.Client
}

func New(apiKey, tailnet string) *Client {
	if tailnet == "" {
		tailnet = "-"
	}
	return &Client{
		apiKey:  apiKey,
		tailnet: tailnet,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// User represents a Tailscale tailnet member as returned by the API.
type User struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"displayName"`
	LoginName   string    `json:"loginName"`
	TailnetID   string    `json:"tailnetId"`
	Type        string    `json:"type"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	DeviceCount int       `json:"deviceCount"`
	LastSeen    time.Time `json:"lastSeen"`
}

// ListUsers returns all member-type users in the tailnet, including suspended ones.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/tailnet/%s/users?type=member", baseURL, c.tailnet), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list users: HTTP %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Users []User `json:"users"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("list users: decode: %w", err)
	}
	return result.Users, nil
}

// SuspendUser suspends the user with the given stable user ID.
func (c *Client) SuspendUser(ctx context.Context, userID string) error {
	return c.postEmpty(ctx, fmt.Sprintf("%s/users/%s/suspend", baseURL, userID))
}

// RestoreUser restores a suspended user by their stable user ID.
func (c *Client) RestoreUser(ctx context.Context, userID string) error {
	return c.postEmpty(ctx, fmt.Sprintf("%s/users/%s/restore", baseURL, userID))
}

func (c *Client) postEmpty(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}
