package workers

import (
	"context"
	"log/slog"
)

// Workers manages all background work with ordered shutdown.
type Workers struct {
	Services   []Service
	Waiters    []Waiter
	Drainables []Service
	Cleanup    []Stoppable
	Logger     *slog.Logger
}

func (w *Workers) Start(ctx context.Context) {
	for _, svc := range w.Services {
		svc.Start(ctx)
	}
	for _, svc := range w.Drainables {
		svc.Start(ctx)
	}
}

// Stop shuts down all workers in four ordered phases:
// services, waiters, drainables, cleanup.
func (w *Workers) Stop() {
	w.Logger.Info("stopping background services")
	for _, svc := range w.Services {
		svc.Stop()
	}

	w.Logger.Info("waiting for in-flight work to drain")
	for _, waiter := range w.Waiters {
		waiter.Wait()
	}

	w.Logger.Info("stopping drainable services")
	for _, svc := range w.Drainables {
		svc.Stop()
	}

	w.Logger.Info("cleaning up caches and resources")
	for _, c := range w.Cleanup {
		c.Stop()
	}
}
