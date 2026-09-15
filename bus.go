package popcorn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/paluszkiewiczB/popcorn/plog"
	"github.com/paluszkiewiczB/popcorn/plog/attr"
)

// CR: shouldn't it accept `plog.Logger`?

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

	bufMu        sync.Mutex
	flushCond    *sync.Cond
	buffering    bool
	buf          []Event
	pendingSends int
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

	bus := &Bus{
		mu:        sync.RWMutex{},
		listeners: make(map[string]chan<- Event),
		timeout:   cfg.timeout,
		log:       cfg.log,
		bufMu:     sync.Mutex{},
	}
	bus.flushCond = sync.NewCond(&bus.bufMu)
	return bus, nil
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
	if _, ok := b.listeners[id]; ok {
		b.mu.Unlock()
		return ErrDuplicateID
	}
	b.listeners[id] = ch
	b.mu.Unlock()

	b.bufMu.Lock()
	if b.buffering {
		for _, e := range b.buf {
			select {
			case ch <- e:
			default:
			}
		}
	}
	b.bufMu.Unlock()

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

// StartBuffering enables event buffering for late subscribers.
// Events sent while buffering is active are stored and replayed to
// subscribers that join later. Any existing buffer is cleared first.
func (b *Bus) StartBuffering() {
	b.bufMu.Lock()
	b.buf = nil
	b.buffering = true
	b.bufMu.Unlock()
}

// FinishBuffering disables buffering and delivers any remaining
// buffered events to all current subscribers.
func (b *Bus) FinishBuffering() {
	b.bufMu.Lock()
	for b.pendingSends > 0 {
		b.flushCond.Wait()
	}
	remaining := b.buf
	b.buf = nil
	b.buffering = false
	b.bufMu.Unlock()

	b.mu.RLock()
	chans := make([]chan<- Event, 0, len(b.listeners))
	for _, ch := range b.listeners {
		chans = append(chans, ch)
	}
	b.mu.RUnlock()

	for _, e := range remaining {
		go func(evt Event) {
			for _, ch := range chans {
				ch <- evt
			}
		}(e)
	}
}

// SetBuffering enables or disables event buffering.
//
// Deprecated: use [Bus.StartBuffering] and [Bus.FinishBuffering].
func (b *Bus) SetBuffering(v bool) {
	if v {
		b.StartBuffering()
	} else {
		b.bufMu.Lock()
		b.buffering = false
		b.bufMu.Unlock()
	}
}

// Buffer returns all buffered events.
func (b *Bus) Buffer() []Event {
	b.bufMu.Lock()
	defer b.bufMu.Unlock()

	return slices.Clone(b.buf)
}

// ClearBuffer clears all buffered events and disables buffering.
//
// Deprecated: use [Bus.FinishBuffering].
func (b *Bus) ClearBuffer() {
	b.FinishBuffering()
}

// Send sends an event to all registered listeners.
func (b *Bus) Send(ctx context.Context, e Event) error {
	if b == nil {
		return nil
	}

	b.bufMu.Lock()
	b.pendingSends++
	if b.buffering {
		b.buf = append(b.buf, e)
	}
	b.pendingSends--
	if b.pendingSends == 0 {
		b.flushCond.Broadcast()
	}
	b.bufMu.Unlock()

	noCancelCtx := context.WithoutCancel(ctx)
	sendCtx, cancel := context.WithTimeout(noCancelCtx, b.timeout)
	defer cancel()

	return b.deliverToAll(sendCtx, e)
}

func (b *Bus) deliverToAll(ctx context.Context, e Event) error {
	b.mu.RLock()
	listeners := make(map[string]chan<- Event, len(b.listeners))
	maps.Copy(listeners, b.listeners)
	b.mu.RUnlock()

	b.log.LogAttrs(
		ctx, slog.LevelDebug, "sending event",
		slog.String("kind", e.Kind), attr.ModID(e.Source),
		slog.Int("listeners", len(listeners)),
	)

	for id, ch := range listeners {
		if err := sendEvent(ctx, e, ch); err != nil {
			b.log.LogAttrs(ctx, slog.LevelWarn, "failed to send event", attr.ModID(id), attr.Err(err))
		}
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("send timeout: %w", err)
	}

	return nil
}

func sendEvent(ctx context.Context, e Event, ch chan<- Event) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("send failed: %w", ctx.Err())
	case ch <- e:
		return nil
	}
}
