package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/federation"
)

const testToken = "test-token"

type mockAPFederator struct {
	resolveFollowTargetFn func(ctx context.Context, input string) (string, error)
	fetchActorFn          func(ctx context.Context, actorURL string) (*activitypub.Actor, error)
	deliverActivityFn     func(ctx context.Context, inboxURL string, activityJSON []byte) error
	sendAcceptFn          func(ctx context.Context, followerActorURL string) error
	sendRejectFn          func(ctx context.Context, followerActorURL string) error
	sendFollowFn          func(ctx context.Context, targetActorURL string) (string, error)
}

func (m *mockAPFederator) ResolveFollowTarget(ctx context.Context, input string) (string, error) {
	if m.resolveFollowTargetFn != nil {
		return m.resolveFollowTargetFn(ctx, input)
	}
	return input, nil
}

func (m *mockAPFederator) FetchActor(ctx context.Context, actorURL string) (*activitypub.Actor, error) {
	if m.fetchActorFn != nil {
		return m.fetchActorFn(ctx, actorURL)
	}
	return nil, errors.New("fetchActor not configured")
}

func (m *mockAPFederator) DeliverActivity(ctx context.Context, inboxURL string, activityJSON []byte) error {
	if m.deliverActivityFn != nil {
		return m.deliverActivityFn(ctx, inboxURL, activityJSON)
	}
	return nil
}

func (m *mockAPFederator) SendAccept(ctx context.Context, followerActorURL string) error {
	if m.sendAcceptFn != nil {
		return m.sendAcceptFn(ctx, followerActorURL)
	}
	return errors.New("sendAccept not configured")
}

func (m *mockAPFederator) SendReject(ctx context.Context, followerActorURL string) error {
	if m.sendRejectFn != nil {
		return m.sendRejectFn(ctx, followerActorURL)
	}
	return errors.New("sendReject not configured")
}

func (m *mockAPFederator) SendUndo(_ context.Context, _ string) error {
	return nil
}

func (m *mockAPFederator) SendFollow(ctx context.Context, targetActorURL string) (string, error) {
	if m.sendFollowFn != nil {
		return m.sendFollowFn(ctx, targetActorURL)
	}
	return targetActorURL, nil
}

func testServerWithMock(t *testing.T, fed federation.Federator) *Server {
	t.Helper()
	s := testServer(t)
	s.fedSvc = &federation.Service{
		Fed:      fed,
		DB:       s.db,
		ActorURL: s.identity.ActorURL,
		Logger:   s.logger,
	}
	return s
}

func adminActor(actorURL, inboxURL string) *activitypub.Actor { //nolint:unparam // test helper, param aids readability
	return &activitypub.Actor{
		ID:    actorURL,
		Inbox: inboxURL,
		PublicKey: activitypub.ActorPublicKey{
			ID:           actorURL + "#main-key",
			Owner:        actorURL,
			PublicKeyPEM: "-----BEGIN PUBLIC KEY-----\nfake\n-----END PUBLIC KEY-----",
		},
	}
}

func TestAdminGetIdentityFields(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/identity", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var info map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&info))
	require.Equal(t, "test-node", info["name"])
	require.Equal(t, "test.example.com", info["domain"])
	require.Equal(t, "test.example.com", info["accountDomain"])
	require.Equal(t, "https://test.example.com", info["endpoint"])
	require.NotEmpty(t, info["actorURL"])
	require.NotEmpty(t, info["keyID"])
	require.Contains(t, info["publicKey"], "-----BEGIN PUBLIC KEY-----")
}

func TestAdminListFollowsEmpty(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var follows []any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&follows))
	require.Empty(t, follows)
}

func TestAdminListFollowsWithData(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	require.NoError(t, s.db.AddFollow(ctx, "https://alice.example.com/ap/actor", "pubkey-alice", "https://alice.example.com", nil))
	require.NoError(t, s.db.AddFollow(ctx, "https://bob.example.com/ap/actor", "pubkey-bob", "https://bob.example.com", nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var follows []struct {
		ActorURL string `json:"actor_url"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&follows))
	require.Len(t, follows, 2)

	seen := make(map[string]bool)
	for _, f := range follows {
		seen[f.ActorURL] = true
	}
	require.True(t, seen["https://alice.example.com/ap/actor"])
	require.True(t, seen["https://bob.example.com/ap/actor"])
}

func TestAdminListFollowsInternalError(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	_ = s.db.Close() // force DB error

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestAdminListPendingEmpty(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows/pending", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var requests []any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&requests))
	require.Empty(t, requests)
}

func TestAdminListPendingWithData(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	require.NoError(t, s.db.AddFollowRequest(ctx, "https://carol.example.com/ap/actor", "pubkey-carol", "https://carol.example.com", nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows/pending", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var requests []struct {
		ActorURL string `json:"ActorURL"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&requests))
	require.Len(t, requests, 1)
	require.Equal(t, "https://carol.example.com/ap/actor", requests[0].ActorURL)
}

func TestAdminAddFollowMissingTarget(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	for _, body := range []string{`{}`, `{"target":""}`, `not-json`} {
		req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body: %s", body)
	}
}

func TestAdminAddFollowResolveError(t *testing.T) {
	fed := &mockAPFederator{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("resolve failed")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows", strings.NewReader(`{"target":"bad-target"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestAdminAddFollowFetchActorError(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	fed := &mockAPFederator{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return nil, errors.New("unreachable")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestAdminAddFollowDeliveryError(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"
	fed := &mockAPFederator{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return adminActor(actorURL, inboxURL), nil
		},
		deliverActivityFn: func(_ context.Context, _ string, _ []byte) error {
			return errors.New("delivery failed")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestAdminAddFollowSuccess(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	var deliveredInbox string
	fed := &mockAPFederator{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return adminActor(actorURL, inboxURL), nil
		},
		deliverActivityFn: func(_ context.Context, inbox string, _ []byte) error {
			deliveredInbox = inbox
			return nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["followed"])
	require.Equal(t, inboxURL, deliveredInbox)

	ctx := context.Background()
	of, err := s.db.GetOutgoingFollow(ctx, actorURL)
	require.NoError(t, err)
	require.NotNil(t, of, "outgoing follow should be persisted")
	require.NotNil(t, of.WeFollowStatus)
	require.Equal(t, "pending", *of.WeFollowStatus)
}

func TestAdminAcceptFollowMissingTarget(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdminAcceptFollowSendAcceptError(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	fed := &mockAPFederator{
		sendAcceptFn: func(_ context.Context, _ string) error {
			return errors.New("no pending request")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestAdminAcceptFollowSuccess(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	ctx := context.Background()

	var acceptedURL string
	fed := &mockAPFederator{
		sendAcceptFn: func(_ context.Context, followerActorURL string) error {
			acceptedURL = followerActorURL
			return nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollowRequest(ctx, actorURL, "pubkey-peer", inboxURL, nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["accepted"])
	require.Equal(t, actorURL, acceptedURL)
}

func TestAdminAcceptFollowMutualFollowBack(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	ctx := context.Background()

	var followBackActor string
	fed := &mockAPFederator{
		sendAcceptFn: func(_ context.Context, _ string) error { return nil },
		sendFollowFn: func(_ context.Context, actor string) (string, error) {
			followBackActor = actor
			return actor, nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	s.cfg.Federation.AutoAccept = activitypub.AutoAcceptMutual
	require.NoError(t, s.db.AddFollowRequest(ctx, actorURL, "pubkey-peer", inboxURL, nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["accepted"])
	require.Equal(t, actorURL, result["followed_back"])
	require.Equal(t, actorURL, followBackActor, "SendFollow should be called with the accepted actor")
}

func TestAdminAcceptFollowMutualSkipsExistingOutgoing(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	ctx := context.Background()

	followCalled := false
	fed := &mockAPFederator{
		sendAcceptFn: func(_ context.Context, _ string) error { return nil },
		sendFollowFn: func(_ context.Context, _ string) (string, error) {
			followCalled = true
			return "", nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	s.cfg.Federation.AutoAccept = activitypub.AutoAcceptMutual
	require.NoError(t, s.db.AddFollowRequest(ctx, actorURL, "pubkey-peer", inboxURL, nil))
	// Already following this actor.
	require.NoError(t, s.db.AddOutgoingFollow(ctx, actorURL))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/accept", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["accepted"])
	require.Empty(t, result["followed_back"], "should not follow back when outgoing follow already exists")
	require.False(t, followCalled, "SendFollow should not be called when outgoing follow already exists")
}

func TestAdminRejectFollowMissingTarget(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/reject", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdminRejectFollowSendRejectError(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	fed := &mockAPFederator{
		sendRejectFn: func(_ context.Context, _ string) error {
			return errors.New("no pending request")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/reject", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestAdminRejectFollowSuccess(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	ctx := context.Background()

	var rejectedURL string
	fed := &mockAPFederator{
		sendRejectFn: func(_ context.Context, followerActorURL string) error {
			rejectedURL = followerActorURL
			return nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollowRequest(ctx, actorURL, "pubkey-peer", inboxURL, nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows/reject", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["rejected"])
	require.Equal(t, actorURL, rejectedURL)
}

func TestAdminRemoveFollowMissingTarget(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("DELETE", srv.URL+"/api/admin/follows", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdminRemoveFollowNotFound(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/admin/follows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestAdminRemoveFollowSuccess(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	ctx := context.Background()

	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollow(ctx, actorURL, "pubkey-peer", "https://peer.example.com", nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/admin/follows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, actorURL, result["removed"])

	f, err := s.db.GetFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, f, "follow should be removed from the DB")
}

func TestAdminRemoveFollowOnlyOutgoing(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	ctx := context.Background()

	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddOutgoingFollow(ctx, actorURL))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/admin/follows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	of, err := s.db.GetOutgoingFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, of, "outgoing follow should be removed")
}

func TestAdminRemoveFollowBothTables(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	ctx := context.Background()

	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollow(ctx, actorURL, "pubkey-peer", "https://peer.example.com", nil))
	require.NoError(t, s.db.AddOutgoingFollow(ctx, actorURL))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/admin/follows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	f, err := s.db.GetFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, f, "inbound follow should be removed")

	of, err := s.db.GetOutgoingFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, of, "outgoing follow should be removed")
}

func TestAdminRemoveFollowForceRemovesDespiteUnreachablePeer(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	ctx := context.Background()

	// Federator that always fails to resolve and send Undo.
	fed := &mockAPFederator{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("peer unreachable")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollow(ctx, actorURL, "pubkey-peer", "https://peer.example.com", nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `","force":true}`
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/admin/follows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	f, err := s.db.GetFollow(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, f, "follow should be removed even when peer is unreachable")

	a, err := s.db.GetActor(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, a, "actor row should be fully deleted on force")
}

func TestAdminRemoveFollowWithoutForceFailsOnUnreachablePeer(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	ctx := context.Background()

	fed := &mockAPFederator{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("peer unreachable")
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	require.NoError(t, s.db.AddFollow(ctx, actorURL, "pubkey-peer", "https://peer.example.com", nil))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// No force flag — the resolve error should surface as a 500.
	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/admin/follows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestAdminAddFollowCreatesOutgoingNotInbound(t *testing.T) {
	const actorURL = "https://peer.example.com/ap/actor"
	const inboxURL = "https://peer.example.com/ap/inbox"

	fed := &mockAPFederator{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return adminActor(actorURL, inboxURL), nil
		},
		deliverActivityFn: func(_ context.Context, _ string, _ []byte) error {
			return nil
		},
	}
	s := testServerWithMock(t, fed)
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	body := `{"target":"` + actorURL + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/admin/follows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	ctx := context.Background()

	// Must be in outgoing_follows.
	of, err := s.db.GetOutgoingFollow(ctx, actorURL)
	require.NoError(t, err)
	require.NotNil(t, of, "outgoing follow should be persisted")

	// Must NOT be in follow_requests (that's for inbound requests).
	fr, err := s.db.GetFollowRequest(ctx, actorURL)
	require.NoError(t, err)
	require.Nil(t, fr, "follow_requests should not have outgoing follow")
}

func TestAdminAllEndpointsRequireAuth(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	type endpoint struct {
		method string
		path   string
		body   string
	}
	endpoints := []endpoint{
		{http.MethodGet, "/api/admin/identity", ""},
		{http.MethodGet, "/api/admin/follows", ""},
		{http.MethodGet, "/api/admin/follows/pending", ""},
		{http.MethodPost, "/api/admin/follows", `{"target":"https://x.example.com/ap/actor"}`},
		{http.MethodPost, "/api/admin/follows/accept", `{"target":"https://x.example.com/ap/actor"}`},
		{http.MethodPost, "/api/admin/follows/reject", `{"target":"https://x.example.com/ap/actor"}`},
		{http.MethodDelete, "/api/admin/follows", `{"target":"https://x.example.com/ap/actor"}`},
	}

	for _, ep := range endpoints {
		req, _ := http.NewRequest(ep.method, srv.URL+ep.path, strings.NewReader(ep.body))
		if ep.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"%s %s without auth should return 401", ep.method, ep.path)
	}
}

func TestAdminAllEndpointsRejectWrongToken(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = "correct-token"
	s.cfg.AdminToken = "correct-token"
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAdminListOutgoingFollowsEmpty(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows/outgoing", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var follows []any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&follows))
	require.Empty(t, follows)
}

func TestAdminListOutgoingFollowsWithData(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	// Add some outgoing follows with different statuses
	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://pending.example.com/ap/actor"))
	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://accepted.example.com/ap/actor"))
	require.NoError(t, s.db.AcceptOutgoingFollow(ctx, "https://accepted.example.com/ap/actor"))
	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://rejected.example.com/ap/actor"))
	require.NoError(t, s.db.RejectOutgoingFollow(ctx, "https://rejected.example.com/ap/actor"))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Test listing all
	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows/outgoing", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var follows []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&follows))
	require.Len(t, follows, 3)
}

func TestAdminListOutgoingFollowsFilterByStatus(t *testing.T) {
	s := testServerWithMock(t, &mockAPFederator{})
	s.cfg.RegistryToken = testToken
	s.cfg.AdminToken = testToken
	ctx := context.Background()

	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://pending1.example.com/ap/actor"))
	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://pending2.example.com/ap/actor"))
	require.NoError(t, s.db.AddOutgoingFollow(ctx, "https://accepted.example.com/ap/actor"))
	require.NoError(t, s.db.AcceptOutgoingFollow(ctx, "https://accepted.example.com/ap/actor"))

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Test filtering by pending status
	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows/outgoing?status=pending", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var follows []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&follows))
	require.Len(t, follows, 2)
	for _, f := range follows {
		require.Equal(t, "pending", f["we_follow_status"])
	}
}
