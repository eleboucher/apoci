// Package metrics provides application-level metrics using expvar.
// All metrics are served at /debug/vars as JSON.
package metrics

import "expvar"

// Inbox: inbound activity counters by type.
var (
	InboxActivities  = expvar.NewMap("inbox_activities") // by type: Follow, Create, etc.
	InboxDedupHits   = expvar.NewInt("inbox_dedup_hits")
	InboxRateLimited = expvar.NewInt("inbox_rate_limited")
)

// Publisher: outbound activity counters.
var (
	OutboundActivities = expvar.NewMap("outbound_activities") // by type
)

// Delivery queue.
var (
	DeliveryEnqueued  = expvar.NewInt("delivery_enqueued")
	DeliverySucceeded = expvar.NewInt("delivery_succeeded")
	DeliveryFailed    = expvar.NewInt("delivery_failed")
	DeliveryRetries   = expvar.NewInt("delivery_retries")
	DeliveryPending   = expvar.NewInt("delivery_pending") // gauge, updated per batch
)

// Blob replication.
var (
	BlobReplicationsStarted   = expvar.NewInt("blob_replications_started")
	BlobReplicationsSucceeded = expvar.NewInt("blob_replications_succeeded")
	BlobReplicationsFailed    = expvar.NewInt("blob_replications_failed")
	BlobReplicationsInFlight  = expvar.NewInt("blob_replications_inflight") // gauge
)

// Garbage collection.
var (
	GCStalePeerBlobs   = expvar.NewInt("gc_stale_peer_blobs")
	GCOrphanedMetadata = expvar.NewInt("gc_orphaned_metadata")
	GCOrphanedFiles    = expvar.NewInt("gc_orphaned_files")
	GCCyclesCompleted  = expvar.NewInt("gc_cycles_completed")
)

// OCI registry operations.
var (
	RegistryManifestPushes   = expvar.NewInt("registry_manifest_pushes")
	RegistryManifestPulls    = expvar.NewInt("registry_manifest_pulls")
	RegistryBlobPushes       = expvar.NewInt("registry_blob_pushes")
	RegistryBlobPulls        = expvar.NewInt("registry_blob_pulls")
	RegistryBlobPullThru     = expvar.NewInt("registry_blob_pull_throughs")
	RegistryManifestPullThru = expvar.NewInt("registry_manifest_pull_throughs")
	RegistryPushRateLimited  = expvar.NewInt("registry_push_rate_limited")
)

// Federation state (gauges, set periodically or on change).
var (
	FederationFollowers = expvar.NewInt("federation_followers")
	FederationFollowing = expvar.NewInt("federation_following")
)
