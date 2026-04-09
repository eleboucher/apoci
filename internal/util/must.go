package util

import (
	"log/slog"
	"runtime/debug"
	"time"
)

const (
	initialBackoff = 100 * time.Millisecond
	maxBackoff     = 30 * time.Second
)

// Must wraps fn with panic recovery. On panic, logs the stack trace and restarts
// with exponential backoff (starting at 100ms, capped at 30s).
// Use this to wrap long-running worker loops to ensure they survive unexpected panics.
func Must(logger *slog.Logger, fn func()) {
	backoff := initialBackoff
	for {
		if !mustExec(logger, fn, &backoff) {
			return
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func mustExec(logger *slog.Logger, fn func(), backoff *time.Duration) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("worker panic recovered, restarting",
				"panic", r,
				"backoff", *backoff,
				"stack", string(debug.Stack()),
			)
			panicked = true
		}
	}()
	fn()
	*backoff = initialBackoff
	return false
}
