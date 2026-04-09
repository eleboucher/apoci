package upstream

import (
	"sync"
	"time"
)

const (
	circuitThreshold    = 5               // consecutive failures before opening
	circuitOpenDuration = 5 * time.Minute // shorter than delivery since upstream failures recover faster
)

// circuitBreaker tracks consecutive fetch failures per upstream registry
// and fast-fails requests to registries that have exceeded the failure threshold.
type circuitBreaker struct {
	mu        sync.Mutex
	failures  map[string]int
	openUntil map[string]time.Time
}

func newCircuitBreaker() *circuitBreaker {
	return &circuitBreaker{
		failures:  make(map[string]int),
		openUntil: make(map[string]time.Time),
	}
}

// isOpen reports whether the circuit is currently open for the given registry.
// Expired circuits are cleaned up inline and treated as closed.
func (cb *circuitBreaker) isOpen(registry string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	t, ok := cb.openUntil[registry]
	if !ok {
		return false
	}
	if time.Now().After(t) {
		delete(cb.openUntil, registry)
		delete(cb.failures, registry)
		return false
	}
	return true
}

// recordSuccess resets the failure count and closes the circuit if open.
// Returns true if the circuit was open and is now closed.
func (cb *circuitBreaker) recordSuccess(registry string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	_, wasOpen := cb.openUntil[registry]
	delete(cb.failures, registry)
	delete(cb.openUntil, registry)
	return wasOpen
}

// recordFailure increments the failure count and opens the circuit if the
// threshold is reached. Returns true the first time the circuit opens.
func (cb *circuitBreaker) recordFailure(registry string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures[registry]++
	if cb.failures[registry] >= circuitThreshold {
		if _, alreadyOpen := cb.openUntil[registry]; !alreadyOpen {
			cb.openUntil[registry] = time.Now().Add(circuitOpenDuration)
			return true
		}
	}
	return false
}

func (cb *circuitBreaker) openCount() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := time.Now()
	count := 0
	for _, t := range cb.openUntil {
		if now.Before(t) {
			count++
		}
	}
	return count
}
