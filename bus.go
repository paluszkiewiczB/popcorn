package popcorn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paluszkiewiczB/popcorn/internal"
	"github.com/paluszkiewiczB/popcorn/plog"
	"github.com/paluszkiewiczB/popcorn/plog/attr"
)

// Bus is an in-process event bus that delivers events to registered listeners.
type Bus struct {
	mu        sync.RWMutex
	listeners map[string]chan<- Event
	timeout   time.Duration
	log       plog.Logger

	shouldBuf atomic.Bool
	bufMu     sync.Mutex
	buf       []Event
}

type busConfig struct {
	timeout time.Duration
	log     plog.Logger
}

// BusOption configures a [Bus].
type BusOption func(*busConfig) error

// WithSendTimeout sets the per-listener send timeout.
func WithSendTimeout(d time.Duration) BusOption {
	return func(c *busConfig) error {
		c.timeout = d
		return nil
	}
}

//CR: shouldn't it accept `plog.Logger`?

// WithBusLogger sets the logger used by the bus.
func WithBusLogger(l *slog.Logger) BusOption {
	return func(c *busConfig) error {
		c.log = l
		return nil
	}
}

// NewBus creates a new [Bus] with the given options.
func NewBus(opts ...BusOption) (*Bus, error) {
	cfg := busConfig{
		timeout: time.Second,
		log:     slog.Default(),
	}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, fmt.Errorf("applying bus option: %w", err)
		}
	}
	return &Bus{
		listeners: make(map[string]chan<- Event),
		timeout:   cfg.timeout,
		log:       cfg.log,
	}, nil
}

// Subscribe registers a listener channel under the given ID.
// Returns an error if the ID is empty or already registered.
func (b *Bus) Subscribe(id string, ch chan<- Event) error {
	if b == nil {
		return nil
	}
	if id == "" {
		return fmt.Errorf("listener id is empty")
	}
	if ch == nil {
		return fmt.Errorf("listener channel is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.listeners[id]; ok {
		return fmt.Errorf("listener %q already registered", id)
	}
	b.listeners[id] = ch
	return nil
}

// Unsubscribe removes the listener with the given ID.
func (b *Bus) Unsubscribe(id string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.listeners, id)
}

// SetBuffering enables or disables synchronous buffering of events.
// While buffering is enabled, every sent event is appended to the internal buffer
// before being delivered to listeners.
func (b *Bus) SetBuffering(v bool) {
	if b == nil {
		return
	}
	b.shouldBuf.Store(v)
}

// Buffer returns a copy of the currently buffered events.
func (b *Bus) Buffer() []Event {
	if b == nil {
		return nil
	}
	b.bufMu.Lock()
	defer b.bufMu.Unlock()
	return slices.Clone(b.buf)
}

// ClearBuffer removes all buffered events.
func (b *Bus) ClearBuffer() {
	if b == nil {
		return
	}
	b.bufMu.Lock()
	defer b.bufMu.Unlock()
	b.buf = nil
}

// Send delivers the event to all registered listeners.
func (b *Bus) Send(ctx context.Context, e Event) error {
	if b == nil {
		return nil
	}

	if b.shouldBuf.Load() {
		b.bufMu.Lock()
		b.buf = append(b.buf, e)
		b.bufMu.Unlock()
	}

	l := b.log.With(slog.String("eid", internal.RandomID()))
	l.LogAttrs(ctx, slog.LevelInfo, "sending event", eventAttr(e))

	noCancelCtx := context.WithoutCancel(ctx)
	sendCtx, cancel := context.WithTimeout(noCancelCtx, b.timeout)
	defer cancel()

	b.mu.RLock()
	//CR: isn't it maps.Copy ?
	listeners := make(map[string]chan<- Event, len(b.listeners))
	for id, ch := range b.listeners {
		listeners[id] = ch
	}
	b.mu.RUnlock()

	var errs []error
	for id, ch := range listeners {
		l.LogAttrs(ctx, slog.LevelDebug, "sending event to listener", attr.ModID(id))
		if err := b.sendEvent(sendCtx, e, ch); err != nil {
			errs = append(errs, fmt.Errorf("sending event to %s: %w", id, err))
		}
	}

	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (b *Bus) sendEvent(ctx context.Context, e Event, ch chan<- Event) error {
	if b.timeout != 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, b.timeout)
		defer cancel()
	}

	select {
	case ch <- e:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
