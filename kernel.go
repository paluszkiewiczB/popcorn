package popcorn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/paluszkiewiczB/popcorn/plog"
	"github.com/paluszkiewiczB/popcorn/plog/attr"
)

// KernelUnhealthyError is returned by [Kernel.Start] when the kernel health state becomes NOK.
type KernelUnhealthyError struct {
	// Cause is the underlying reason the kernel became unhealthy.
	Cause error
}

// Error implements the error interface.
func (e KernelUnhealthyError) Error() string {
	return fmt.Sprintf("kernel is unhealthy: %s", e.Cause)
}

// Unwrap returns the cause of the error.
func (e KernelUnhealthyError) Unwrap() error {
	return e.Cause
}

// ModuleState represents the health state of a module or the kernel.
type ModuleState int32

func (s ModuleState) asInt() int32 {
	return int32(s)
}

const (
	// ModuleStateUnknown is the initial state of a module.
	ModuleStateUnknown ModuleState = iota
	// ModuleStateOK means the module is healthy.
	ModuleStateOK
	// ModuleStateNOK means the module is unhealthy.
	ModuleStateNOK
	// ModuleStateTempNOK means the module is temporarily unhealthy.
	ModuleStateTempNOK
)

// EventSourceKernel is the reserved source identifier for events emitted by the [Kernel].
const EventSourceKernel = "kernel"

// ErrNilModule is returned when a nil module is provided to the kernel.
var ErrNilModule = errors.New("nil module")

// ErrDuplicateModuleID is returned when a module with the same id is registered twice.
var ErrDuplicateModuleID = errors.New("duplicate module id")

// ErrUnknownDependency is returned when a module depends on an unknown module.
var ErrUnknownDependency = errors.New("module depends on unknown module")

// ErrSelfDependency is returned when a module depends on itself.
var ErrSelfDependency = errors.New("module depends on itself")

// ErrCircularDependency is returned when module dependencies form a cycle.
var ErrCircularDependency = errors.New("circular dependency detected")

// ErrContextCanceled is returned when the context is canceled.
var ErrContextCanceled = errors.New("context canceled")

const errModuleNOK = "module NOK"

const (
	defaultHealthTick  = time.Second
	defaultStopTimeout = 5 * time.Second
)

// Kernel manages the lifecycle of a set of [Module]s.
type Kernel struct {
	modules        []Module
	order          []Module
	started        []Module
	stopFuncs      map[string]StopFunc
	bus            *Bus
	log            plog.Logger
	healthTick     time.Duration
	stopTimeout    time.Duration
	state          *ModuleStateStore
	nokModuleID    string
	cancelListener func()
}

type kernelConfig struct {
	bus         *Bus
	log         plog.Logger
	modules     []Module
	healthTick  time.Duration
	stopTimeout time.Duration
}

// KernelOption configures a [Kernel].
type KernelOption func(*kernelConfig) error

// WithBus sets the event bus used by the kernel.
func WithBus(b *Bus) KernelOption {
	return func(c *kernelConfig) error {
		c.bus = b
		return nil
	}
}

// WithLogger sets the logger used by the kernel.
func WithLogger(l *slog.Logger) KernelOption {
	return func(c *kernelConfig) error {
		c.log = l
		return nil
	}
}

// WithModules adds modules to the kernel.
func WithModules(modules ...Module) KernelOption {
	return func(c *kernelConfig) error {
		c.modules = append(c.modules, modules...)
		return nil
	}
}

// WithHealthTick sets the interval between health checks.
func WithHealthTick(d time.Duration) KernelOption {
	return func(c *kernelConfig) error {
		c.healthTick = d
		return nil
	}
}

// WithStopTimeout sets the timeout for the shutdown phase.
func WithStopTimeout(d time.Duration) KernelOption {
	return func(c *kernelConfig) error {
		c.stopTimeout = d
		return nil
	}
}

// NewKernel creates a new [Kernel] with the given options.
func NewKernel(opts ...KernelOption) (*Kernel, error) {
	cfg := kernelConfig{
		bus:         nil,
		log:         slog.Default(),
		modules:     nil,
		healthTick:  defaultHealthTick,
		stopTimeout: defaultStopTimeout,
	}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, fmt.Errorf("applying kernel option: %w", err)
		}
	}

	if cfg.bus == nil {
		b, err := NewBus()
		if err != nil {
			return nil, fmt.Errorf("creating default bus: %w", err)
		}

		cfg.bus = b
	}

	ids := make(map[string]struct{}, len(cfg.modules))
	for _, m := range cfg.modules {
		if m == nil {
			return nil, fmt.Errorf("%w", ErrNilModule)
		}

		if _, ok := ids[m.ID()]; ok {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateModuleID, m.ID())
		}

		ids[m.ID()] = struct{}{}
	}

	depGraph, err := newDepGraph(cfg.modules)
	if err != nil {
		return nil, fmt.Errorf("resolving dependencies: %w", err)
	}

	return &Kernel{
		modules:     cfg.modules,
		order:       depGraph.order,
		bus:         cfg.bus,
		stopFuncs:   make(map[string]StopFunc),
		started:     nil,
		healthTick:  cfg.healthTick,
		stopTimeout: cfg.stopTimeout,
		log:         cfg.log,
		state:       &ModuleStateStore{},
		nokModuleID: "",
	}, nil
}

// Start starts all modules in dependency order and blocks until the context is canceled,
// a module reports NOK, or all [TaskModule]s are done.
func (k *Kernel) Start(ctx context.Context) error {
	// CR: are you sure it's thread safe?
	// SetBuffering(false), reading from the buffer and then clearing it are not atomic
	// so an event emitted just after the startup might be LOST due to time-of-check vs time-of-delete data race
	k.bus.SetBuffering(true)
	defer k.bus.SetBuffering(false)
	defer k.bus.ClearBuffer()

	// CR: should we use background here?
	kernelEvents := make(chan Event, 1)
	listenCtx := context.WithoutCancel(ctx)
	listenCtx, cancel := context.WithCancel(listenCtx)

	k.cancelListener = cancel
	go k.listener(listenCtx, kernelEvents)

	if err := k.bus.Subscribe(EventSourceKernel, kernelEvents); err != nil {
		return fmt.Errorf("subscribing kernel to bus: %w", err)
	}
	defer k.bus.Unsubscribe(EventSourceKernel)

	if err := k.startModules(ctx); err != nil {
		return err
	}

	return k.run(ctx)
}

func (k *Kernel) listener(ctx context.Context, ch <-chan Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			k.handleEvent(e)
		}
	}
}

func (k *Kernel) startModules(ctx context.Context) error {
	k.log.LogAttrs(ctx, slog.LevelInfo, "starting the kernel", slog.Int("modCount", len(k.modules)))

	for _, m := range k.order {
		id := m.ID()
		k.log.LogAttrs(ctx, slog.LevelInfo, "starting module", attr.ModID(id))

		start := time.Now()

		stopFunc, err := m.Start(ctx)
		if err != nil {
			return fmt.Errorf("starting module %s: %w", id, err)
		}

		k.started = append(k.started, m)
		k.stopFuncs[id] = stopFunc

		if er, ok := m.(EventReceiver); ok {
			if ch := er.Events(); ch != nil {
				if err := k.bus.Subscribe(id, ch); err != nil {
					return fmt.Errorf("subscribing module %s to bus: %w", id, err)
				}
				// CR: event will not receive ALL the events, just those buffered before it started up
				// but it will never receive an event buffered AFTER it started up, right? since buffering was enabled
				k.replayStartupEvents(ctx, ch)
			}
		}

		if err := k.bus.Send(ctx, NewEvent(EventSourceKernel, ModuleStarted{
			ID:        id,
			Order:     len(k.started) - 1,
			StartTook: time.Since(start),
		})); err != nil {
			return fmt.Errorf("sending ModuleStarted event: %w", err)
		}
	}

	return nil
}

func (k *Kernel) replayStartupEvents(_ context.Context, ch chan<- Event) {
	for _, e := range k.bus.Buffer() {
		select {
		case ch <- e:
		default:
			return
		}
	}
}

type depGraph struct {
	order []Module
}

func newDepGraph(modules []Module) (*depGraph, error) {
	byID := buildIndex(modules)
	if err := validateDeps(modules, byID); err != nil {
		return nil, err
	}

	inDegree, dependents := buildDegree(modules)

	order := topologicalSort(modules, inDegree, dependents, byID)
	if len(order) != len(modules) {
		// CR: please explain why comapring the lens is enough to detect it?
		return nil, ErrCircularDependency
	}

	return &depGraph{order: order}, nil
}

func buildIndex(modules []Module) map[string]Module {
	byID := make(map[string]Module, len(modules))
	for _, m := range modules {
		byID[m.ID()] = m
	}

	return byID
}

func validateDeps(modules []Module, byID map[string]Module) error {
	for _, m := range modules {
		for _, dep := range m.Dependencies() {
			if _, ok := byID[dep]; !ok {
				return fmt.Errorf("%w %q", ErrUnknownDependency, dep)
			}
			if dep == m.ID() {
				return fmt.Errorf("%w: %q", ErrSelfDependency, m.ID())
			}
		}
	}
	return nil
}

func buildDegree(modules []Module) (map[string]int, map[string][]string) {
	// CR: what the fuck does the `degree` mean here?
	inDegree := make(map[string]int, len(modules))

	dependents := make(map[string][]string, len(modules))
	for _, m := range modules {
		inDegree[m.ID()] = len(m.Dependencies())
		for _, dep := range m.Dependencies() {
			dependents[dep] = append(dependents[dep], m.ID())
		}
	}

	return inDegree, dependents
}

func topologicalSort(
	modules []Module,
	inDegree map[string]int,
	dependents map[string][]string,
	byID map[string]Module,
) []Module {
	var ready []string

	for id, deg := range inDegree {
		if deg == 0 {
			ready = append(ready, id)
		}
	}

	slices.Sort(ready)

	order := make([]Module, 0, len(modules))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]

		order = append(order, byID[id])

		for _, depID := range dependents[id] {
			inDegree[depID]--
			if inDegree[depID] == 0 {
				ready = append(ready, depID)
				slices.Sort(ready)
			}
		}
	}

	return order
}

func (k *Kernel) run(ctx context.Context) error {
	taskDone := k.watchTasks()

	ticker := time.NewTicker(k.healthTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			k.log.LogAttrs(ctx, slog.LevelInfo, "context canceled, stopping the kernel", attr.Err(ctx.Err()))
			return k.stop(ctx, fmt.Errorf("%w: %w", ErrContextCanceled, ctx.Err()))
		case <-taskDone:
			// CR: what if we have a mix of task modules and long-runnnig modules?
			k.log.LogAttrs(ctx, slog.LevelInfo, "all task modules finished, stopping the kernel")
			return k.stop(ctx, nil)
		case <-ticker.C:
			if k.state.Get() == ModuleStateNOK {
				return k.stop(ctx, KernelUnhealthyError{Cause: fmt.Errorf("%s: %s", errModuleNOK, k.nokModuleID)}) //nolint:err113
			}
		}
	}
}

func (k *Kernel) watchTasks() <-chan struct{} {
	var wg sync.WaitGroup

	hasTasks := false

	for _, m := range k.started {
		if tm, ok := m.(TaskModule); ok {
			hasTasks = true

			wg.Add(1)
			go func(doneCh <-chan struct{}) {
				defer wg.Done()

				<-doneCh
			}(tm.Done())
		}
	}

	if !hasTasks {
		return nil
	}

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	return done
}

func (k *Kernel) stop(ctx context.Context, cause error) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), k.stopTimeout)
	defer cancel()

	var errs []error
	if cause != nil {
		errs = append(errs, cause)
	}

	for _, m := range slices.Backward(k.started) {
		if sf := k.stopFuncs[m.ID()]; sf != nil {
			errs = append(errs, sf(stopCtx))
		}
	}

	if k.cancelListener != nil {
		k.cancelListener()
	}

	return errors.Join(errs...)
}

func (k *Kernel) handleEvent(e Event) {
	if msc, ok := e.Payload.(ModuleStatusChanged); ok && msc.To == ModuleStateNOK {
		k.state.Set(ModuleStateNOK)
		k.nokModuleID = msc.ID
	}
}

// RetainContext returns a new context with new [context.CancelFunc].
func RetainContext(ctx context.Context) (retained context.Context, cancel func()) {
	retained = context.WithoutCancel(ctx)
	return context.WithCancel(retained)
}

// RetainContextCause returns a new context with new [context.CancelCauseFunc].
func RetainContextCause(ctx context.Context) (retained context.Context, cancel func(error)) {
	retained = context.WithoutCancel(ctx)
	return context.WithCancelCause(retained)
}
