package activitypub

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	inboxQueueSize   = 256
	inboxWorkerCount = 4
	inboxTaskTimeout = 10 * time.Second
)

// InboxWorker processes validated ActivityPub activities asynchronously.
type InboxWorker struct {
	handler *InboxHandler
	queue   chan InboxTask
	logger  *slog.Logger
	wg      sync.WaitGroup
	cancel  context.CancelFunc
}

func NewInboxWorker(handler *InboxHandler, logger *slog.Logger) *InboxWorker {
	return &InboxWorker{
		handler: handler,
		queue:   make(chan InboxTask, inboxQueueSize),
		logger:  logger,
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
	childCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	for range inboxWorkerCount {
		w.wg.Add(1)
		go w.run(childCtx)
	}
}

func (w *InboxWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
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
	if err := w.handler.dispatch(ctx, task); err != nil {
		w.logger.Warn("inbox worker: processing failed",
			"type", task.Activity.Type,
			"id", task.Activity.ID,
			"actor", task.Activity.Actor,
			"error", err,
		)
	}
}
