package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSimpleQueue_PushPop(t *testing.T) {
	q := NewSimpleQueue[int](10)

	q.Push(1)
	q.Push(2)
	q.Push(3)

	assert.Equal(t, 3, q.Len())

	v, ok := q.Pop()
	assert.True(t, ok)
	assert.Equal(t, 1, v)

	v, ok = q.Pop()
	assert.True(t, ok)
	assert.Equal(t, 2, v)

	v, ok = q.Pop()
	assert.True(t, ok)
	assert.Equal(t, 3, v)

	_, ok = q.Pop()
	assert.False(t, ok)
}

func TestSimpleQueue_TryPush(t *testing.T) {
	q := NewSimpleQueue[int](2)

	assert.True(t, q.TryPush(1, 2))
	assert.True(t, q.TryPush(2, 2))
	assert.False(t, q.TryPush(3, 2))

	assert.Equal(t, 2, q.Len())
}

func TestSimpleQueue_PopCtx_Success(t *testing.T) {
	q := NewSimpleQueue[int](10)
	ctx := context.Background()

	go func() {
		time.Sleep(10 * time.Millisecond)
		q.Push(42)
	}()

	v, ok := q.PopCtx(ctx)
	assert.True(t, ok)
	assert.Equal(t, 42, v)
}

func TestSimpleQueue_PopCtx_ContextCancelled(t *testing.T) {
	q := NewSimpleQueue[int](10)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, ok := q.PopCtx(ctx)
	assert.False(t, ok)
}

func TestSimpleQueue_PopCtx_ImmediateReturn(t *testing.T) {
	q := NewSimpleQueue[int](10)
	q.Push(1)

	ctx := context.Background()
	v, ok := q.PopCtx(ctx)
	assert.True(t, ok)
	assert.Equal(t, 1, v)
}

func TestSimpleQueue_Drain(t *testing.T) {
	q := NewSimpleQueue[int](10)
	q.Push(1)
	q.Push(2)
	q.Push(3)

	items := q.Drain()
	assert.Equal(t, []int{1, 2, 3}, items)
	assert.Equal(t, 0, q.Len())
}

func TestSimpleQueue_PopCtx_PreCancelledContext(t *testing.T) {
	q := NewSimpleQueue[int](10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before calling PopCtx

	_, ok := q.PopCtx(ctx)
	assert.False(t, ok, "should return false for pre-cancelled context")
}

func TestSimpleQueue_DrainEmpty(t *testing.T) {
	q := NewSimpleQueue[int](10)

	items := q.Drain()
	assert.Empty(t, items)
	assert.Equal(t, 0, q.Len())
}

func TestSimpleQueue_TryPushZeroMax(t *testing.T) {
	q := NewSimpleQueue[int](10)

	ok := q.TryPush(1, 0)
	assert.False(t, ok, "should fail when maxSize is 0")
}

func TestSimpleQueue_Concurrent(t *testing.T) {
	q := NewSimpleQueue[int](100)
	var wg sync.WaitGroup

	// Multiple producers
	for i := range 10 {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := range 10 {
				q.Push(base*10 + j)
			}
		}(i)
	}

	// Multiple consumers
	var received sync.Map
	for range 5 {
		wg.Go(func() {
			for range 20 {
				if v, ok := q.Pop(); ok {
					received.Store(v, true)
				}
			}
		})
	}

	wg.Wait()

	// Allow some items to remain in queue
	remaining := q.Drain()
	count := len(remaining)
	received.Range(func(_, _ any) bool {
		count++
		return true
	})

	require.Equal(t, 100, count, "all items should be accounted for")
}
