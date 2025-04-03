package popcorn

import (
	"context"
	"errors"
	"fmt"
	"github.com/paluszkiewiczB/popcorn/plog"
	"github.com/paluszkiewiczB/popcorn/plog/attr"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type ErrKernelUnhealthy struct {
	cause error
}

func (e ErrKernelUnhealthy) Error() string {
	return fmt.Sprintf("kernel is unhealthy, %s", e.cause)
}

func (e ErrKernelUnhealthy) Unwrap() error {
	return e.cause
}

type ModuleState int32

func (s ModuleState) asInt() int32 {
	return int32(s)
}

const (
	ModuleStateUnknown ModuleState = iota
	ModuleStateOK
	ModuleStateNOK
	ModuleStateTempNOK
)

// The ModuleStateStore stores a ModuleState
type ModuleStateStore atomic.Int32

func (s *ModuleStateStore) Set(state ModuleState) {
	s.asAtomic().Store(state.asInt())
}

func (s *ModuleStateStore) CAS(from, to ModuleState) bool {
	return s.asAtomic().CompareAndSwap(from.asInt(), to.asInt())
}

func (s *ModuleStateStore) Get() ModuleState {
	return ModuleState(s.asAtomic().Load())
}

func (s *ModuleStateStore) asAtomic() *atomic.Int32 {
	return (*atomic.Int32)(s)
}

// SetupFunc is a function used to set up (initialize) a [Module].
// Context passed to this function can time out - you must not retain it.
type SetupFunc func(ctx context.Context) error

// StartFunc is a function used to start a [Module].
// If the returned StopFunc is non-nil, it will be invoked to stop the module during [Kernel] shutdown.
// StartFunc *must not* retain the context - [Kernel] might cancel it to limit time spent on a StartFunc (timeout).
// To reuse values passed in the context, use [RetainContext] or [RetainContextCause].
type StartFunc func(ctx context.Context) (StopFunc, error)

// RetainContext returns a new context with new [context.CancelFunc].
func RetainContext(ctx context.Context) (context.Context, func()) {
	retained := context.WithoutCancel(ctx)
	return context.WithCancel(retained)
}

// RetainContextCause returns a new context with new [context.CancelCauseFunc].
func RetainContextCause(ctx context.Context) (context.Context, func(error)) {
	retained := context.WithoutCancel(ctx)
	return context.WithCancelCause(retained)
}

// StopFunc is invoked to stop a [Module].
// It should clean all the resources allocated during the startup process, background jobs, etc.
type StopFunc func(ctx context.Context) (err error)

func NoStop(err ...error) (StopFunc, error) {
	return nil, errors.Join(err...)
}

// Module represents a module of a Popcorn framework.
// It should be a self-containing piece of code, knowing how to properly start and stop itself.
// Kernel manages a lifecycle of a Module.
type Module struct {
	id               string   // it is recommended to use a full Go mod path as a module id, e.g. "github.com/paluszkiewiczB/popcorn/modules/sql"
	dependencies     []string // startup dependencies
	dependenciesLeft []string // startup dependencies, mutable during startup

	c chan Event

	start StartFunc
	stop  StopFunc
}

// ModRecipe is a recipe used to create a [Module].
type ModRecipe struct {
	// ID of the module.
	// Must not be empty.
	// Must be different from [EventSourceKernel].
	// Must be unique - [Kernel] won't start with two modules having the same ID.
	//
	// It is recommended to use a full import path of the module as a 'prefix' of the ID.
	ID string
	// Dependencies are IDs of modules, which must be started before this one.
	Dependencies []string
	// Start is a non-nil function called, when the [Module] is started. See [StartFunc] docs.
	Start StartFunc
	// EventsChan is an optional channel, which will be used to notify the [Module] about in-app Events.
	// Events are not sent before Start is called.
	// After the successful start of a module, this channel will receive all the events that took place before this Module was started, then the ModuleStarted event.
	EventsChan chan Event
}

func (c ModRecipe) validate() error {
	if len(c.ID) == 0 {
		return fmt.Errorf("id not set")
	}

	if EventSourceKernel == c.ID {
		return fmt.Errorf("cannot use module id '%s' - it is reserved by the kernel", EventSourceKernel)
	}

	if c.Start == nil {
		return fmt.Errorf("start function not set")
	}

	return nil
}

func NewModule(cfg ModRecipe) (*Module, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("creating new module with invalid configuration: %v, %w", cfg, err)
	}

	return &Module{
		id:               cfg.ID,
		dependencies:     cfg.Dependencies,
		dependenciesLeft: slices.Clone(cfg.Dependencies),
		c:                cfg.EventsChan,
		start:            cfg.Start,
	}, nil
}

type Kernel struct {
	state          *ModuleStateStore
	modules        []*Module
	startedModules []*Module

	b              *Bus
	cancelListener func(error)

	healthTick  time.Duration
	stopTimeout time.Duration

	l plog.Slog
}

func NewKernel(b *Bus, l plog.Slog, mods ...*Module) (*Kernel, error) {
	ids := make(map[string]int)
	for i, mod := range mods {
		if oldIndex, ok := ids[mod.id]; ok {
			return nil, fmt.Errorf("duplicate module id: %s, passed at indexes: %d and %d", mod.id, oldIndex, i)
		} else {
			ids[mod.id] = i
		}
	}

	if l == nil {
		l = slog.Default()
		l.LogAttrs(context.Background(), slog.LevelWarn, "logger was nil, using default one")
	}

	if b.timeout == 0 {
		b.timeout = time.Second
	}

	if b.l == nil {
		b.l = l
	}

	return &Kernel{
		state:       &ModuleStateStore{},
		modules:     mods,
		b:           b,
		stopTimeout: time.Second * 5,
		healthTick:  time.Second,
		l:           l,
	}, nil
}

func (k *Kernel) Start(ctx context.Context) error {
	if k.b != nil {
		k.b.shouldBuf.Store(true)
		listenCtx, cf := context.WithCancelCause(ctx)
		k.cancelListener = cf

		c := make(chan Event, 10)
		k.b.listeners[EventSourceKernel] = c
		go func() {
			for {
				select {
				case <-listenCtx.Done():
					k.l.LogAttrs(ctx, slog.LevelInfo, "listening for events by the Kernel was canceled", attr.Err(listenCtx.Err()))
					return
				case e := <-c:
					k.handleEvent(ctx, e)
				}
			}
		}()
	}

	addListener := func(m *Module) {
		if k.b != nil && m.c != nil {
			k.l.LogAttrs(ctx, slog.LevelInfo, "adding new event listener", attr.ModId(m.id), attr.Chan(m.c))
			k.b.listeners[m.id] = m.c
		}
	}

	k.l.LogAttrs(ctx, slog.LevelInfo, "starting the kernel", slog.Int("modCount", len(k.modules)))
	started := make(map[string]struct{}, len(k.modules))
	left := make(map[string]*Module)

	// key is module id, value is a list of other modules depending on the key module
	dependents := make(map[string][]string)

	for _, mod := range k.modules {
		id := mod.id
		left[id] = mod
		for _, depId := range mod.dependencies {
			k.l.LogAttrs(ctx, slog.LevelInfo, "module has a dependency", attr.ModId(id), slog.String("dependsOn", depId))
			dependents[depId] = append(dependents[depId], id)
		}
	}

	var i int
	// FIXME this will block forever if there is a circular dependency
	//  build a dependency tree here
	for len(left) != 0 {
		newStarted := make([]string, 0)
		for id, mod := range left {
			k.l.LogAttrs(ctx, slog.LevelInfo, "trying to start module", attr.ModId(id), slog.Int("depsLeft", len(mod.dependenciesLeft)))
			if len(mod.dependenciesLeft) == 0 {
				k.l.LogAttrs(ctx, slog.LevelInfo, "module has no dependencies left, starting", attr.ModId(id))

				start := time.Now()
				stop, err := mod.start(ctx)
				if err != nil {
					// TODO: stop all the modules that were started
					return fmt.Errorf("starting a module: %s, %w", id, err)
				}

				if stop != nil {
					mod.stop = stop
				}

				k.l.LogAttrs(ctx, slog.LevelInfo, "module started", attr.ModId(id))
				started[id] = struct{}{}
				newStarted = append(newStarted, id)
				k.l.LogAttrs(ctx, slog.LevelInfo, "notifying dependent modules", slog.Any("dependents", dependents[id]))
				for _, dep := range dependents[id] {
					left[dep].dependenciesLeft = slices.DeleteFunc(left[dep].dependenciesLeft, func(s string) bool { return s == id })
				}
				addListener(mod)
				for _, bufd := range k.b.cloneBuf() {
					err := k.b.Send(ctx, bufd)
					if err != nil {
						return fmt.Errorf("sending buffored event, %w", err)
					}
				}
				err = k.b.Send(ctx, newKernelEvent(ModuleStarted{
					ID:        id,
					Order:     i,
					StartTook: time.Since(start),
				}))

				if err != nil {
					return fmt.Errorf("sending ModuleStarted event, %w", err)
				}
			}

			i++
		}

		for _, id := range newStarted {
			delete(left, id)
		}
	}

	k.b.shouldBuf.Store(false)

	err := k.checkHealth(ctx)
	stopCtx := ctx
	if ctx.Err() != nil {
		// context is already canceled, so we need to create a new one
		stopCtx = context.Background()
		if k.stopTimeout != 0 {
			var cf func()
			stopCtx, cf = context.WithTimeout(ctx, k.stopTimeout)
			defer cf()
		}

	}

	// TODO: consider returning nil, when non-nil errors are `context.Canceled`
	stopErr := k.stop(stopCtx)
	return errors.Join(err, stopErr)
}

func (k *Kernel) stop(ctx context.Context) error {
	var errs []error
	for _, module := range k.startedModules {
		if module.stop != nil {
			errs = append(errs, module.stop(ctx))
		}
	}

	k.cancelListener(fmt.Errorf("kernel stopped"))
	return errors.Join(errs...)
}

func (k *Kernel) handleEvent(ctx context.Context, event Event) {
	k.l.LogAttrs(ctx, slog.LevelInfo, "kernel received the event", eventAttr(event))

	switch p := event.Payload.(type) {
	case ModuleStatusChanged:
		if p.To == ModuleStateNOK {
			k.state.Set(ModuleStateNOK)
			return
		}
	case ModuleStarted:
		for _, module := range k.modules {
			if module.id == p.ID {
				k.startedModules = append(k.startedModules, module)
				return
			}
		}

		k.l.LogAttrs(ctx, slog.LevelError, "received event with module ModID not matching any of the modules", attr.ModId(p.ID))
	default:
		k.l.LogAttrs(ctx, slog.LevelError, "unknown event type", attr.TypeOf("event", event))
	}
}

func (k *Kernel) checkHealth(ctx context.Context) error {
	ticker := time.NewTicker(k.healthTick)
	for {
		select {
		case <-ctx.Done():
			k.l.LogAttrs(ctx, slog.LevelInfo, "context canceled, stopping the kernel", attr.Err(ctx.Err()))
			return ctx.Err()
		case <-ticker.C:
			ks := k.state.Get()
			k.l.LogAttrs(ctx, slog.LevelDebug, "read kernel state", slog.Any("state", ks))
			if ks == ModuleStateNOK {
				return ErrKernelUnhealthy{
					cause: fmt.Errorf("kernel state is NOK"),
				}
			}
		}
	}
}

const EventSourceKernel = "kernel"

type BusCfg struct {
	SendEvtTimeout time.Duration
	Slog           plog.Slog
}

type Bus struct {
	listeners map[string]chan<- Event
	timeout   time.Duration
	l         plog.Slog

	shouldBuf atomic.Bool
	mux       sync.RWMutex
	buf       []Event
}

func NewBus(cfg *BusCfg) (*Bus, error) {
	timeout := time.Second
	var l plog.Slog

	if cfg != nil {
		timeout = cfg.SendEvtTimeout
		l = cfg.Slog
	}

	return &Bus{
		listeners: make(map[string]chan<- Event),
		timeout:   timeout,
		l:         l,
	}, nil
}

// Send sends the Event to all the listeners.
// Every listener receives all the events, filtering must be implemented on the reader side.
// Context is used to limit total time spent on sending, but additionally, a try of sending the message to the listener can time out.
// Bus always tries to send the event to all the listeners, but will stop once context was canceled.
func (b *Bus) Send(ctx context.Context, e Event) error {
	if b == nil {
		return nil
	}

	l := b.l.With(attr.RandomID("eid"))
	l.LogAttrs(ctx, slog.LevelInfo, "sending event", eventAttr(e))

	// ignore the cancellation signal to deliver the event to all the listeners - use the timeout instead
	noCancCtx := context.WithoutCancel(ctx)
	sendCtx, cf := context.WithTimeout(noCancCtx, time.Second)
	defer cf()

	var errs []error
	for id, c := range b.listeners {
		l.LogAttrs(ctx, slog.LevelDebug, "sending event to listener", attr.ModId(id))
		err := b.sendEvent(sendCtx, e, c)
		if err != nil {
			errs = append(errs, fmt.Errorf("sending event: %s to %s, %w", e, id, err))
		}
	}

	// still notify about the parent context being cancelled
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}

	b.tryBuf(e)
	return errors.Join(errs...)
}

func (b *Bus) sendEvent(ctx context.Context, e Event, c chan<- Event) error {
	if b.timeout != 0 {
		// every event gets its own timeout if specified
		var cf func()
		ctx, cf = context.WithTimeout(ctx, b.timeout)
		defer cf()
	}

	select {
	case c <- e:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Bus) cloneBuf() []Event {
	b.mux.RLock()
	defer b.mux.RUnlock()
	return slices.Clone(b.buf)
}

func (b *Bus) tryBuf(e Event) {
	if b.shouldBuf.Load() {
		b.mux.Lock()
		defer b.mux.Unlock()

		b.buf = append(b.buf, e)
	}
}
