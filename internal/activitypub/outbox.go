package activitypub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const defaultPageSize = 20

type OutboxRepository interface {
	CountActivities(ctx context.Context, actorURL string) (int, error)
	ListActivitiesPage(ctx context.Context, actorURL string, beforeID int64, limit int) ([]database.Activity, error)
}

type FollowersRepository interface {
	CountFollows(ctx context.Context) (int, error)
	ListFollowsPage(ctx context.Context, offset, limit int) ([]database.Actor, error)
}

// FollowingRepository is the persistence port for the following handler.
type FollowingRepository interface {
	ListOutgoingFollows(ctx context.Context, status string) ([]database.Actor, error)
	CountOutgoingFollows(ctx context.Context, status string) (int, error)
	ListOutgoingFollowsPage(ctx context.Context, status string, limit, offset int) ([]database.Actor, error)
}

type OutboxHandler struct {
	identity *Identity
	db       OutboxRepository
}

func NewOutboxHandler(identity *Identity, db OutboxRepository) *OutboxHandler {
	return &OutboxHandler{identity: identity, db: db}
}

func (h *OutboxHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	baseURL := "https://" + h.identity.Domain + "/ap/outbox"
	pageParam := r.URL.Query().Get("page")

	// If no page param, return the collection summary with first/last links.
	if pageParam == "" {
		total, err := h.db.CountActivities(r.Context(), h.identity.ActorURL)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		collection := map[string]any{
			"@context":   ContextActivityStreams,
			"type":       "OrderedCollection",
			"id":         baseURL,
			"totalItems": total,
			"first":      baseURL + "?page=1",
		}

		w.Header().Set("Content-Type", "application/activity+json")
		_ = json.NewEncoder(w).Encode(collection)
		return
	}

	beforeID := int64(0)
	if cursor := r.URL.Query().Get("before"); cursor != "" {
		beforeID, _ = strconv.ParseInt(cursor, 10, 64)
	}

	activities, err := h.db.ListActivitiesPage(r.Context(), h.identity.ActorURL, beforeID, defaultPageSize+1)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	hasMore := len(activities) > defaultPageSize
	if hasMore {
		activities = activities[:defaultPageSize]
	}

	var items []json.RawMessage
	for _, a := range activities {
		items = append(items, json.RawMessage(a.ObjectJSON))
	}

	page := map[string]any{
		"@context":     ContextActivityStreams,
		"type":         "OrderedCollectionPage",
		"id":           fmt.Sprintf("%s?page=%s&before=%d", baseURL, pageParam, beforeID),
		"partOf":       baseURL,
		"orderedItems": items,
	}

	if hasMore {
		lastActivity := activities[len(activities)-1]
		page["next"] = fmt.Sprintf("%s?page=1&before=%d", baseURL, lastActivity.ID)
	}

	w.Header().Set("Content-Type", "application/activity+json")
	_ = json.NewEncoder(w).Encode(page)
}

type FollowersHandler struct {
	identity *Identity
	db       FollowersRepository
}

func NewFollowersHandler(identity *Identity, db FollowersRepository) *FollowersHandler {
	return &FollowersHandler{identity: identity, db: db}
}

func (h *FollowersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	baseURL := "https://" + h.identity.Domain + "/ap/followers"
	pageParam := r.URL.Query().Get("page")

	if pageParam == "" {
		total, err := h.db.CountFollows(r.Context())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		collection := map[string]any{
			"@context":   ContextActivityStreams,
			"type":       "OrderedCollection",
			"id":         baseURL,
			"totalItems": total,
			"first":      baseURL + "?page=1",
		}

		w.Header().Set("Content-Type", "application/activity+json")
		_ = json.NewEncoder(w).Encode(collection)
		return
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	follows, err := h.db.ListFollowsPage(r.Context(), offset, defaultPageSize+1)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	hasMore := len(follows) > defaultPageSize
	if hasMore {
		follows = follows[:defaultPageSize]
	}

	var items []string
	for _, f := range follows {
		items = append(items, f.ActorURL)
	}

	page := map[string]any{
		"@context":     ContextActivityStreams,
		"type":         "OrderedCollectionPage",
		"id":           fmt.Sprintf("%s?page=1&offset=%d", baseURL, offset),
		"partOf":       baseURL,
		"orderedItems": items,
	}

	if hasMore {
		page["next"] = fmt.Sprintf("%s?page=1&offset=%d", baseURL, offset+defaultPageSize)
	}

	w.Header().Set("Content-Type", "application/activity+json")
	_ = json.NewEncoder(w).Encode(page)
}

// FollowingHandler returns who this instance is following (outgoing follows).
type FollowingHandler struct {
	identity *Identity
	db       FollowingRepository
}

func NewFollowingHandler(identity *Identity, db FollowingRepository) *FollowingHandler {
	return &FollowingHandler{identity: identity, db: db}
}

func (h *FollowingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	baseURL := "https://" + h.identity.Domain + "/ap/following"

	offsetParam := r.URL.Query().Get("offset")
	if offsetParam == "" {
		// No page param — return collection summary with first link.
		total, err := h.db.CountOutgoingFollows(r.Context(), "accepted")
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		collection := map[string]any{
			"@context":   ContextActivityStreams,
			"type":       "OrderedCollection",
			"id":         baseURL,
			"totalItems": total,
			"first":      fmt.Sprintf("%s?offset=0", baseURL),
		}
		w.Header().Set("Content-Type", "application/activity+json")
		_ = json.NewEncoder(w).Encode(collection)
		return
	}

	offset, err := strconv.Atoi(offsetParam)
	if err != nil || offset < 0 {
		http.Error(w, "invalid offset", http.StatusBadRequest)
		return
	}

	total, err := h.db.CountOutgoingFollows(r.Context(), "accepted")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	follows, err := h.db.ListOutgoingFollowsPage(r.Context(), "accepted", defaultPageSize, offset)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	items := make([]string, 0, len(follows))
	for _, f := range follows {
		items = append(items, f.ActorURL)
	}

	page := map[string]any{
		"@context":     ContextActivityStreams,
		"type":         "OrderedCollectionPage",
		"id":           fmt.Sprintf("%s?offset=%d", baseURL, offset),
		"partOf":       baseURL,
		"totalItems":   total,
		"orderedItems": items,
	}
	if next := offset + defaultPageSize; next < total {
		page["next"] = fmt.Sprintf("%s?offset=%d", baseURL, next)
	}

	w.Header().Set("Content-Type", "application/activity+json")
	_ = json.NewEncoder(w).Encode(page)
}
