package workers

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"codeberg.org/gruf/go-runners"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/util"
)

// PeriodicTask defines a recurring background task.
type PeriodicTask struct {
	Name     string
	Interval time.Duration
	Fn       func(ctx context.Context)
}

// Scheduler runs periodic tasks. Add tasks before calling Start.
type Scheduler struct {
	tasks   []PeriodicTask
	logger  *slog.Logger
	service runners.Service
	taskWg  sync.WaitGroup
}

func NewScheduler(logger *slog.Logger) *Scheduler {
	return &Scheduler{
		logger: logger,
	}
}

func (s *Scheduler) Add(task PeriodicTask) {
	s.tasks = append(s.tasks, task)
}

func (s *Scheduler) Start(ctx context.Context) {
	s.service.GoRun(func(svcCtx context.Context) {
		mergedCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		go func() {
			select {
			case <-svcCtx.Done():
				cancel()
			case <-mergedCtx.Done():
			}
		}()

		for _, task := range s.tasks {
			s.taskWg.Add(1)
			go s.run(mergedCtx, task)
		}

		<-mergedCtx.Done()
		s.taskWg.Wait()
	})
}

func (s *Scheduler) Stop() {
	s.service.Stop()
}

func (s *Scheduler) run(ctx context.Context, task PeriodicTask) {
	defer s.taskWg.Done()
	util.Must(s.logger.With("task", task.Name), func() {
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
	})
}
