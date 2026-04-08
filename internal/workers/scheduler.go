package workers

import (
	"context"
	"sync"
	"time"
)

// PeriodicTask defines a recurring background task.
type PeriodicTask struct {
	Interval time.Duration
	Fn       func(ctx context.Context)
}

// Scheduler runs periodic tasks. Add tasks before calling Start.
type Scheduler struct {
	tasks  []PeriodicTask
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

func NewScheduler() *Scheduler {
	return &Scheduler{}
}

func (s *Scheduler) Add(task PeriodicTask) {
	s.tasks = append(s.tasks, task)
}

func (s *Scheduler) Start(ctx context.Context) {
	childCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	for _, task := range s.tasks {
		s.wg.Add(1)
		go s.run(childCtx, task)
	}
}

func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *Scheduler) run(ctx context.Context, task PeriodicTask) {
	defer s.wg.Done()
	ticker := time.NewTicker(task.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			task.Fn(ctx)
		}
	}
}
