package activitypub

import (
	"context"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
)

const (
	deliveryPollInterval = 5 * time.Second
	deliveryBatchSize    = 50
	deliveryCleanupAge   = 7 * 24 * time.Hour // 1 week

	circuitBreakerThreshold = 5         // consecutive failures before opening the circuit
	circuitOpenDuration     = time.Hour // how long the circuit stays open
)

// deliveryCircuitBreaker tracks consecutive delivery failures per peer domain
// and fast-fails deliveries to domains that have exceeded the failure threshold.
type deliveryCircuitBreaker struct {
	mu        sync.Mutex
	failures  map[string]int
	openUntil map[string]time.Time
}

func newDeliveryCircuitBreaker() *deliveryCircuitBreaker {
	return &deliveryCircuitBreaker{
		failures:  make(map[string]int),
		openUntil: make(map[string]time.Time),
	}
}

func (cb *deliveryCircuitBreaker) isOpen(domain string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	t, ok := cb.openUntil[domain]
	if !ok {
		return false
	}
	if time.Now().After(t) {
		delete(cb.openUntil, domain)
		delete(cb.failures, domain)
		return false
	}
	return true
}

func (cb *deliveryCircuitBreaker) recordSuccess(domain string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	delete(cb.failures, domain)
	delete(cb.openUntil, domain)
}

// recordFailure increments the failure count and opens the circuit if the
// threshold is reached. Returns true the first time the circuit opens.
func (cb *deliveryCircuitBreaker) recordFailure(domain string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures[domain]++
	if cb.failures[domain] >= circuitBreakerThreshold {
		if _, alreadyOpen := cb.openUntil[domain]; !alreadyOpen {
			cb.openUntil[domain] = time.Now().Add(circuitOpenDuration)
			return true
		}
	}
	return false
}

func (cb *deliveryCircuitBreaker) openCount() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := time.Now()
	count := 0
	for _, t := range cb.openUntil {
		if now.Before(t) {
			count++
		}
	}
	return count
}

func (cb *deliveryCircuitBreaker) forceOpen(domain string, duration time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.openUntil[domain] = time.Now().Add(duration)
}

type DeliveryRepository interface {
	PendingDeliveries(ctx context.Context, limit int) ([]database.Delivery, error)
	MarkDeliveryFailed(ctx context.Context, id int64, attempts, maxAttempts int, lastError string) error
	MarkDelivered(ctx context.Context, id int64) error
	CleanupDeliveries(ctx context.Context, olderThan time.Duration) (int64, error)
	SetPeerHealthByDomain(ctx context.Context, domain string, healthy bool) error
	UnhealthyPeerDomains(ctx context.Context) ([]string, error)
}

type DeliveryQueue struct {
	db       DeliveryRepository
	identity *Identity
	logger   *slog.Logger
	circuit  *deliveryCircuitBreaker
	wg       sync.WaitGroup
	stop     chan struct{}
	notify   chan struct{}
	once     sync.Once
}

func NewDeliveryQueue(db DeliveryRepository, identity *Identity, logger *slog.Logger) *DeliveryQueue {
	return &DeliveryQueue{
		db:       db,
		identity: identity,
		logger:   logger,
		circuit:  newDeliveryCircuitBreaker(),
		stop:     make(chan struct{}),
		notify:   make(chan struct{}, 1),
	}
}

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

// PreWarmCircuit loads unhealthy peer domains from the DB and opens the
// circuit breaker for each one. Call this once before Start to ensure a
// restart during an outage doesn't immediately retry all dead peers.
func (q *DeliveryQueue) PreWarmCircuit(ctx context.Context) {
	domains, err := q.db.UnhealthyPeerDomains(ctx)
	if err != nil {
		q.logger.Warn("failed to load unhealthy peers for circuit pre-warm", "error", err)
		return
	}
	for _, d := range domains {
		q.circuit.forceOpen(d, circuitOpenDuration)
	}
	if len(domains) > 0 {
		q.logger.Info("pre-warmed circuit breaker from DB", "domains", len(domains))
		metrics.DeliveryCircuitOpen.Set(float64(q.circuit.openCount()))
	}
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

	cleanupTicker := time.NewTicker(time.Hour)
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

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentDeliveries)

	for _, d := range deliveries {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			q.deliver(ctx, d)
		}()
	}

	wg.Wait()
}

func (q *DeliveryQueue) deliver(ctx context.Context, d database.Delivery) {
	domain := inboxDomain(d.InboxURL)

	if domain != "" && q.circuit.isOpen(domain) {
		metrics.DeliveryRetries.Add(1)
		q.logger.Debug("circuit open: skipping delivery",
			"inbox", d.InboxURL,
			"domain", domain,
		)
		if dbErr := q.db.MarkDeliveryFailed(ctx, d.ID, d.Attempts, d.MaxAttempts, "circuit open"); dbErr != nil {
			q.logger.Error("failed to mark circuit-skipped delivery failed", "error", dbErr)
		}
		if d.Attempts+1 >= d.MaxAttempts {
			metrics.DeliveryFailed.WithLabelValues(domainLabel(domain)).Add(1)
		}
		return
	}

	start := time.Now()
	err := DeliverActivity(ctx, d.InboxURL, d.ActivityJSON, q.identity)
	metrics.DeliveryDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.DeliveryRetries.Add(1)
		q.logger.Warn("delivery failed",
			"inbox", d.InboxURL,
			"attempt", d.Attempts+1,
			"max", d.MaxAttempts,
			"error", err,
		)
		if domain != "" {
			if opened := q.circuit.recordFailure(domain); opened {
				q.logger.Warn("circuit opened for peer domain",
					"domain", domain,
					"threshold", circuitBreakerThreshold,
				)
				metrics.DeliveryCircuitOpen.Set(float64(q.circuit.openCount()))
				if dbErr := q.db.SetPeerHealthByDomain(ctx, domain, false); dbErr != nil {
					q.logger.Warn("failed to persist circuit open state", "domain", domain, "error", dbErr)
				}
			}
		}
		if dbErr := q.db.MarkDeliveryFailed(ctx, d.ID, d.Attempts, d.MaxAttempts, err.Error()); dbErr != nil {
			q.logger.Error("failed to mark delivery failed", "error", dbErr)
		}
		if d.Attempts+1 >= d.MaxAttempts {
			metrics.DeliveryFailed.WithLabelValues(domainLabel(domain)).Add(1)
		}
		return
	}

	if domain != "" {
		wasClosed := q.circuit.isOpen(domain)
		q.circuit.recordSuccess(domain)
		metrics.DeliveryCircuitOpen.Set(float64(q.circuit.openCount()))
		if wasClosed {
			if dbErr := q.db.SetPeerHealthByDomain(ctx, domain, true); dbErr != nil {
				q.logger.Warn("failed to persist circuit close state", "domain", domain, "error", dbErr)
			}
		}
	}
	metrics.DeliverySucceeded.WithLabelValues(domainLabel(domain)).Add(1)
	if err := q.db.MarkDelivered(ctx, d.ID); err != nil {
		q.logger.Error("failed to mark delivery delivered", "error", err)
	} else {
		q.logger.Debug("delivered activity", "inbox", d.InboxURL, "activity", d.ActivityID)
	}
}

// domainLabel returns the domain for use as a Prometheus label value,
// falling back to "unknown" when the domain cannot be extracted.
func domainLabel(domain string) string {
	if domain == "" {
		return "unknown"
	}
	return domain
}

// inboxDomain extracts the hostname from an inbox URL for circuit-breaker keying.
func inboxDomain(inboxURL string) string {
	u, err := url.Parse(inboxURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
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
