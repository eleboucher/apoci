package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/federation"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/oci"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/workers"
)

type Server struct {
	cfg                 *config.Config
	db                  *database.DB
	identity            *activitypub.Identity
	fedSvc              *federation.Service
	registry            *oci.Registry
	workers             *workers.Workers
	ociHandler          http.Handler
	actorHandler        http.Handler
	webfingerHandler    http.Handler
	nodeinfoHandler     *activitypub.NodeInfoHandler
	inboxHandler        *activitypub.InboxHandler
	outboxHandler       http.Handler
	followersHandler    http.Handler
	followingHandler    http.Handler
	inboxLimiter        *ipRateLimiter
	registryPushLimiter *ipRateLimiter
	httpServer          *http.Server
	logger              *slog.Logger
}

func New(cfg *config.Config, db *database.DB, blobs *blobstore.Store, identity *activitypub.Identity, version string, logger *slog.Logger) (*Server, error) {
	if cfg.Federation.AllowInsecureHTTP {
		activitypub.SetAllowInsecureHTTP(true)
		validate.AllowPrivateIPs.Store(true)
		logger.Warn("insecure HTTP federation enabled")
	}

	registry, err := oci.NewRegistry(db, blobs, identity.ActorURL, cfg.AccountDomain, cfg.ImmutableTags, cfg.Limits.MaxManifestSize, cfg.Limits.MaxBlobSize, logger)
	if err != nil {
		return nil, err
	}

	apPublisher := activitypub.NewAPPublisher(identity, db, cfg.Endpoint, logger)
	apResolver := activitypub.NewAPResolver(db, logger)
	deliveryQueue := activitypub.NewDeliveryQueue(db, identity, logger)
	fetcher := peering.NewFetcher(cfg.Peering.FetchTimeout, cfg.Limits.MaxBlobSize, cfg.Limits.MaxManifestSize, logger)
	healthChecker := peering.NewHealthChecker(db, fetcher, cfg.Peering.HealthCheckInterval, logger)

	blobReplicator := peering.NewBlobReplicator(db, blobs, fetcher, logger)
	gc := peering.NewGarbageCollector(peering.GCConfig{
		Interval:         cfg.GC.Interval,
		StalePeerBlobAge: cfg.GC.StalePeerBlobAge,
		OrphanBatchSize:  cfg.GC.OrphanBatchSize,
	}, db, blobs, logger)

	apPublisher.SetNotifyFunc(deliveryQueue.Notify)

	registry.SetPublisher(apPublisher)
	registry.SetFederation(apResolver, fetcher)

	enqueueFunc := activitypub.EnqueueFunc(func(ctx context.Context, activityID, inboxURL string, activityJSON []byte) error {
		if err := db.EnqueueDelivery(ctx, activityID, inboxURL, activityJSON); err != nil {
			return err
		}
		deliveryQueue.Notify()
		return nil
	})

	inboxHandler := activitypub.NewInboxHandler(identity, db, activitypub.InboxConfig{
		MaxManifestSize: cfg.Limits.MaxManifestSize,
		MaxBlobSize:     cfg.Limits.MaxBlobSize,
		AutoAccept:      cfg.Federation.AutoAccept,
		AllowedDomains:  cfg.Federation.AllowedDomains,
		BlockedDomains:  cfg.Federation.BlockedDomains,
		BlockedActors:   cfg.Federation.BlockedActors,
	}, logger)
	inboxHandler.SetBlobReplicator(blobReplicator)
	inboxHandler.SetEnqueueFunc(enqueueFunc)
	inboxHandler.SetActorCache(apPublisher.ActorCache())

	inboxWorker := activitypub.NewInboxWorker(inboxHandler, logger)
	inboxHandler.SetWorker(inboxWorker)

	inboxLimiter := newIPRateLimiter(10, 50)       // 10 req/sec, burst of 50
	registryPushLimiter := newIPRateLimiter(5, 20) // 5 req/sec, burst of 20 for push ops

	scheduler := workers.NewScheduler()
	scheduler.Add(workers.PeriodicTask{
		Interval: 5 * time.Minute,
		Fn: func(ctx context.Context) {
			if _, err := registry.CleanExpiredUploads(ctx); err != nil {
				logger.Warn("upload session cleanup failed", "error", err)
			}
		},
	})

	services := []workers.Service{healthChecker, scheduler}
	if *cfg.GC.Enabled {
		services = append(services, gc)
	}

	w := &workers.Workers{
		Services:   services,
		Waiters:    []workers.Waiter{blobReplicator},
		Drainables: []workers.Service{inboxWorker, deliveryQueue},
		Cleanup:    []workers.Stoppable{inboxHandler, inboxLimiter, registryPushLimiter, apPublisher},
		Logger:     logger,
	}

	s := &Server{
		cfg:      cfg,
		db:       db,
		identity: identity,
		fedSvc: &federation.Service{
			Fed:      &federation.RealFederator{Identity: identity, Enqueue: enqueueFunc},
			DB:       db,
			ActorURL: identity.ActorURL,
			Logger:   logger,
		},
		registry:            registry,
		workers:             w,
		ociHandler:          registry.Handler(),
		actorHandler:        activitypub.NewActorHandler(identity, cfg.Name, cfg.Endpoint),
		webfingerHandler:    activitypub.NewWebFingerHandler(identity),
		nodeinfoHandler:     activitypub.NewNodeInfoHandler(identity.Domain, version),
		inboxHandler:        inboxHandler,
		outboxHandler:       activitypub.NewOutboxHandler(identity, db),
		followersHandler:    activitypub.NewFollowersHandler(identity, db),
		followingHandler:    activitypub.NewFollowingHandler(identity, db),
		inboxLimiter:        inboxLimiter,
		registryPushLimiter: registryPushLimiter,
		logger:              logger,
	}

	s.httpServer = &http.Server{
		Handler:           s.routes(),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	return s, nil
}

func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.Listen, err)
	}

	s.workers.Start(ctx)

	if s.cfg.Metrics.Enabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
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
	metrics.FederationFollowers.Set(float64(len(follows)))
	metrics.FederationFollowing.Set(float64(len(outgoing)))

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

		// 1. Stop accepting new requests.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.logger.Info("shutting down HTTP server")
		_ = s.httpServer.Shutdown(shutdownCtx)

		// 2. Stop all background workers (ordered phases handled internally).
		s.workers.Stop()
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
