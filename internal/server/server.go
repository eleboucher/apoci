package server

import (
	"context"
	"expvar"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/oci"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
)

type Server struct {
	cfg              *config.Config
	db               *database.DB
	blobs            *blobstore.Store
	identity         *activitypub.Identity
	apFed            apFederator
	registry         *oci.Registry
	publisher        *activitypub.APPublisher
	deliveryQueue    *activitypub.DeliveryQueue
	blobReplicator   *peering.BlobReplicator
	gc               *peering.GarbageCollector
	healthChecker    *peering.HealthChecker
	ociHandler       http.Handler
	actorHandler     http.Handler
	webfingerHandler http.Handler
	nodeinfoHandler  *activitypub.NodeInfoHandler
	inboxHandler     http.Handler
	outboxHandler    http.Handler
	followersHandler http.Handler
	followingHandler http.Handler
	inboxLimiter     *ipRateLimiter
	httpServer       *http.Server
	logger           *slog.Logger
}

func New(cfg *config.Config, db *database.DB, blobs *blobstore.Store, identity *activitypub.Identity, version string, logger *slog.Logger) *Server {
	registry := oci.NewRegistry(db, blobs, identity.ActorURL, cfg.Domain, cfg.ImmutableTags, cfg.Limits.MaxManifestSize, cfg.Limits.MaxBlobSize, logger)

	apPublisher := activitypub.NewAPPublisher(context.Background(), identity, db, cfg.Endpoint, logger)
	apResolver := activitypub.NewAPResolver(db, logger)
	deliveryQueue := activitypub.NewDeliveryQueue(db, identity, logger)
	fetcher := peering.NewFetcher(cfg.Peering.FetchTimeout, cfg.Limits.MaxBlobSize, cfg.Limits.MaxManifestSize, logger)
	healthChecker := peering.NewHealthChecker(db, fetcher, cfg.Peering.HealthCheckInterval, logger)

	blobReplicator := peering.NewBlobReplicator(db, blobs, fetcher, logger)
	gc := peering.NewGarbageCollector(db, blobs, logger)

	apPublisher.SetNotifyFunc(deliveryQueue.Notify)

	registry.SetPublisher(apPublisher)
	registry.SetFederation(apResolver, fetcher)

	inboxHandler := activitypub.NewInboxHandler(identity, db, activitypub.InboxConfig{
		MaxManifestSize: cfg.Limits.MaxManifestSize,
		MaxBlobSize:     cfg.Limits.MaxBlobSize,
		AutoAccept:      cfg.Federation.AutoAccept,
		AllowedDomains:  cfg.Federation.AllowedDomains,
		BlockedDomains:  cfg.Federation.BlockedDomains,
		BlockedActors:   cfg.Federation.BlockedActors,
	}, logger)
	inboxHandler.SetBlobReplicator(blobReplicator)

	s := &Server{
		cfg:              cfg,
		db:               db,
		blobs:            blobs,
		identity:         identity,
		apFed:            &realAPFederator{identity: identity, db: db},
		registry:         registry,
		publisher:        apPublisher,
		deliveryQueue:    deliveryQueue,
		blobReplicator:   blobReplicator,
		gc:               gc,
		healthChecker:    healthChecker,
		ociHandler:       registry.Handler(),
		actorHandler:     activitypub.NewActorHandler(identity, cfg.Name, cfg.Endpoint),
		webfingerHandler: activitypub.NewWebFingerHandler(identity),
		nodeinfoHandler:  activitypub.NewNodeInfoHandler(identity.Domain, version, nil),
		inboxHandler:     inboxHandler,
		outboxHandler:    activitypub.NewOutboxHandler(identity, db),
		followersHandler: activitypub.NewFollowersHandler(identity, db),
		followingHandler: activitypub.NewFollowingHandler(identity, db),
		inboxLimiter:     newIPRateLimiter(10, 50), // 10 req/sec, burst of 50
		logger:           logger,
	}

	s.httpServer = &http.Server{
		Handler:           s.routes(),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	return s
}

func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.Listen, err)
	}

	s.healthChecker.Start(ctx)
	s.deliveryQueue.Start(ctx)
	s.gc.Start(ctx)

	if s.cfg.Metrics.Enabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/debug/vars", expvar.Handler())
		var metricsHandler http.Handler = metricsMux
		if s.cfg.Metrics.Token != "" {
			metricsHandler = bearerAuthMiddleware(s.cfg.Metrics.Token)(metricsMux)
		}
		metricsServer := &http.Server{
			Addr:              s.cfg.Metrics.Listen,
			Handler:           metricsHandler,
			ReadTimeout:       5 * time.Second,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		go func() { //nolint:gosec // intentional background goroutine for metrics server
			s.logger.Info("metrics server listening", "address", s.cfg.Metrics.Listen)
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.logger.Error("metrics server error", "error", err)
			}
		}()
		go func() { //nolint:gosec // intentional background goroutine for graceful shutdown
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsServer.Shutdown(shutdownCtx)
		}()
	}

	follows, err := s.db.ListFollows(ctx)
	if err != nil {
		s.logger.Warn("failed to count followers", "error", err)
	}
	outgoing, err := s.db.ListOutgoingFollows(ctx, "accepted")
	if err != nil {
		s.logger.Warn("failed to count following", "error", err)
	}
	metrics.FederationFollowers.Set(int64(len(follows)))
	metrics.FederationFollowing.Set(int64(len(outgoing)))

	s.logger.Info("OCI registry listening",
		"address", ln.Addr().String(),
		"node", s.cfg.Name,
		"actor", s.identity.ActorURL,
		"followers", len(follows),
		"following", len(outgoing),
	)

	shutdownDone := make(chan struct{})
	go func() { //nolint:gosec // intentional background goroutine for graceful shutdown
		defer close(shutdownDone)
		<-ctx.Done()
		s.healthChecker.Stop()
		s.gc.Stop()
		s.logger.Info("waiting for in-flight blob replications")
		s.blobReplicator.Wait()
		s.logger.Info("stopping delivery queue")
		s.deliveryQueue.Stop()
		s.inboxLimiter.Stop()
		s.publisher.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.logger.Info("shutting down HTTP server")
		_ = s.httpServer.Shutdown(shutdownCtx)
	}()

	var serveErr error
	if s.cfg.TLS != nil {
		serveErr = s.httpServer.ServeTLS(ln, s.cfg.TLS.Cert, s.cfg.TLS.Key)
	} else {
		serveErr = s.httpServer.Serve(ln)
	}
	// Wait for shutdown goroutine to finish before returning.
	<-shutdownDone
	return serveErr
}
