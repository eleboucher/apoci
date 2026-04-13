package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/version"
)

type Client struct {
	base   string
	token  string
	client *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		base:  baseURL + "/api/admin",
		token: token,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) GetIdentity(ctx context.Context) (map[string]string, error) {
	var out map[string]string
	return out, c.get(ctx, "/identity", &out)
}

func (c *Client) ListFollows(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.get(ctx, "/follows", &out)
}

func (c *Client) ListPending(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.get(ctx, "/follows/pending", &out)
}

func (c *Client) ListOutgoingFollows(ctx context.Context, status string) (json.RawMessage, error) {
	var out json.RawMessage
	path := "/follows/outgoing"
	if status != "" {
		path += "?status=" + status
	}
	return out, c.get(ctx, path, &out)
}

func (c *Client) AddFollow(ctx context.Context, target string) (map[string]string, error) {
	var out map[string]string
	return out, c.post(ctx, "/follows", map[string]string{"target": target}, &out)
}

func (c *Client) AcceptFollow(ctx context.Context, target string) (map[string]string, error) {
	var out map[string]string
	return out, c.post(ctx, "/follows/accept", map[string]string{"target": target}, &out)
}

func (c *Client) RejectFollow(ctx context.Context, target string) (map[string]string, error) {
	var out map[string]string
	return out, c.post(ctx, "/follows/reject", map[string]string{"target": target}, &out)
}

func (c *Client) RemoveFollow(ctx context.Context, target string, force bool) (map[string]string, error) {
	var out map[string]string
	body := map[string]any{"target": target}
	if force {
		body["force"] = true
	}
	return out, c.do(ctx, http.MethodDelete, "/follows", body, &out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", version.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
