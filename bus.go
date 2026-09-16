package popcorn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"sync"
)

const defaultHistoryCap = 1024

// bufferDrainTries bounds how many scheduling yields a full buffered ring attempts
// before it gives up and evicts the oldest event. It lets an already-runnable reader
// catch up without letting a genuinely slow subscriber stall Send.
const bufferDrainTries = 4

var (
	errNilBus             = errors.New("popcorn: nil bus")
	errSubscriptionID     = errors.New("popcorn: subscription id not set")
	errSubscriptionExists = errors.New("popcorn: subscription already registered")
	errBusNotInitialized  = errors.New("popcorn: bus not initialized")
	errReplayBuffer       = errors.New("popcorn: replay buffer must not be negative")
	errBacklog            = errors.New("popcorn: backlog must not be negative")
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
	s := &subscription{id: id, ch: ch, filter: cfg.filter, backlog: cfg.backlog}
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

// teardown stops the delivery goroutine (if any) and closes the subscription channel.
// The caller must hold b.mu so that no Send can be enqueuing concurrently.
func (s *subscription) teardown() {
	if s.backlog == 0 {
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

// seed replays the matching recent history into a new buffered subscription, oldest
// first. Rendezvous subscriptions have no ring to seed.
func (b *Bus) seed(s *subscription) {
	if s.backlog == 0 {
		return
	}
	matched := make([]Event, 0, s.backlog)
	for _, e := range slices.Backward(b.history) {
		if len(matched) >= s.backlog {
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
	if s.backlog == 0 {
		s.offerRendezvous(e)
		return
	}
	s.offerBuffered(e)
}

// offerBuffered implements drop-oldest delivery into a buffered subscription ring.
// The Bus is the only writer. When the ring is full it first gives an active reader
// a chance to drain (a bounded, non-blocking yield); only then does it evict the
// oldest event to make room, so Send still never stalls.
func (s *subscription) offerBuffered(e Event) {
	select {
	case s.ch <- e:
		return
	default:
	}
	for range bufferDrainTries {
		runtime.Gosched()
		select {
		case s.ch <- e:
			return
		default:
		}
	}
	// Ring is full and the reader is not keeping up: evict exactly one oldest event.
	select {
	case <-s.ch:
	default:
	}
	select {
	case s.ch <- e:
	case <-s.done:
	}
}

// offerRendezvous enqueues into a rendezvous subscription. A cap-0 subscription has
// no ring: it holds at most the event the delivery goroutine is handing off plus one
// queued event. A third pending event means the reader is not keeping up, so the
// in-flight hand-off is aborted and the subscription sheds until a reader catches up.
func (s *subscription) offerRendezvous(e Event) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if len(s.inbox) == 0 && s.waiting && !s.handoffClaimed {
		// The delivery goroutine is idle and waiting for work: hand the event
		// over synchronously so a following send finds the inbox empty. The
		// claim flag keeps a second send from racing the receiver's reset.
		s.waiting = false
		s.handoffClaimed = true
		s.mu.Unlock()
		select {
		case s.handoff <- e:
		case <-s.done:
		}
		return
	}
	if s.shedding {
		// A reader fell behind: drop rather than queue behind the in-flight
		// event so a burst cannot accumulate.
		s.mu.Unlock()
		return
	}
	if len(s.inbox) > 0 {
		s.shed = true
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

// cap0Loop moves queued events to the subscription channel. It blocks on the
// channel until a reader arrives. A write that overflows the single-slot inbox
// aborts the in-flight hand-off, and the subscription then sheds events without
// blocking until a reader shows up again.
func (s *subscription) cap0Loop() {
	defer close(s.stopped)
	for {
		e, abort, force, ok := s.nextRendezvous()
		if !ok {
			return
		}
		if force {
			s.setShedding(true)
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

// setShedding records shed mode. Leaving shed mode also discards any queued event,
// which belongs to the burst the reader fell behind on.
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

// nextRendezvous blocks until there is an event to hand off or the subscription is
// torn down. The abort channel cancels an in-flight hand-off once a newer event
// arrives; force reports a pending overflow so the caller enters shedding mode.
func (s *subscription) nextRendezvous() (Event, chan struct{}, bool, bool) {
	s.mu.Lock()
	if len(s.inbox) > 0 {
		e := s.inbox[0]
		s.inbox = s.inbox[:0]
		abort, force := s.abort, s.shed
		s.shed = false
		s.mu.Unlock()
		return e, abort, force, true
	}
	if s.closed {
		s.mu.Unlock()
		return Event{}, nil, false, false
	}
	s.waiting = true
	s.mu.Unlock()

	select {
	case e := <-s.handoff:
		s.mu.Lock()
		s.waiting = false
		s.handoffClaimed = false
		abort, force := s.abort, s.shed
		s.shed = false
		s.mu.Unlock()
		return e, abort, force, true
	case <-s.done:
		return Event{}, nil, false, false
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
		return fmt.Errorf("popcorn: send context: %w", err)
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

// WithBusLogger sets the Bus logger.
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
// f is panic-recovered; a panic drops the event. A nil filter accepts everything.
// Keep f pure, non-blocking, and free of Bus or Kernel calls: live delivery runs it
// outside the bus lock, but subscribe-time replay runs it under that lock and the
// kernel publishes lifecycle events while holding its state lock, so a filter that
// blocks or waits for a later lifecycle event can deadlock.
func WithFilter(f func(Event) bool) SubOption {
	return func(cfg *subConfig) error {
		cfg.filter = guardFilter(f)
		return nil
	}
}

func guardFilter(f func(Event) bool) func(Event) bool {
	if f == nil {
		return nil
	}
	return func(e Event) (ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		return f(e)
	}
}

// busConfig, subConfig are private option targets.
type busConfig struct {
	replayBuffer int
	log          *slog.Logger
}

type subConfig struct {
	backlog int
	filter  func(Event) bool
}

type subscription struct {
	id      string
	ch      chan Event
	filter  func(Event) bool
	backlog int

	// Rendezvous (backlog == 0) delivery state.
	mu             sync.Mutex
	inbox          []Event
	handoff        chan Event
	waiting        bool
	handoffClaimed bool
	shedding       bool
	shed           bool
	abort          chan struct{}
	done           chan struct{}
	stopped        chan struct{}
	closed         bool
}
