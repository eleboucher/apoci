package activitypub

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/apoci/apoci/internal/validate"
)

var httpClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		DialContext: validate.SafeDialContext,
	},
}

func DeliverActivity(ctx context.Context, inboxURL string, activityJSON []byte, identity *Identity) error {
	if err := validateFederationURL(inboxURL); err != nil {
		return fmt.Errorf("unsafe inbox URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inboxURL, bytes.NewReader(activityJSON))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("Accept", "application/activity+json")

	if err := SignRequest(req, identity.KeyID(), identity.PrivateKey, activityJSON); err != nil {
		return fmt.Errorf("signing request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending to %s: %w", inboxURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("inbox %s returned %d", inboxURL, resp.StatusCode)
	}

	return nil
}

// FetchActor fetches and parses a remote actor profile, signing the GET request
// for compatibility with servers that require authenticated fetches (e.g. Mastodon secure mode).
func FetchActor(ctx context.Context, actorURL string) (*Actor, error) {
	return FetchActorSigned(ctx, actorURL, nil)
}

// FetchActorSigned fetches a remote actor, optionally signing the request.
func FetchActorSigned(ctx context.Context, actorURL string, identity *Identity) (*Actor, error) {
	if err := validateFederationURL(actorURL); err != nil {
		return nil, fmt.Errorf("unsafe actor URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", actorURL, nil) //nolint:gosec // actorURL is validated above
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/activity+json, application/ld+json")

	if identity != nil {
		if err := SignRequest(req, identity.KeyID(), identity.PrivateKey, nil); err != nil {
			return nil, fmt.Errorf("signing actor fetch: %w", err)
		}
	}

	resp, err := httpClient.Do(req) //nolint:gosec // actorURL is validated above
	if err != nil {
		return nil, fmt.Errorf("fetching actor %s: %w", actorURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("actor %s returned %d", actorURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, fmt.Errorf("reading actor response: %w", err)
	}

	return ParseActor(body)
}
