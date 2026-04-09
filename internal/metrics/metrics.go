package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	// Inbox: inbound activity counters by type.
	InboxActivities = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "inbox",
		Name:      "activities_total",
		Help:      "Total inbound activities by type.",
	}, []string{"type"})
	InboxDedupHits = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "inbox",
		Name:      "dedup_hits_total",
		Help:      "Total duplicate activities dropped.",
	})
	InboxRateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "inbox",
		Name:      "rate_limited_total",
		Help:      "Total inbound requests rejected by rate limiter.",
	})

	// Publisher: outbound activity counters.
	OutboundActivities = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "outbound",
		Name:      "activities_total",
		Help:      "Total outbound activities by type.",
	}, []string{"type"})

	// Delivery queue.
	DeliveryEnqueued = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "delivery",
		Name:      "enqueued_total",
		Help:      "Total deliveries enqueued.",
	})
	DeliverySucceeded = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "delivery",
		Name:      "succeeded_total",
		Help:      "Total deliveries succeeded.",
	})
	DeliveryFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "delivery",
		Name:      "failed_total",
		Help:      "Total deliveries permanently failed.",
	})
	DeliveryRetries = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "delivery",
		Name:      "retries_total",
		Help:      "Total delivery retries.",
	})
	DeliveryPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "apoci",
		Subsystem: "delivery",
		Name:      "pending",
		Help:      "Number of deliveries currently pending.",
	})
	DeliveryCircuitOpen = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "apoci",
		Subsystem: "delivery",
		Name:      "circuit_open_domains",
		Help:      "Number of peer domains currently circuit-broken (delivery skipped).",
	})

	// Blob replication.
	BlobReplicationsStarted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "blob_replication",
		Name:      "started_total",
		Help:      "Total blob replications started.",
	})
	BlobReplicationsSucceeded = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "blob_replication",
		Name:      "succeeded_total",
		Help:      "Total blob replications succeeded.",
	})
	BlobReplicationsFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "blob_replication",
		Name:      "failed_total",
		Help:      "Total blob replications failed.",
	})
	BlobReplicationsInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "apoci",
		Subsystem: "blob_replication",
		Name:      "in_flight",
		Help:      "Number of blob replications currently in progress.",
	})

	// Garbage collection.
	GCStalePeerBlobs = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "gc",
		Name:      "stale_peer_blobs_total",
		Help:      "Total stale peer blobs removed.",
	})
	GCOrphanedMetadata = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "gc",
		Name:      "orphaned_metadata_total",
		Help:      "Total orphaned metadata entries removed.",
	})
	GCOrphanedFiles = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "gc",
		Name:      "orphaned_files_total",
		Help:      "Total orphaned files removed.",
	})
	GCCyclesCompleted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "gc",
		Name:      "cycles_completed_total",
		Help:      "Total GC cycles completed.",
	})

	// OCI registry operations.
	RegistryManifestPushes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "registry",
		Name:      "manifest_pushes_total",
		Help:      "Total manifest pushes.",
	})
	RegistryManifestPulls = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "registry",
		Name:      "manifest_pulls_total",
		Help:      "Total manifest pulls.",
	})
	RegistryBlobPushes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "registry",
		Name:      "blob_pushes_total",
		Help:      "Total blob pushes.",
	})
	RegistryBlobPulls = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "registry",
		Name:      "blob_pulls_total",
		Help:      "Total blob pulls.",
	})
	RegistryBlobPullThru = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "registry",
		Name:      "blob_pull_throughs_total",
		Help:      "Total blob pull-throughs from peers.",
	})
	RegistryManifestPullThru = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "registry",
		Name:      "manifest_pull_throughs_total",
		Help:      "Total manifest pull-throughs from peers.",
	})
	RegistryPushRateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "apoci",
		Subsystem: "registry",
		Name:      "push_rate_limited_total",
		Help:      "Total pushes rejected by rate limiter.",
	})

	// Latency histograms.
	latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

	DeliveryDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "apoci",
		Subsystem: "delivery",
		Name:      "duration_seconds",
		Help:      "Duration of individual activity delivery attempts.",
		Buckets:   latencyBuckets,
	})
	BlobFetchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "apoci",
		Subsystem: "blob_replication",
		Name:      "fetch_duration_seconds",
		Help:      "Duration of blob fetch operations from peers.",
		Buckets:   latencyBuckets,
	})
	InboxProcessingDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "apoci",
		Subsystem: "inbox",
		Name:      "processing_duration_seconds",
		Help:      "Duration of inbox activity processing.",
		Buckets:   latencyBuckets,
	})
	PeerFetchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "apoci",
		Subsystem: "registry",
		Name:      "peer_fetch_duration_seconds",
		Help:      "Duration of pull-through manifest/blob fetches from peers.",
		Buckets:   latencyBuckets,
	})

	// Federation state (gauges).
	FederationFollowers = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "apoci",
		Subsystem: "federation",
		Name:      "followers",
		Help:      "Current number of followers.",
	})
	FederationFollowing = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "apoci",
		Subsystem: "federation",
		Name:      "following",
		Help:      "Current number of peers being followed.",
	})
)

func init() {
	prometheus.MustRegister(
		InboxActivities,
		InboxDedupHits,
		InboxRateLimited,
		OutboundActivities,
		DeliveryEnqueued,
		DeliverySucceeded,
		DeliveryFailed,
		DeliveryRetries,
		DeliveryPending,
		DeliveryCircuitOpen,
		BlobReplicationsStarted,
		BlobReplicationsSucceeded,
		BlobReplicationsFailed,
		BlobReplicationsInFlight,
		GCStalePeerBlobs,
		GCOrphanedMetadata,
		GCOrphanedFiles,
		GCCyclesCompleted,
		RegistryManifestPushes,
		RegistryManifestPulls,
		RegistryBlobPushes,
		RegistryBlobPulls,
		RegistryBlobPullThru,
		RegistryManifestPullThru,
		RegistryPushRateLimited,
		DeliveryDuration,
		BlobFetchDuration,
		InboxProcessingDuration,
		PeerFetchDuration,
		FederationFollowers,
		FederationFollowing,
	)
}
