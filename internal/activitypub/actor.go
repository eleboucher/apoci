package activitypub

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type Actor struct {
	Context           []any             `json:"@context"`
	Type              string            `json:"type"`
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	PreferredUsername string            `json:"preferredUsername"`
	Inbox             string            `json:"inbox"`
	Outbox            string            `json:"outbox"`
	Followers         string            `json:"followers"`
	Following         string            `json:"following"`
	PublicKey         ActorPublicKey    `json:"publicKey"`
	Endpoints         map[string]string `json:"endpoints,omitempty"`
	URL               string            `json:"url,omitempty"`
	OCINamespace      string            `json:"ociNamespace,omitempty"`
}

type ActorPublicKey struct {
	ID           string `json:"id"`
	Owner        string `json:"owner"`
	PublicKeyPEM string `json:"publicKeyPem"`
}

type ActorHandler struct {
	identity *Identity
	nodeName string
	endpoint string
}

func NewActorHandler(identity *Identity, nodeName, endpoint string) *ActorHandler {
	return &ActorHandler{
		identity: identity,
		nodeName: nodeName,
		endpoint: endpoint,
	}
}

func (h *ActorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pubPEM, err := h.identity.PublicKeyPEM()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	base := h.endpoint
	actor := Actor{
		Context: []any{
			"https://www.w3.org/ns/activitystreams",
			"https://w3id.org/security/v1",
		},
		Type:              "Application",
		ID:                h.identity.ActorURL,
		Name:              h.nodeName,
		PreferredUsername: "registry",
		Inbox:             base + "/ap/inbox",
		Outbox:            base + "/ap/outbox",
		Followers:         base + "/ap/followers",
		Following:         base + "/ap/following",
		PublicKey: ActorPublicKey{
			ID:           h.identity.KeyID(),
			Owner:        h.identity.ActorURL,
			PublicKeyPEM: pubPEM,
		},
		Endpoints: map[string]string{
			"sharedInbox": base + "/ap/inbox",
		},
		URL:          h.endpoint,
		OCINamespace: h.identity.AccountDomain,
	}

	w.Header().Set("Content-Type", "application/activity+json")
	if err := json.NewEncoder(w).Encode(actor); err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
}

func ParseActor(data []byte) (*Actor, error) {
	var actor Actor
	if err := json.Unmarshal(data, &actor); err != nil {
		return nil, fmt.Errorf("parsing actor document: %w", err)
	}
	if actor.ID == "" {
		return nil, fmt.Errorf("actor document missing id")
	}
	return &actor, nil
}
