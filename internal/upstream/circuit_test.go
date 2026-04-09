package upstream

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCircuitBreaker_InitialState(t *testing.T) {
	cb := newCircuitBreaker()
	require.False(t, cb.isOpen("registry.example.com"))
	require.Equal(t, 0, cb.openCount())
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := newCircuitBreaker()
	registry := "docker.io"

	for range circuitThreshold - 1 {
		opened := cb.recordFailure(registry)
		require.False(t, opened, "circuit should not open before threshold")
		require.False(t, cb.isOpen(registry))
	}

	opened := cb.recordFailure(registry)
	require.True(t, opened, "circuit should open at threshold")
	require.True(t, cb.isOpen(registry))
	require.Equal(t, 1, cb.openCount())
}

func TestCircuitBreaker_SuccessResets(t *testing.T) {
	cb := newCircuitBreaker()
	registry := "ghcr.io"

	for range circuitThreshold - 1 {
		cb.recordFailure(registry)
	}
	cb.recordSuccess(registry)

	for range circuitThreshold - 1 {
		opened := cb.recordFailure(registry)
		require.False(t, opened)
	}
	require.False(t, cb.isOpen(registry))
}

func TestCircuitBreaker_ClosesAfterDuration(t *testing.T) {
	cb := &circuitBreaker{
		failures:  make(map[string]int),
		openUntil: make(map[string]time.Time),
	}
	registry := "quay.io"

	cb.openUntil[registry] = time.Now().Add(10 * time.Millisecond)
	require.True(t, cb.isOpen(registry))

	time.Sleep(20 * time.Millisecond)
	require.False(t, cb.isOpen(registry), "circuit should auto-close after duration")
	require.Equal(t, 0, cb.openCount())
}

func TestCircuitBreaker_MultipleRegistries(t *testing.T) {
	cb := newCircuitBreaker()

	for range circuitThreshold {
		cb.recordFailure("registry1.example.com")
	}

	require.True(t, cb.isOpen("registry1.example.com"))
	require.False(t, cb.isOpen("registry2.example.com"))
	require.Equal(t, 1, cb.openCount())

	for range circuitThreshold {
		cb.recordFailure("registry2.example.com")
	}

	require.True(t, cb.isOpen("registry1.example.com"))
	require.True(t, cb.isOpen("registry2.example.com"))
	require.Equal(t, 2, cb.openCount())
}

func TestCircuitBreaker_RepeatedFailuresAfterOpen(t *testing.T) {
	cb := newCircuitBreaker()
	registry := "docker.io"

	for range circuitThreshold {
		cb.recordFailure(registry)
	}
	require.True(t, cb.isOpen(registry))

	opened := cb.recordFailure(registry)
	require.False(t, opened, "should not report opening again when already open")
	require.True(t, cb.isOpen(registry))
}
