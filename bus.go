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
// Each subscription has its own buffer, and Send never waits for a reader. A
// buffered subscription drops its oldest event when full; an unbuffered one
// drops events a stalled consumer cannot keep up with. A slow subscriber can
// only overflow its own subscription. See [WithBacklog] and [WithReplayBuffer].
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

// newBus builds a Bus from an already-resolved config. Unlike NewBus it cannot
// fail, so callers that assemble the config directly (the Kernel's own bus) need
// no error path.
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
// The channel buffers up to the [WithBacklog] size, 0 by default. A buffered
// subscription created after events have already been sent is first seeded with
// matching history, capped by the backlog, so a late subscriber can catch up; an
// unbuffered subscription is never seeded. See [WithReplayBuffer].
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

	ch := make(chan Event, cfg.backlog)
	s := &subscription{id: id, ch: ch, filter: guardFilter(cfg.filter, b.cfg.log)}
	if cfg.backlog == 0 {
		s.handoff = make(chan Event)
		s.abort = make(chan struct{})
		s.done = make(chan struct{})
		s.stopped = make(chan struct{})
		s.waiting = true
		go s.cap0Loop()
	}
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

// Caller must hold b.mu so no Send can be enqueuing concurrently.
func (s *subscription) teardown() {
	if s.unbuffered() {
		s.mu.Lock()
		if !s.closed {
			s.closed = true
			close(s.done)
			s.abortLocked()
		}
		s.mu.Unlock()
		<-s.stopped
	}
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
	if s.unbuffered() || len(b.history) == 0 {
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

// remember writes e into the fixed-length history ring in O(1). The ring is
// empty when replay is disabled, in which case nothing is retained.
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
	if s.unbuffered() {
		s.offerRendezvous(e)
		return
	}
	s.offerBuffered(e)
}

func (s *subscription) unbuffered() bool { return cap(s.ch) == 0 }

// The Bus is the only writer, so a full ring always has room after one eviction.
func (s *subscription) offerBuffered(e Event) {
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

// Holds at most one queued event next to the in-flight hand-off. A third pending
// event aborts the hand-off and sheds until a reader catches up.
func (s *subscription) offerRendezvous(e Event) {
	s.mu.Lock()
	if len(s.inbox) == 0 && s.waiting && !s.handoffClaimed {
		// The delivery goroutine is idle: hand over synchronously. The claim flag
		// keeps a second send from racing the receiver's reset.
		s.waiting = false
		s.handoffClaimed = true
		s.mu.Unlock()
		select {
		case s.handoff <- e:
		case <-s.done:
		}
		return
	}
	if len(s.inbox) > 0 {
		s.shedding = true
		s.abortLocked()
	}
	s.inbox = append(s.inbox[:0], e)
	s.mu.Unlock()
}

// abortLocked is only ever reached under s.mu with a fresh abort channel: every
// call replaces the one it closes, and teardown runs at most once.
func (s *subscription) abortLocked() {
	close(s.abort)
	s.abort = make(chan struct{})
}

func (s *subscription) cap0Loop() {
	defer close(s.stopped)
	for {
		e, abort, ok := s.nextRendezvous()
		if !ok {
			return
		}
		if s.isShedding() {
			select {
			case s.ch <- e:
				s.setShedding(false)
			default:
			}
			continue
		}
		select {
		case s.ch <- e:
		case <-abort:
			s.setShedding(true)
		}
	}
}

// Discards the queued event too; it belongs to the burst the reader fell behind on.
func (s *subscription) setShedding(v bool) {
	s.mu.Lock()
	s.shedding = v
	if !v {
		s.inbox = s.inbox[:0]
	}
	s.mu.Unlock()
}

func (s *subscription) isShedding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shedding
}

func (s *subscription) nextRendezvous() (Event, chan struct{}, bool) {
	s.mu.Lock()
	if len(s.inbox) > 0 {
		e := s.inbox[0]
		s.inbox = s.inbox[:0]
		abort := s.abort
		s.mu.Unlock()
		return e, abort, true
	}
	if s.closed {
		s.mu.Unlock()
		return Event{}, nil, false
	}
	s.waiting = true
	s.mu.Unlock()

	select {
	case e := <-s.handoff:
		s.mu.Lock()
		s.waiting = false
		s.handoffClaimed = false
		abort := s.abort
		s.mu.Unlock()
		return e, abort, true
	case <-s.done:
		return Event{}, nil, false
	}
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
		// Identity, not id: the same id may have been torn down and resubscribed
		// while the filter ran, and offering to the torn-down ring would panic.
		if live := b.subs[s.id]; live == s {
			s.offer(e)
		}
		b.mu.Unlock()
	}
	return nil
}

// BusOption configures a Bus.
type BusOption func(*busConfig) error

// WithReplayBuffer sets how many recent events the Bus retains so that a
// subscription created later can be seeded with matching history. The default is
// 64; 0 disables replay; a negative n errors. A subscription is seeded up to its
// [WithBacklog] size, so an unbuffered subscription is never seeded.
func WithReplayBuffer(n int) BusOption {
	return func(cfg *busConfig) error {
		if n < 0 {
			return errReplayBuffer
		}
		cfg.replayBuffer = n
		return nil
	}
}

// WithBusLogger sets the logger for internal diagnostics, currently only
// panicking event filters. A nil logger is replaced by a discarding logger.
func WithBusLogger(l *slog.Logger) BusOption {
	return func(cfg *busConfig) error {
		cfg.log = l
		return nil
	}
}

// SubOption configures a single subscription.
type SubOption func(*subConfig) error

// WithBacklog sets the subscription's buffer size. A buffered subscription never
// blocks Send; when it is full, its oldest buffered event is dropped. The default
// is 0, an unbuffered subscription that drops events under a burst rather than
// queueing them. WithBacklog also caps how many history events are replayed at
// subscribe time.
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

// guardFilter wraps f so a panicking filter drops the event instead of taking down
// Send. The panic is logged: it is always a bug in caller code.
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

	// Rendezvous (cap-0) delivery state.
	mu             sync.Mutex
	inbox          []Event
	handoff        chan Event
	waiting        bool
	handoffClaimed bool
	shedding       bool
	abort          chan struct{}
	done           chan struct{}
	stopped        chan struct{}
	closed         bool
}
