package queue

import (
	"context"
	"sync"
)

// SimpleQueue provides a thread-safe queue with blocking pop operations.
type SimpleQueue[T any] struct {
	mu    sync.Mutex
	cond  *sync.Cond
	items []T
}

// NewSimpleQueue creates a new SimpleQueue with the given initial capacity.
func NewSimpleQueue[T any](capacity int) *SimpleQueue[T] {
	q := &SimpleQueue[T]{
		items: make([]T, 0, capacity),
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Push adds an item to the back of the queue and signals any waiting consumers.
func (q *SimpleQueue[T]) Push(item T) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.cond.Signal()
	q.mu.Unlock()
}

// TryPush attempts to add an item if the queue has capacity.
// Returns false if the queue is at capacity.
func (q *SimpleQueue[T]) TryPush(item T, maxSize int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= maxSize {
		return false
	}
	q.items = append(q.items, item)
	q.cond.Signal()
	return true
}

// Pop removes and returns the front item. Returns false if the queue is empty.
func (q *SimpleQueue[T]) Pop() (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		var zero T
		return zero, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, true
}

// PopCtx blocks until an item is available or the context is cancelled.
func (q *SimpleQueue[T]) PopCtx(ctx context.Context) (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.cond.Broadcast()
		case <-done:
		}
	}()
	defer close(done)

	for len(q.items) == 0 {
		if ctx.Err() != nil {
			var zero T
			return zero, false
		}
		q.cond.Wait()
	}

	if ctx.Err() != nil {
		var zero T
		return zero, false
	}

	item := q.items[0]
	q.items = q.items[1:]
	return item, true
}

// Len returns the current number of items in the queue.
func (q *SimpleQueue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Drain removes and returns all items from the queue.
func (q *SimpleQueue[T]) Drain() []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = make([]T, 0, cap(q.items))
	return items
}
