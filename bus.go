package popcorn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
)

const defaultHistoryCap = 64

var (
	errNilBus             = errors.New("nil bus")
	errSubscriptionID     = errors.New("subscription id not set")
	errSubscriptionExists = errors.New("subscription already registered")
	errBusNotInitialized  = errors.New("publisher is not bound to a bus")
	errReplayBuffer       = errors.New("replay buffer must not be negative")
	errBacklog            = errors.New("backlog must not be negative")
)

// Bus is an in-process event bus. It fans every event out to all matching
// subscriptions and is the single communication mechanism between modules.
//
// Each subscription is a bounded channel. Send never waits for a reader: when a
// subscription's buffer is full, its oldest event is dropped. A slow subscriber
// can only overflow its own subscription. See [WithBacklog] and
// [WithReplayBuffer].
type Bus struct {
	cfg busConfig

	mu       sync.Mutex
	subs     map[string]*subscription
	history  []Event
	histHead int
	histLen  int
}

// NewBus creates a Bus with a bounded replay history; see [WithReplayBuffer].
func NewBus(opts ...BusOption) (*Bus, error) {
	cfg := busConfig{log: discardLogger(), replayBuffer: defaultHistoryCap}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	return newBus(cfg), nil
}

func newBus(cfg busConfig) *Bus {
	if cfg.log == nil {
		cfg.log = discardLogger()
	}
	b := &Bus{
		cfg:  cfg,
		subs: map[string]*subscription{},
	}
	if cfg.replayBuffer > 0 {
		b.history = make([]Event, cfg.replayBuffer)
	}
	return b
}

// Subscribe registers a subscription under id and returns its receive-only
// channel. The Bus owns the channel: it is the only writer and the only closer,
// so a consumer can read it until [Bus.Close] or [Bus.Unsubscribe].
//
// The channel buffers up to the [WithBacklog] size, a minimum of one event by
// default, and drops its oldest event when full. A subscription created after
// events have already been sent is first seeded with matching history, capped by
// the buffer size, so a late subscriber can catch up. See [WithReplayBuffer].
//
// id identifies the subscription and is the value [Publisher.Send] compares
// against [Event.Source] to skip a publisher's own subscription.
func (b *Bus) Subscribe(id string, opts ...SubOption) (<-chan Event, error) {
	if b == nil {
		return nil, errNilBus
	}
	cfg := subConfig{}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	if id == "" {
		return nil, errSubscriptionID
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.subs[id]; exists {
		return nil, fmt.Errorf("%w: %q", errSubscriptionExists, id)
	}

	ch := make(chan Event, max(1, cfg.backlog))
	s := &subscription{id: id, ch: ch, filter: guardFilter(cfg.filter, b.cfg.log)}
	b.seed(s)
	b.subs[id] = s
	return ch, nil
}

// Unsubscribe removes the subscription with id and closes its channel. Events
// already buffered remain readable. It is safe to call more than once.
func (b *Bus) Unsubscribe(id string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.subs[id]
	if !ok {
		return
	}
	delete(b.subs, id)
	s.teardown()
}

// Close tears down every subscription and closes its channels. Events already
// buffered remain readable. It is safe to call more than once and is intended
// for shutdown and tests.
func (b *Bus) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, s := range b.subs {
		delete(b.subs, id)
		s.teardown()
	}
}

func (s *subscription) teardown() {
	close(s.ch)
}

// Publisher returns a Publisher bound to id. Send stamps [Event.Source] with id
// and skips the subscription with the same id, so a module does not receive its
// own events. A nil Bus yields a nil Publisher.
func (b *Bus) Publisher(id string) *Publisher {
	if b == nil {
		return nil
	}
	return &Publisher{bus: b, id: id}
}

func (b *Bus) seed(s *subscription) {
	if len(b.history) == 0 {
		return
	}
	backlog := cap(s.ch)
	matched := make([]Event, 0, min(backlog, b.histLen))
	for i := 0; i < b.histLen && len(matched) < backlog; i++ {
		e := b.history[(b.histHead+b.histLen-1-i)%len(b.history)]
		if s.filter == nil || s.filter(e) {
			matched = append(matched, e)
		}
	}
	for _, e := range slices.Backward(matched) {
		select {
		case s.ch <- e:
		default:
		}
	}
}

func (b *Bus) remember(e Event) {
	if len(b.history) == 0 {
		return
	}
	if b.histLen < len(b.history) {
		b.history[(b.histHead+b.histLen)%len(b.history)] = e
		b.histLen++
		return
	}
	b.history[b.histHead] = e
	b.histHead = (b.histHead + 1) % len(b.history)
}

func (s *subscription) offer(e Event) {
	select {
	case s.ch <- e:
		return
	default:
	}
	select {
	case <-s.ch:
	default:
	}
	s.ch <- e
}

// Publisher sends events on behalf of a single id. The Bus creates it via
// [Bus.Publisher], which binds the id so [Event.Source] is set by the Bus rather
// than by the caller.
type Publisher struct {
	bus *Bus
	id  string
}

// Send delivers e to every subscription except the publisher's own. If ctx is
// already canceled, Send returns an error and delivers nothing; otherwise
// delivery does not block on slow subscribers.
func (p *Publisher) Send(ctx context.Context, e Event) error {
	if p == nil || p.bus == nil {
		return errBusNotInitialized
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	e.source = p.id

	b := p.bus
	b.mu.Lock()
	b.remember(e)
	targets := make([]*subscription, 0, len(b.subs))
	for _, s := range b.subs {
		if s.id != p.id {
			targets = append(targets, s)
		}
	}
	b.mu.Unlock()

	for _, s := range targets {
		if s.filter != nil && !s.filter(e) {
			continue
		}
		b.mu.Lock()
		if live := b.subs[s.id]; live == s {
			s.offer(e)
		}
		b.mu.Unlock()
	}
	return nil
}

// BusOption configures a Bus.
type BusOption func(*busConfig) error

// WithReplayBuffer sets how many recent events the Bus retains to seed a
// subscription created later. The default is 64; 0 disables replay; a negative n
// errors. A subscription is seeded up to its buffer size, which is at least one
// event.
func WithReplayBuffer(n int) BusOption {
	return func(cfg *busConfig) error {
		if n < 0 {
			return errReplayBuffer
		}
		cfg.replayBuffer = n
		return nil
	}
}

// WithBusLogger sets the logger for internal Bus diagnostics. A nil logger
// discards them.
func WithBusLogger(l *slog.Logger) BusOption {
	return func(cfg *busConfig) error {
		cfg.log = l
		return nil
	}
}

// SubOption configures a single subscription.
type SubOption func(*subConfig) error

// WithBacklog sets the subscription's buffer size. A subscription never blocks
// Send; when full, its oldest buffered event is dropped. The default is 0, which
// means a single-event buffer, the minimum. WithBacklog also caps how many
// history events are replayed at subscribe time; the buffer is at least one
// event.
func WithBacklog(n int) SubOption {
	return func(cfg *subConfig) error {
		if n < 0 {
			return errBacklog
		}
		cfg.backlog = n
		return nil
	}
}

// WithFilter restricts which events are enqueued and replayed for this
// subscription. A nil filter accepts everything.
//
// Keep f pure, fast, and non-blocking, and do not call the Bus or the Kernel from
// it: a filter may run while the Bus is locked, so calling back into the Bus can
// deadlock. A panicking filter drops the event and logs it; see [WithBusLogger].
func WithFilter(f func(Event) bool) SubOption {
	return func(cfg *subConfig) error {
		cfg.filter = f
		return nil
	}
}

func guardFilter(f func(Event) bool, log *slog.Logger) func(Event) bool {
	if f == nil {
		return nil
	}
	return func(e Event) (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				log.Warn("event filter panicked", "kind", e.Kind, "panic", r)
				ok = false
			}
		}()
		return f(e)
	}
}

type busConfig struct {
	replayBuffer int
	log          *slog.Logger
}

type subConfig struct {
	backlog int
	filter  func(Event) bool
}

type subscription struct {
	id     string
	ch     chan Event
	filter func(Event) bool
}
