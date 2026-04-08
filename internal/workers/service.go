package workers

import "context"

// Service is a background worker with a Start/Stop lifecycle.
type Service interface {
	Start(ctx context.Context)
	Stop()
}

// Waiter drains in-flight work without a Start/Stop lifecycle.
type Waiter interface {
	Wait()
}

// Stoppable is a resource that only needs cleanup on shutdown.
type Stoppable interface {
	Stop()
}
