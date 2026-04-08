package activitypub

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
)

const (
	deliveryPollInterval = 5 * time.Second
	deliveryBatchSize    = 50
	deliveryCleanupAge   = 7 * 24 * time.Hour // 1 week
)

// DeliveryQueue processes pending activity deliveries with retry and backoff.
type DeliveryQueue struct {
	db       *database.DB
	identity *Identity
	logger   *slog.Logger
	wg       sync.WaitGroup
	stop     chan struct{}
	notify   chan struct{}
	once     sync.Once
}

func NewDeliveryQueue(db *database.DB, identity *Identity, logger *slog.Logger) *DeliveryQueue {
	return &DeliveryQueue{
		db:       db,
		identity: identity,
		logger:   logger,
		stop:     make(chan struct{}),
		notify:   make(chan struct{}, 1),
	}
}

// Notify wakes up the delivery loop to process newly enqueued deliveries immediately.
func (q *DeliveryQueue) Notify() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *DeliveryQueue) Start(ctx context.Context) {
	q.wg.Add(1)
	go q.run(ctx)
}

// Stop signals the worker to stop and waits for it to finish. Safe to call multiple times.
func (q *DeliveryQueue) Stop() {
	q.once.Do(func() { close(q.stop) })
	q.wg.Wait()
}

func (q *DeliveryQueue) run(ctx context.Context) {
	defer q.wg.Done()

	ticker := time.NewTicker(deliveryPollInterval)
	defer ticker.Stop()

	cleanupTicker := time.NewTicker(1 * time.Hour)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			q.drainRemaining()
			return
		case <-q.stop:
			q.drainRemaining()
			return
		case <-q.notify:
			q.processBatch(ctx)
		case <-ticker.C:
			q.processBatch(ctx)
		case <-cleanupTicker.C:
			q.cleanup(ctx)
		}
	}
}

func (q *DeliveryQueue) processBatch(ctx context.Context) {
	deliveries, err := q.db.PendingDeliveries(ctx, deliveryBatchSize)
	if err != nil {
		q.logger.Error("failed to fetch pending deliveries", "error", err)
		return
	}

	metrics.DeliveryPending.Set(float64(len(deliveries)))

	if len(deliveries) == 0 {
		return
	}

	g := new(errgroup.Group)
	g.SetLimit(maxConcurrentDeliveries)

	for _, d := range deliveries {
		g.Go(func() error {
			q.deliver(ctx, d)
			return nil
		})
	}

	err = g.Wait()
	if err != nil {
		q.logger.Error("error processing deliveries", "error", err)
	}
}

func (q *DeliveryQueue) deliver(ctx context.Context, d database.Delivery) {
	if err := DeliverActivity(ctx, d.InboxURL, d.ActivityJSON, q.identity); err != nil {
		metrics.DeliveryRetries.Add(1)
		q.logger.Warn("delivery failed",
			"inbox", d.InboxURL,
			"attempt", d.Attempts+1,
			"max", d.MaxAttempts,
			"error", err,
		)
		if dbErr := q.db.MarkDeliveryFailed(ctx, d.ID, d.Attempts, d.MaxAttempts, err.Error()); dbErr != nil {
			q.logger.Error("failed to mark delivery failed", "error", dbErr)
		}
		if d.Attempts+1 >= d.MaxAttempts {
			metrics.DeliveryFailed.Add(1)
		}
		return
	}

	metrics.DeliverySucceeded.Add(1)
	if err := q.db.MarkDelivered(ctx, d.ID); err != nil {
		q.logger.Error("failed to mark delivery delivered", "error", err)
	} else {
		q.logger.Debug("delivered activity", "inbox", d.InboxURL, "activity", d.ActivityID)
	}
}

// drainRemaining makes one final attempt to process any pending deliveries on shutdown.
func (q *DeliveryQueue) drainRemaining() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q.processBatch(ctx)
}

func (q *DeliveryQueue) cleanup(ctx context.Context) {
	n, err := q.db.CleanupDeliveries(ctx, deliveryCleanupAge)
	if err != nil {
		q.logger.Error("failed to cleanup deliveries", "error", err)
		return
	}
	if n > 0 {
		q.logger.Info("cleaned up old deliveries", "count", n)
	}
}
