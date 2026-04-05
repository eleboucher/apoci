package peering

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/apoci/apoci/internal/database"
)

type HealthChecker struct {
	db       *database.DB
	fetcher  *Fetcher
	interval time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func NewHealthChecker(db *database.DB, fetcher *Fetcher, interval time.Duration, logger *slog.Logger) *HealthChecker {
	return &HealthChecker{
		db:       db,
		fetcher:  fetcher,
		interval: interval,
		logger:   logger,
	}
}

func (hc *HealthChecker) Start(ctx context.Context) {
	hc.mu.Lock()
	if hc.running {
		hc.mu.Unlock()
		return
	}
	hc.running = true
	childCtx, cancel := context.WithCancel(ctx)
	hc.cancel = cancel
	hc.wg.Add(1)
	hc.mu.Unlock()

	go hc.loop(childCtx)
}

func (hc *HealthChecker) Stop() {
	hc.mu.Lock()
	if hc.cancel != nil {
		hc.cancel()
		hc.running = false
	}
	hc.mu.Unlock()
	hc.wg.Wait()
}

func (hc *HealthChecker) loop(ctx context.Context) {
	defer hc.wg.Done()
	hc.checkAll(ctx)

	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hc.checkAll(ctx)
		}
	}
}

func (hc *HealthChecker) checkAll(ctx context.Context) {
	peers, err := hc.db.ListAllPeers(ctx)
	if err != nil {
		hc.logger.Error("failed to list peers for health check", "error", err)
		return
	}

	for _, peer := range peers {
		if ctx.Err() != nil {
			return
		}

		err := hc.fetcher.CheckHealth(ctx, peer.Endpoint)
		healthy := err == nil

		if healthy != peer.IsHealthy {
			hc.logger.Info("peer health changed",
				"peer", peer.ActorURL,
				"endpoint", peer.Endpoint,
				"healthy", healthy,
			)
		}

		if err := hc.db.SetPeerHealth(ctx, peer.ActorURL, healthy); err != nil {
			hc.logger.Error("failed to update peer health",
				"peer", peer.ActorURL,
				"error", err,
			)
		}
	}
}
