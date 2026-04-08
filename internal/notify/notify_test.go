package notify

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestNoopNotifierDoesNotPanic(t *testing.T) {
	n := New("test", nil, nil, nopLog())
	n.Send(EventPeerHealth, "should be a no-op")
	n.Stop()
}

func TestEventFiltering(t *testing.T) {
	// Sender is nil (no URLs), so nothing can actually send,
	// but we verify the event set is built correctly.
	n := New("test", nil, []string{EventPeerHealth, EventGCError}, nopLog())
	require.Contains(t, n.events, EventPeerHealth)
	require.Contains(t, n.events, EventGCError)
	require.NotContains(t, n.events, EventFollowRequest)
	require.NotContains(t, n.events, EventReplicationFailure)
}

func TestValidEvents(t *testing.T) {
	require.True(t, ValidEvents[EventPeerHealth])
	require.True(t, ValidEvents[EventFollowRequest])
	require.True(t, ValidEvents[EventReplicationFailure])
	require.True(t, ValidEvents[EventGCError])
	require.False(t, ValidEvents["unknown_event"])
}

func TestStopWithoutStartIsNoop(t *testing.T) {
	n := &Notifier{logger: nopLog()}
	n.Stop() // should not panic
}
