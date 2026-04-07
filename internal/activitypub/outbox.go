package activitypub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const defaultPageSize = 20

type OutboxHandler struct {
	identity *Identity
	db       *database.DB
}

func NewOutboxHandler(identity *Identity, db *database.DB) *OutboxHandler {
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
	db       *database.DB
}

func NewFollowersHandler(identity *Identity, db *database.DB) *FollowersHandler {
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
	db       *database.DB
}

func NewFollowingHandler(identity *Identity, db *database.DB) *FollowingHandler {
	return &FollowingHandler{identity: identity, db: db}
}

func (h *FollowingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	baseURL := "https://" + h.identity.Domain + "/ap/following"

	outgoing, err := h.db.ListOutgoingFollows(r.Context(), "accepted")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var items []string
	for _, f := range outgoing {
		items = append(items, f.ActorURL)
	}

	collection := map[string]any{
		"@context":     ContextActivityStreams,
		"type":         "OrderedCollection",
		"id":           baseURL,
		"totalItems":   len(items),
		"orderedItems": items,
	}

	w.Header().Set("Content-Type", "application/activity+json")
	_ = json.NewEncoder(w).Encode(collection)
}
