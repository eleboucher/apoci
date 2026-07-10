package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/version"
)

type ImageEntry struct {
	Name      string    `json:"name"`
	Tags      []string  `json:"tags"`
	SizeBytes int64     `json:"size_bytes"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UploadSessionEntry is an in-progress chunked upload, as returned by
// GET /api/admin/uploads.
type UploadSessionEntry struct {
	UUID          string    `json:"uuid"`
	RepositoryID  int64     `json:"repository_id"`
	BytesReceived int64     `json:"bytes_received"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// IdentityResponse is the payload returned by GET /api/admin/identity.
type IdentityResponse struct {
	Name          string `json:"name"`
	ActorURL      string `json:"actorURL"`
	KeyID         string `json:"keyID"`
	Domain        string `json:"domain"`
	AccountDomain string `json:"accountDomain"`
	Endpoint      string `json:"endpoint"`
	PublicKey     string `json:"publicKey"`
}

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

func (c *Client) ListImages(ctx context.Context) ([]ImageEntry, error) {
	var out []ImageEntry
	return out, c.get(ctx, "/images", &out)
}

func (c *Client) ListActors(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.get(ctx, "/actors", &out)
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

const keyTarget = "target"

func (c *Client) AddFollow(ctx context.Context, target string) (map[string]string, error) {
	var out map[string]string
	return out, c.post(ctx, "/follows", map[string]string{keyTarget: target}, &out)
}

func (c *Client) AcceptFollow(ctx context.Context, target string) (map[string]string, error) {
	var out map[string]string
	return out, c.post(ctx, "/follows/accept", map[string]string{keyTarget: target}, &out)
}

func (c *Client) RejectFollow(ctx context.Context, target string) (map[string]string, error) {
	var out map[string]string
	return out, c.post(ctx, "/follows/reject", map[string]string{keyTarget: target}, &out)
}

func (c *Client) UpdateFollowFilter(ctx context.Context, target string, tagGlobs []string) (map[string]any, error) {
	var out map[string]any
	body := map[string]any{keyTarget: target, "tag_globs": tagGlobs}
	return out, c.do(ctx, http.MethodPatch, "/follows", body, &out)
}

func (c *Client) RemoveFollow(ctx context.Context, target string, force bool) (map[string]string, error) {
	var out map[string]string
	body := map[string]any{keyTarget: target}
	if force {
		body["force"] = true
	}
	return out, c.do(ctx, http.MethodDelete, "/follows", body, &out)
}

func (c *Client) RunGC(ctx context.Context) (map[string]string, error) {
	var out map[string]string
	return out, c.post(ctx, "/gc", nil, &out)
}

func (c *Client) ListUploads(ctx context.Context) ([]UploadSessionEntry, error) {
	var out []UploadSessionEntry
	return out, c.get(ctx, "/uploads", &out)
}

func (c *Client) PurgeUploads(ctx context.Context, repo string) (map[string]any, error) {
	path := "/uploads"
	if repo != "" {
		path += "?repo=" + url.QueryEscape(repo)
	}
	var out map[string]any
	return out, c.do(ctx, http.MethodDelete, path, nil, &out)
}

func (c *Client) EvictMirror(ctx context.Context, repo, digest string) (map[string]string, error) {
	var out map[string]string
	path := "/mirrors/" + repo
	if digest != "" {
		path += "?digest=" + url.QueryEscape(digest)
	}
	return out, c.do(ctx, http.MethodDelete, path, nil, &out)
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
