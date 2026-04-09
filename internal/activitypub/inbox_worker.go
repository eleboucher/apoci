package activitypub

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"codeberg.org/gruf/go-runners"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/queue"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/util"
)

const (
	inboxQueueSize   = 256
	inboxTaskTimeout = 10 * time.Second
)

// inboxWorkerCount returns the number of inbox workers based on GOMAXPROCS.
func inboxWorkerCount() int {
	n := runtime.GOMAXPROCS(0) * 4
	if n < 1 {
		return 1
	}
	return n
}

// InboxDispatcher processes incoming ActivityPub tasks.
type InboxDispatcher interface {
	dispatch(ctx context.Context, task InboxTask) error
	storeActivity(ctx context.Context, activityID, activityType, actorURL string, activityJSON []byte)
}

// InboxWorker processes validated ActivityPub activities asynchronously.
type InboxWorker struct {
	handler  InboxDispatcher
	queue    *queue.SimpleQueue[InboxTask]
	logger   *slog.Logger
	services []runners.Service
	workerWg sync.WaitGroup
}

func NewInboxWorker(handler InboxDispatcher, logger *slog.Logger) *InboxWorker {
	return &InboxWorker{
		handler: handler,
		queue:   queue.NewSimpleQueue[InboxTask](inboxQueueSize),
		logger:  logger,
	}
}

// Enqueue pushes a task for async processing. Returns false if the queue is full.
func (w *InboxWorker) Enqueue(task InboxTask) bool {
	return w.queue.TryPush(task, inboxQueueSize)
}

func (w *InboxWorker) Start(ctx context.Context) {
	n := inboxWorkerCount()
	w.services = make([]runners.Service, n)
	for i := range n {
		w.workerWg.Add(1)
		w.services[i].GoRun(func(svcCtx context.Context) {
			defer w.workerWg.Done()
			w.run(ctx, svcCtx)
		})
	}
	w.logger.Info("started inbox workers", "count", n)
}

func (w *InboxWorker) Stop() {
	for i := range w.services {
		w.services[i].Stop()
	}
	w.workerWg.Wait()
	w.drain()
}

func (w *InboxWorker) run(parentCtx, svcCtx context.Context) {
	util.Must(w.logger, func() {
		for {
			select {
			case <-svcCtx.Done():
				return
			case <-parentCtx.Done():
				return
			default:
			}

			task, ok := w.queue.PopCtx(svcCtx)
			if !ok {
				return
			}
			w.process(task)
		}
	})
}

func (w *InboxWorker) drain() {
	for {
		task, ok := w.queue.Pop()
		if !ok {
			return
		}
		w.process(task)
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
