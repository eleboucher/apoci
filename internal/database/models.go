package database

import (
	"time"

	"github.com/uptrace/bun"
)

// Outgoing follow status constants.
const (
	FollowStatusPending  = "pending"
	FollowStatusAccepted = "accepted"
	FollowStatusRejected = "rejected"
)

type Repository struct {
	bun.BaseModel `bun:"table:repositories"`

	ID        int64     `bun:"id,pk,autoincrement"`
	Name      string    `bun:"name,notnull,unique"`
	OwnerID   string    `bun:"owner_id,notnull"`
	Private   bool      `bun:"private,notnull,default:false"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

type Manifest struct {
	bun.BaseModel `bun:"table:manifests"`

	ID            int64     `bun:"id,pk,autoincrement"`
	RepositoryID  int64     `bun:"repository_id,notnull"`
	Digest        string    `bun:"digest,notnull"`
	MediaType     string    `bun:"media_type,notnull"`
	SizeBytes     int64     `bun:"size_bytes,notnull"`
	Content       []byte    `bun:"content"`
	SourceActor   *string   `bun:"source_actor"`
	SubjectDigest *string   `bun:"subject_digest"`
	ArtifactType  *string   `bun:"artifact_type"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

type Tag struct {
	bun.BaseModel `bun:"table:tags"`

	ID             int64     `bun:"id,pk,autoincrement"`
	RepositoryID   int64     `bun:"repository_id,notnull"`
	Name           string    `bun:"name,notnull"`
	ManifestDigest string    `bun:"manifest_digest,notnull"`
	Immutable      bool      `bun:"immutable,notnull,default:false"`
	UpdatedAt      time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

type Blob struct {
	bun.BaseModel `bun:"table:blobs"`

	ID            int64     `bun:"id,pk,autoincrement"`
	Digest        string    `bun:"digest,notnull,unique"`
	SizeBytes     int64     `bun:"size_bytes,notnull"`
	MediaType     *string   `bun:"media_type"`
	StoredLocally bool      `bun:"stored_locally,notnull,default:false"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

type RepositoryOwner struct {
	bun.BaseModel `bun:"table:repository_owners"`

	RepositoryID int64     `bun:"repository_id,notnull"`
	OwnerID      string    `bun:"owner_id,notnull"`
	GrantedAt    time.Time `bun:"granted_at,notnull,default:current_timestamp"`
}

type ManifestLayer struct {
	bun.BaseModel `bun:"table:manifest_layers"`

	ManifestID int64  `bun:"manifest_id,notnull"`
	BlobDigest string `bun:"blob_digest,notnull"`
}

type PeerBlob struct {
	bun.BaseModel `bun:"table:peer_blobs"`

	ID             int64      `bun:"id,pk,autoincrement"`
	PeerActor      string     `bun:"peer_actor,notnull"`
	BlobDigest     string     `bun:"blob_digest,notnull"`
	PeerEndpoint   string     `bun:"peer_endpoint,notnull"`
	LastVerifiedAt *time.Time `bun:"last_verified_at"`
}

// Actor represents a remote ActivityPub actor. Consolidates peers, follows, and outgoing_follows.
type Actor struct {
	bun.BaseModel `bun:"table:actors"`

	ID           int64   `bun:"id,pk,autoincrement" json:"id"`
	ActorURL     string  `bun:"actor_url,notnull,unique" json:"actor_url"`
	Name         *string `bun:"name" json:"name,omitempty"`
	Alias        *string `bun:"alias" json:"alias,omitempty"`
	Endpoint     string  `bun:"endpoint,notnull" json:"endpoint"`
	PublicKeyPEM *string `bun:"public_key_pem" json:"public_key_pem,omitempty"`

	// Inbound: they follow us
	TheyFollowUs   bool       `bun:"they_follow_us,notnull,default:false" json:"they_follow_us"`
	TheyFollowUsAt *time.Time `bun:"they_follow_us_at" json:"they_follow_us_at,omitempty"`

	// Outbound: we follow them
	WeFollowThem     bool       `bun:"we_follow_them,notnull,default:false" json:"we_follow_them"`
	WeFollowStatus   *string    `bun:"we_follow_status" json:"we_follow_status,omitempty"` // pending, accepted, rejected
	WeFollowAcceptAt *time.Time `bun:"we_follow_accept_at" json:"we_follow_accept_at,omitempty"`

	// Health & replication (for blob fetching)
	IsHealthy         bool       `bun:"is_healthy,notnull,default:true" json:"is_healthy"`
	ReplicationPolicy string     `bun:"replication_policy,notnull,default:'lazy'" json:"replication_policy"`
	LastSeenAt        *time.Time `bun:"last_seen_at" json:"last_seen_at,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

func (a *Actor) HasPendingOutgoingFollow() bool {
	return a != nil && a.WeFollowThem && a.WeFollowStatus != nil && *a.WeFollowStatus == FollowStatusPending
}

func (a *Actor) HasAcceptedOutgoingFollow() bool {
	return a != nil && a.WeFollowThem && a.WeFollowStatus != nil && *a.WeFollowStatus == FollowStatusAccepted
}

func (a *Actor) HasPendingOrAcceptedOutgoingFollow() bool {
	return a.HasPendingOutgoingFollow() || a.HasAcceptedOutgoingFollow()
}

func (a *Actor) GetPublicKeyPEM() string {
	if a == nil || a.PublicKeyPEM == nil {
		return ""
	}
	return *a.PublicKeyPEM
}

func (a *Actor) GetWeFollowStatus() string {
	if a == nil || a.WeFollowStatus == nil {
		return ""
	}
	return *a.WeFollowStatus
}

type FollowRequest struct {
	bun.BaseModel `bun:"table:follow_requests"`

	ID           int64     `bun:"id,pk,autoincrement"`
	ActorURL     string    `bun:"actor_url,notnull,unique"`
	PublicKeyPEM string    `bun:"public_key_pem,notnull"`
	Endpoint     string    `bun:"endpoint,notnull"`
	Alias        *string   `bun:"alias"`
	RequestedAt  time.Time `bun:"requested_at,notnull,default:current_timestamp"`
}

type Activity struct {
	bun.BaseModel `bun:"table:activities"`

	ID          int64     `bun:"id,pk,autoincrement"`
	ActivityID  string    `bun:"activity_id,notnull,unique"`
	Type        string    `bun:"type,notnull"`
	ActorURL    string    `bun:"actor_url,notnull"`
	ObjectJSON  []byte    `bun:"object_json,notnull"`
	PublishedAt time.Time `bun:"published_at,notnull,default:current_timestamp"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

type UploadSession struct {
	bun.BaseModel `bun:"table:upload_sessions"`

	ID            int64     `bun:"id,pk,autoincrement"`
	UUID          string    `bun:"uuid,notnull,unique"`
	RepositoryID  int64     `bun:"repository_id,notnull"`
	BytesReceived int64     `bun:"bytes_received,notnull,default:0"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
	ExpiresAt     time.Time `bun:"expires_at,notnull"`
}

type Delivery struct {
	bun.BaseModel `bun:"table:delivery_queue"`

	ID            int64     `bun:"id,pk,autoincrement"`
	ActivityID    string    `bun:"activity_id,notnull"`
	InboxURL      string    `bun:"inbox_url,notnull"`
	ActivityJSON  []byte    `bun:"activity_json,notnull"`
	Attempts      int       `bun:"attempts,notnull,default:0"`
	MaxAttempts   int       `bun:"max_attempts,notnull,default:10"`
	NextAttemptAt time.Time `bun:"next_attempt_at,notnull,default:current_timestamp"`
	LastError     *string   `bun:"last_error"`
	Status        string    `bun:"status,notnull,default:'pending'"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

type DeletedManifest struct {
	bun.BaseModel `bun:"table:deleted_manifests"`

	Digest      string    `bun:"digest,pk"`
	RepoName    string    `bun:"repo_name,notnull"`
	DeletedAt   time.Time `bun:"deleted_at,notnull,default:current_timestamp"`
	SourceActor string    `bun:"source_actor,notnull"`
}
