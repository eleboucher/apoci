package workers

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type mockService struct {
	started atomic.Bool
	stopped atomic.Bool
}

func (m *mockService) Start(_ context.Context) { m.started.Store(true) }
func (m *mockService) Stop()                   { m.stopped.Store(true) }

type mockWaiter struct {
	waited atomic.Bool
}

func (m *mockWaiter) Wait() { m.waited.Store(true) }

type mockStoppable struct {
	stopped atomic.Bool
}

func (m *mockStoppable) Stop() { m.stopped.Store(true) }

func TestWorkersStartAndStop(t *testing.T) {
	svc := &mockService{}
	drain := &mockService{}
	waiter := &mockWaiter{}
	cleanup := &mockStoppable{}

	w := &Workers{
		Services:   []Service{svc},
		Waiters:    []Waiter{waiter},
		Drainables: []Service{drain},
		Cleanup:    []Stoppable{cleanup},
		Logger:     nopLog(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	assert.True(t, svc.started.Load(), "service should be started")
	assert.True(t, drain.started.Load(), "drainable should be started")

	cancel()
	w.Stop()

	assert.True(t, svc.stopped.Load(), "service should be stopped")
	assert.True(t, waiter.waited.Load(), "waiter should be waited")
	assert.True(t, drain.stopped.Load(), "drainable should be stopped")
	assert.True(t, cleanup.stopped.Load(), "cleanup should be stopped")
}

func TestSchedulerRunsTasks(t *testing.T) {
	var count atomic.Int32

	s := NewScheduler()
	s.Add(PeriodicTask{
		Interval: 10 * time.Millisecond,
		Fn: func(_ context.Context) {
			count.Add(1)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	require.Eventually(t, func() bool {
		return count.Load() >= 3
	}, 500*time.Millisecond, 5*time.Millisecond, "task should run at least 3 times")

	cancel()
	s.Stop()
}

func TestSchedulerStopsCleanly(t *testing.T) {
	s := NewScheduler()
	s.Add(PeriodicTask{
		Interval: 1 * time.Hour,
		Fn:       func(_ context.Context) {},
	})

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return in time")
	}
}

func TestSchedulerNoTasks(t *testing.T) {
	s := NewScheduler()

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	cancel()
	s.Stop()
}
