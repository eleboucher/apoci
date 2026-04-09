package upstream

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// isOpenBool is a test helper that returns only the open bool from isOpen.
func isOpenBool(cb *circuitBreaker, registry string) bool {
	open, _ := cb.isOpen(registry)
	return open
}

func TestCircuitBreaker_InitialState(t *testing.T) {
	cb := newCircuitBreaker()
	require.False(t, isOpenBool(cb, "registry.example.com"))
	require.Equal(t, 0, cb.openCount())
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := newCircuitBreaker()
	registry := "docker.io"

	// Record failures up to threshold - 1
	for range circuitThreshold - 1 {
		opened := cb.recordFailure(registry)
		require.False(t, opened, "circuit should not open before threshold")
		require.False(t, isOpenBool(cb, registry))
	}

	// One more failure should open the circuit
	opened := cb.recordFailure(registry)
	require.True(t, opened, "circuit should open at threshold")
	require.True(t, isOpenBool(cb, registry))
	require.Equal(t, 1, cb.openCount())
}

func TestCircuitBreaker_SuccessResets(t *testing.T) {
	cb := newCircuitBreaker()
	registry := "ghcr.io"

	// Build up some failures
	for range circuitThreshold - 1 {
		cb.recordFailure(registry)
	}

	// Success should reset the count
	cb.recordSuccess(registry)

	// Now we need threshold failures again to open
	for range circuitThreshold - 1 {
		opened := cb.recordFailure(registry)
		require.False(t, opened)
	}
	require.False(t, isOpenBool(cb, registry))
}

func TestCircuitBreaker_ClosesAfterDuration(t *testing.T) {
	// Create a circuit breaker and manually set a short open duration for testing
	cb := &circuitBreaker{
		failures:  make(map[string]int),
		openUntil: make(map[string]time.Time),
	}
	registry := "quay.io"

	// Open the circuit with a very short duration
	cb.openUntil[registry] = time.Now().Add(10 * time.Millisecond)
	open, expired := cb.isOpen(registry)
	require.True(t, open)
	require.False(t, expired)

	// Wait for it to close
	time.Sleep(20 * time.Millisecond)
	open, expired = cb.isOpen(registry)
	require.False(t, open)
	require.True(t, expired, "expired should be true when the circuit auto-closes")
	require.Equal(t, 0, cb.openCount())
}

func TestCircuitBreaker_MultipleRegistries(t *testing.T) {
	cb := newCircuitBreaker()

	// Open circuit for registry1
	for range circuitThreshold {
		cb.recordFailure("registry1.example.com")
	}

	// registry2 should still be closed
	require.True(t, isOpenBool(cb, "registry1.example.com"))
	require.False(t, isOpenBool(cb, "registry2.example.com"))
	require.Equal(t, 1, cb.openCount())

	// Open circuit for registry2
	for range circuitThreshold {
		cb.recordFailure("registry2.example.com")
	}

	require.True(t, isOpenBool(cb, "registry1.example.com"))
	require.True(t, isOpenBool(cb, "registry2.example.com"))
	require.Equal(t, 2, cb.openCount())
}

func TestCircuitBreaker_RepeatedFailuresAfterOpen(t *testing.T) {
	cb := newCircuitBreaker()
	registry := "docker.io"

	// Open the circuit
	for range circuitThreshold {
		cb.recordFailure(registry)
	}
	require.True(t, isOpenBool(cb, registry))

	// Additional failures should not return true again (already open)
	opened := cb.recordFailure(registry)
	require.False(t, opened, "should not report opening again when already open")
	require.True(t, isOpenBool(cb, registry))
}
