package popcorn

// CR: shouldn't it accept `plog.Logger`?

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paluszkiewiczB/popcorn/plog"
	"github.com/paluszkiewiczB/popcorn/plog/attr"
)

// ErrEmptyID is returned when trying to subscribe with an empty id.
var ErrEmptyID = errors.New("listener id is empty")

// ErrNilChannel is returned when trying to subscribe with a nil channel.
var ErrNilChannel = errors.New("listener channel is nil")

// ErrDuplicateID is returned when trying to subscribe with an already-registered id.
var ErrDuplicateID = errors.New("listener already registered")

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
		mu:        sync.RWMutex{},
		listeners: make(map[string]chan<- Event),
		timeout:   cfg.timeout,
		log:       cfg.log,
		shouldBuf: atomic.Bool{},
		bufMu:     sync.Mutex{},
		buf:       nil,
	}, nil
}

// Subscribe registers a channel to receive events for the given id.
func (b *Bus) Subscribe(id string, ch chan<- Event) error {
	if id == "" {
		return ErrEmptyID
	}

	if ch == nil {
		return ErrNilChannel
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.listeners[id]; ok {
		return ErrDuplicateID
	}

	b.listeners[id] = ch

	return nil
}

// Unsubscribe removes the listener with the given id.
func (b *Bus) Unsubscribe(id string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.listeners, id)
}

// SetBuffering enables or disables event buffering.
func (b *Bus) SetBuffering(v bool) {
	b.shouldBuf.Store(v)
}

// Buffer returns all buffered events.
func (b *Bus) Buffer() []Event {
	b.bufMu.Lock()
	defer b.bufMu.Unlock()

	return slices.Clone(b.buf)
}

// ClearBuffer clears all buffered events.
func (b *Bus) ClearBuffer() {
	b.bufMu.Lock()
	defer b.bufMu.Unlock()

	b.buf = nil
}

// Send sends an event to all registered listeners.
func (b *Bus) Send(ctx context.Context, e Event) error {
	if b == nil {
		return nil
	}
	if b.shouldBuf.Load() {
		b.bufMu.Lock()
		b.buf = append(b.buf, e)
		b.bufMu.Unlock()
	}

	noCancelCtx := context.WithoutCancel(ctx)

	sendCtx, cancel := context.WithTimeout(noCancelCtx, b.timeout)
	defer cancel()

	b.mu.RLock()
	// CR: isn't it maps.Copy ?
	listeners := make(map[string]chan<- Event, len(b.listeners))
	maps.Copy(listeners, b.listeners)
	b.mu.RUnlock()

	b.log.LogAttrs(
		ctx, slog.LevelDebug, "sending event",
		slog.String("kind", e.Kind), attr.ModID(e.Source),
		slog.Int("listeners", len(listeners)),
	)

	for id, ch := range listeners {
		if err := b.sendEvent(sendCtx, e, ch); err != nil {
			b.log.LogAttrs(ctx, slog.LevelWarn, "failed to send event", attr.ModID(id), attr.Err(err))
		}
	}

	if err := sendCtx.Err(); err != nil {
		return fmt.Errorf("send timeout: %w", err)
	}

	return nil
}

func (b *Bus) sendEvent(ctx context.Context, e Event, ch chan<- Event) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("send failed: %w", ctx.Err())
	case ch <- e:
		return nil
	}
}
