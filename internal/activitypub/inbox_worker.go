package activitypub

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
)

const (
	inboxQueueSize   = 256
	inboxWorkerCount = 1
	inboxTaskTimeout = 10 * time.Second
)

// InboxWorker processes validated ActivityPub activities asynchronously.
type InboxWorker struct {
	handler *InboxHandler
	queue   chan InboxTask
	logger  *slog.Logger
	wg      sync.WaitGroup
	stop    chan struct{}
	once    sync.Once
}

func NewInboxWorker(handler *InboxHandler, logger *slog.Logger) *InboxWorker {
	return &InboxWorker{
		handler: handler,
		queue:   make(chan InboxTask, inboxQueueSize),
		logger:  logger,
		stop:    make(chan struct{}),
	}
}

// Enqueue pushes a task for async processing. Returns false if the queue is full.
func (w *InboxWorker) Enqueue(task InboxTask) bool {
	select {
	case w.queue <- task:
		return true
	default:
		return false
	}
}

func (w *InboxWorker) Start(ctx context.Context) {
	for range inboxWorkerCount {
		w.wg.Add(1)
		go w.run(ctx)
	}
}

// Stop signals the worker to stop and waits for it to finish. Safe to call multiple times.
func (w *InboxWorker) Stop() {
	w.once.Do(func() { close(w.stop) })
	w.wg.Wait()
}

func (w *InboxWorker) run(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case task := <-w.queue:
			w.process(task)
		case <-ctx.Done():
			w.drain()
			return
		case <-w.stop:
			w.drain()
			return
		}
	}
}

func (w *InboxWorker) drain() {
	for {
		select {
		case task := <-w.queue:
			w.process(task)
		default:
			return
		}
	}
}

func (w *InboxWorker) process(task InboxTask) {
	ctx, cancel := context.WithTimeout(context.Background(), inboxTaskTimeout)
	defer cancel()
	start := time.Now()
	dispatchErr := w.handler.dispatch(ctx, task)
	metrics.InboxProcessingDuration.Observe(time.Since(start).Seconds())
	if dispatchErr != nil {
		level := slog.LevelWarn
		if fe, ok := AsFedError(dispatchErr); ok {
			level = fe.LogLevel()
		}
		w.logger.Log(ctx, level, "inbox worker: processing failed",
			"type", task.Activity.Type,
			"id", task.Activity.ID,
			"actor", task.Activity.Actor,
			"error", dispatchErr,
		)
		return
	}
	w.handler.storeActivity(ctx, task.Activity.ID, task.Activity.Type, task.Activity.Actor, task.RawBody)
}
