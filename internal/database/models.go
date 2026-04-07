package database

import (
	"time"

	"github.com/uptrace/bun"
)

type Repository struct {
	bun.BaseModel `bun:"table:repositories"`

	ID        int64     `bun:"id,pk,autoincrement"`
	Name      string    `bun:"name,notnull,unique"`
	OwnerID   string    `bun:"owner_id,notnull"`
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

type Peer struct {
	bun.BaseModel `bun:"table:peers"`

	ID                int64      `bun:"id,pk,autoincrement"`
	ActorURL          string     `bun:"actor_url,notnull,unique"`
	Name              *string    `bun:"name"`
	Endpoint          string     `bun:"endpoint,notnull"`
	ReplicationPolicy string     `bun:"replication_policy,default:'lazy'"`
	LastSeenAt        *time.Time `bun:"last_seen_at"`
	IsHealthy         bool       `bun:"is_healthy,notnull,default:true"`
	CreatedAt         time.Time  `bun:"created_at,notnull,default:current_timestamp"`
}

type Follow struct {
	bun.BaseModel `bun:"table:follows"`

	ID           int64     `bun:"id,pk,autoincrement"`
	ActorURL     string    `bun:"actor_url,notnull,unique"`
	PublicKeyPEM string    `bun:"public_key_pem,notnull"`
	Endpoint     string    `bun:"endpoint,notnull"`
	ApprovedAt   time.Time `bun:"approved_at,notnull,default:current_timestamp"`
}

type FollowRequest struct {
	bun.BaseModel `bun:"table:follow_requests"`

	ID           int64     `bun:"id,pk,autoincrement"`
	ActorURL     string    `bun:"actor_url,notnull,unique"`
	PublicKeyPEM string    `bun:"public_key_pem,notnull"`
	Endpoint     string    `bun:"endpoint,notnull"`
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

type OutgoingFollow struct {
	bun.BaseModel `bun:"table:outgoing_follows"`

	ID         int64      `bun:"id,pk,autoincrement"`
	ActorURL   string     `bun:"actor_url,notnull,unique"`
	Status     string     `bun:"status,notnull,default:'pending'"`
	CreatedAt  time.Time  `bun:"created_at,notnull,default:current_timestamp"`
	AcceptedAt *time.Time `bun:"accepted_at"`
}
