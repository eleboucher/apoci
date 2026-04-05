package activitypub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeInfoWellKnown(t *testing.T) {
	handler := NewNodeInfoHandler("test.example.com", "0.1.0", nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/.well-known/nodeinfo", nil)
	handler.ServeWellKnown(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp NodeInfoWellKnown
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Links, 1)
	require.Equal(t, "https://test.example.com/ap/nodeinfo/2.1", resp.Links[0].Href)
}

func TestNodeInfoDocument(t *testing.T) {
	handler := NewNodeInfoHandler("test.example.com", "0.1.0", nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ap/nodeinfo/2.1", nil)
	handler.ServeNodeInfo(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var info NodeInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&info))
	require.Equal(t, "2.1", info.Version)
	require.Equal(t, "apoci", info.Software.Name)
	require.Equal(t, []string{"activitypub"}, info.Protocols)
	require.Equal(t, 1, info.Usage.Users.Total)
	require.False(t, info.OpenRegistrations, "expected openRegistrations=false")
}

type mockStats struct{ count int }

func (m *mockStats) ManifestCount() int { return m.count }

func TestNodeInfoWithStats(t *testing.T) {
	handler := NewNodeInfoHandler("test.example.com", "0.1.0", &mockStats{count: 42})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ap/nodeinfo/2.1", nil)
	handler.ServeNodeInfo(rec, req)

	var info NodeInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&info))
	require.Equal(t, 42, info.Usage.LocalPosts)
}

func TestNodeInfoRejectsPost(t *testing.T) {
	handler := NewNodeInfoHandler("test.example.com", "0.1.0", nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ap/nodeinfo/2.1", nil)
	handler.ServeNodeInfo(rec, req)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
