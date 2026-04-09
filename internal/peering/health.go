package peering

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
)

// PeerRecord is the minimal peer information needed for health checks.
type PeerRecord struct {
	ActorURL  string
	Endpoint  string
	IsHealthy bool
}

type HealthRepository interface {
	ListAllPeers(ctx context.Context) ([]PeerRecord, error)
	SetPeerHealth(ctx context.Context, actorURL string, healthy bool) error
}

type HealthFetcher interface {
	CheckHealth(ctx context.Context, endpoint string) error
}

type HealthChecker struct {
	db       HealthRepository
	fetcher  HealthFetcher
	interval time.Duration
	notifier Notifier
	logger   *slog.Logger

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func NewHealthChecker(db HealthRepository, fetcher HealthFetcher, interval time.Duration, notifier Notifier, logger *slog.Logger) *HealthChecker {
	return &HealthChecker{
		db:       db,
		fetcher:  fetcher,
		interval: interval,
		notifier: notifier,
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
		hc.cancel = nil
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
			if healthy {
				hc.notifier.Send(notify.EventPeerHealth, fmt.Sprintf("Peer %s is back online", peer.Endpoint))
			} else {
				hc.notifier.Send(notify.EventPeerHealth, fmt.Sprintf("Peer %s is unreachable", peer.Endpoint))
			}
		}

		if err := hc.db.SetPeerHealth(ctx, peer.ActorURL, healthy); err != nil {
			hc.logger.Error("failed to update peer health",
				"peer", peer.ActorURL,
				"error", err,
			)
		}
	}
}
