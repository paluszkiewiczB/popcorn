package popcorn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
)

const defaultHistoryCap = 1024

var (
	errNilBus             = errors.New("nil bus")
	errSubscriptionID     = errors.New("subscription id not set")
	errSubscriptionExists = errors.New("subscription already registered")
	errBusNotInitialized  = errors.New("publisher is not bound to a bus")
	errReplayBuffer       = errors.New("replay buffer must not be negative")
	errBacklog            = errors.New("backlog must not be negative")
)

// Bus is an in-process event bus with per-subscription, bounded, isolated delivery.
// It is the single owner of every subscription channel and of the bounded replay
// history.
//
// Delivery: Send enqueues into each matching subscription without waiting for a
// reader, and a subscriber can only overflow its own ring. A buffered subscription
// never blocks Send. A rendezvous (cap-0) subscription synchronizes briefly with its
// delivery goroutine on each send, so a stalled reader is dropped rather than queued.
type Bus struct {
	cfg busConfig

	mu      sync.Mutex
	subs    map[string]*subscription
	history []Event
}

// NewBus creates a Bus.
func NewBus(opts ...BusOption) (*Bus, error) {
	cfg := busConfig{log: discardLogger()}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	if cfg.log == nil {
		cfg.log = discardLogger()
	}
	return &Bus{
		cfg:  cfg,
		subs: map[string]*subscription{},
	}, nil
}

// Subscribe creates a subscription and returns its receive-only channel. The Bus
// owns the channel: it is the only writer and the only closer.
//
// If a replay history is configured, Subscribe atomically seeds the subscription
// with the matching recent events (bounded by WithBacklog) and then registers it for
// live delivery, so there is no gap and no duplicate at the seam.
//
// id identifies the subscription and is also the value the sender-skip check
// compares against Event.Source.
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

// Unsubscribe tears the subscription down and closes its channel. It is safe to call
// more than once.
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

// Close tears down every subscription and closes its channel. It is safe to call
// more than once and is intended for shutdown and tests.
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

// Publisher returns a bound Publisher for id. Send sets Event.Source to id and skips
// the subscription with the same id. A nil Bus yields a nil Publisher.
func (b *Bus) Publisher(id string) *Publisher {
	if b == nil {
		return nil
	}
	return &Publisher{bus: b, id: id}
}

func (b *Bus) seed(s *subscription) {
	if s.unbuffered() {
		return
	}
	backlog := cap(s.ch)
	matched := make([]Event, 0, backlog)
	for _, e := range slices.Backward(b.history) {
		if len(matched) >= backlog {
			break
		}
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

func (b *Bus) replayCap() int {
	if b.cfg.replayBuffer > 0 {
		return b.cfg.replayBuffer
	}
	return defaultHistoryCap
}

func (b *Bus) remember(e Event) {
	hc := b.replayCap()
	b.history = append(b.history, e)
	if len(b.history) > hc {
		b.history = append(b.history[:0], b.history[len(b.history)-hc:]...)
	}
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
	if s.closed {
		s.mu.Unlock()
		return
	}
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

func (s *subscription) abortLocked() {
	select {
	case <-s.abort:
		return
	default:
	}
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
		case <-s.done:
			return
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

// Publisher sends events on behalf of a single module. The Bus creates it via
// Bus.Publisher and binds the id, so Event.Source is set by the Bus rather than by
// the caller. Any holder of the Bus may ask for a Publisher under any id; the kernel
// only trusts sources it has registered.
type Publisher struct {
	bus *Bus
	id  string
}

// Send delivers e to every subscription except the publisher's own. The caller's
// context only gates the send; a canceled context surfaces as an error. Filters run
// outside the bus lock.
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

// WithReplayBuffer sets the maximum number of recent events the Bus retains for
// replay. A buffered subscription (WithBacklog > 0) is seeded with up to its backlog
// of the most recent matching events before live delivery begins. A non-positive n
// keeps the default bound (1024); a positive n replaces it. Rendezvous subscriptions
// have no ring and are never seeded.
func WithReplayBuffer(n int) BusOption {
	return func(cfg *busConfig) error {
		if n < 0 {
			return errReplayBuffer
		}
		cfg.replayBuffer = n
		return nil
	}
}

// WithBusLogger sets the logger used for internal diagnostics, currently only
// panicking event filters.
func WithBusLogger(l *slog.Logger) BusOption {
	return func(cfg *busConfig) error {
		cfg.log = l
		return nil
	}
}

// SubOption configures a single subscription.
type SubOption func(*subConfig) error

// WithBacklog sets the subscription's bounded ring size. It also caps how many
// history events are replayed at subscribe time. The default is 0: a rendezvous
// subscription with no buffering.
func WithBacklog(n int) SubOption {
	return func(cfg *subConfig) error {
		if n < 0 {
			return errBacklog
		}
		cfg.backlog = n
		return nil
	}
}

// WithFilter restricts which events are enqueued and replayed for this subscription.
// A nil filter accepts everything; a panicking one drops the event and is logged.
// Keep f pure, non-blocking, and free of Bus or Kernel calls: live delivery runs it
// outside the bus lock, but subscribe-time replay runs it under that lock and the
// kernel publishes lifecycle events while holding its state lock, so a filter that
// blocks or waits for a later lifecycle event can deadlock.
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
