package notify

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/nicholas-fedor/shoutrrr/pkg/router"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"
)

// Event constants for notification categories.
const (
	EventPeerHealth         = "peer_health"
	EventFollowRequest      = "follow_request"
	EventReplicationFailure = "replication_failure"
	EventGCError            = "gc_error"
)

// ValidEvents is the set of recognized event names.
var ValidEvents = map[string]bool{
	EventPeerHealth:         true,
	EventFollowRequest:      true,
	EventReplicationFailure: true,
	EventGCError:            true,
}

const queueSize = 64

// Notifier sends best-effort notifications via shoutrrr.
// It is safe for concurrent use. A zero-value or nil sender means no-op.
type Notifier struct {
	sender   *router.ServiceRouter
	events   map[string]struct{}
	name     string
	logger   *slog.Logger
	queue    chan string
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New creates a Notifier. If urls is empty or sender creation fails, the
// returned Notifier is a no-op (Send returns immediately).
func New(name string, urls []string, events []string, logger *slog.Logger) *Notifier {
	n := &Notifier{
		name:   name,
		logger: logger,
		events: make(map[string]struct{}, len(events)),
	}

	for _, e := range events {
		n.events[e] = struct{}{}
	}

	if len(urls) == 0 {
		return n
	}

	sender, err := shoutrrr.CreateSenderWithOptions(types.SenderOptions{}, urls...)
	if err != nil {
		logger.Error("failed to create notification sender, notifications disabled", "error", err)
		return n
	}

	n.sender = sender
	n.queue = make(chan string, queueSize)
	n.stop = make(chan struct{})
	n.wg.Add(1)
	go n.drain()

	return n
}

// Send enqueues a notification if the event is enabled.
// It never blocks the caller; messages are dropped if the queue is full.
func (n *Notifier) Send(event, text string) {
	if n.sender == nil {
		return
	}
	if _, ok := n.events[event]; !ok {
		return
	}

	msg := fmt.Sprintf("[%s] %s", n.name, text)

	select {
	case n.queue <- msg:
	case <-n.stop:
	default:
		n.logger.Warn("notification queue full, dropping message", "event", event)
	}
}

// Stop drains the notification queue and waits for pending sends.
// Implements workers.Stoppable. The queue is never closed, so a concurrent
// Send after Stop is a no-op rather than a send-on-closed-channel panic.
func (n *Notifier) Stop() {
	if n.queue == nil {
		return
	}
	n.stopOnce.Do(func() { close(n.stop) })
	n.wg.Wait()
}

func (n *Notifier) drain() {
	defer n.wg.Done()
	for {
		select {
		case msg := <-n.queue:
			n.dispatch(msg)
		case <-n.stop:
			// Flush whatever is already buffered, then exit.
			for {
				select {
				case msg := <-n.queue:
					n.dispatch(msg)
				default:
					return
				}
			}
		}
	}
}

func (n *Notifier) dispatch(msg string) {
	for _, err := range n.sender.Send(msg, &types.Params{}) {
		if err != nil {
			n.logger.Warn("notification send failed", "error", err)
		}
	}
}
