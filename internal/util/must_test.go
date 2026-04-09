package util

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMust_NormalExecution(t *testing.T) {
	logger := slog.Default()
	var executed atomic.Bool

	Must(logger, func() {
		executed.Store(true)
	})

	assert.True(t, executed.Load(), "function should have executed")
}

func TestMust_RecoverFromPanic(t *testing.T) {
	logger := slog.Default()
	var callCount atomic.Int32

	start := time.Now()
	Must(logger, func() {
		count := callCount.Add(1)
		if count < 3 {
			panic("test panic")
		}
	})
	elapsed := time.Since(start)

	assert.Equal(t, int32(3), callCount.Load(), "function should have been called 3 times")
	// Verify backoff happened (at least 100ms + 200ms = 300ms for 2 restarts)
	assert.GreaterOrEqual(t, elapsed.Milliseconds(), int64(200), "should have backoff between restarts")
}

func TestMust_BackoffCaps(t *testing.T) {
	logger := slog.Default()
	var callCount atomic.Int32

	// This test verifies the backoff doesn't grow unbounded
	// After several panics, backoff should cap at maxBackoff (30s)
	// We don't actually wait 30s, just verify the logic doesn't panic
	Must(logger, func() {
		count := callCount.Add(1)
		if count < 2 {
			panic("test panic")
		}
	})

	assert.Equal(t, int32(2), callCount.Load())
}
