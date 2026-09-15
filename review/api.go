// Package popcorn is the PROPOSED public API of the popcorn microframework.
//
// This file is a design draft only. It contains no implementation: every function
// body panics with "not implemented". Its purpose is to pin down the shape,
// ownership rules, and ergonomics of the API before any code is written.
//
// It lives in its own nested module (review/go.mod) so it is excluded from the root
// module's `./...` patterns and does not affect `go build ./...`, `go vet ./...`, or
// `task lint`. Type-check the draft with:
//
//	cd review && go build ./...
//
// Design philosophy:
//
//   - One owner per channel. The Bus creates, writes, and closes subscription
//     channels; modules only read them. No module ever writes to a channel it does
//     not own, and no module closes a bus-owned channel.
//   - One communication mechanism. The event Bus covers module->kernel,
//     module->module, and kernel->module traffic. Health is just an event.
//   - Explicit subscriptions. A module subscribes itself, when it is ready, via the
//     injected Bus. There are no receiver interfaces and no kernel runtime checks.
//   - Identity is enforced. Publishers are bound to an id, so Event.Source cannot be
//     spoofed and Send can skip the sender's own subscription.
//   - Bounded everything. Each subscription has a bounded ring and the Bus keeps a
//     bounded replay history. A slow subscriber can only overflow its own ring.
//   - Health is best-effort. It travels on the shared Bus like any other event.
//   - Events are lightweight values. No pointers, no per-event wrappers.
package popcorn

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"
)

// ============================================================================
// Module model
// ============================================================================

// Module is a self-contained unit of functionality managed by the Kernel.
type Module interface {
	// ID is the unique, non-empty identifier of the module. By convention it is the
	// module's import path. The kernel reserves "kernel" for itself.
	ID() string

	// Dependencies returns the ids of modules that must be ready before this one
	// starts. A dependency is ready when its Start returns; a TaskModule dependency
	// is ready when its Done channel is closed.
	Dependencies() []string

	// Start initializes the module. A module that wants events subscribes itself
	// through the injected Bus, for example:
	//
	//	events, err := bus.Subscribe(m.ID(), popcorn.WithBacklog(64), popcorn.WithFilter(f))
	//
	// Start returns a StopFunc that the kernel invokes during shutdown; the StopFunc
	// may be nil.
	Start(ctx context.Context) (StopFunc, error)
}

// TaskModule is a Module that performs finite work and signals completion.
//
// Readiness: a module that depends on a TaskModule does not start until that
// TaskModule's Done channel is closed.
//
// Exit: the kernel auto-stops once every module is a TaskModule and all are done
// (a pure CLI-style run). If any long-running module is present the kernel runs
// until cancellation or failure, unless WithExitWhenIdle is set.
type TaskModule interface {
	Module

	// Done is closed when the module's work is finished.
	Done() <-chan struct{}
}

// StartFunc is a function-based Module body.
type StartFunc func(ctx context.Context) (StopFunc, error)

// StopFunc cleans up a module. It receives a fresh shutdown context derived from the
// run context (values preserved, own deadline).
type StopFunc func(ctx context.Context) error

// StopFuncFromCloser adapts an io.Closer to a StopFunc. It returns nil for a nil
// closer, so it is safe to write `return popcorn.StopFuncFromCloser(res), nil`.
// REVIEW: StopCloser
func StopFuncFromCloser(c io.Closer) StopFunc { panic("not implemented") }

// ============================================================================
// Events
// ============================================================================

// Event is an asynchronous message sent over the Bus.
//
// Event is a lightweight value: it is copied by value and is safe to compare and
// store. Listeners filter by Kind and type-assert Payload against the expected type.
//
// Source is the only field managed by the framework: it is set by the bound
// Publisher and read through Source().
type Event struct {
	// Kind is derived from the payload type by NewEvent, or supplied explicitly by
	// NewEventOf. It should be globally unique.
	Kind string

	// At is when the event happened. Listeners may use it to drop stale events.
	At time.Time

	// Payload is the event-specific value. Listeners assert its concrete type.
	Payload any

	source string
}

// Source returns the id of the publisher. It is set by the bound Publisher when the
// event is sent and cannot be set by the caller.
func (e Event) Source() string { panic("not implemented") }

// NewEvent builds an Event whose Kind is the name of T. The source is filled in by
// the Publisher when the event is sent; callers never set it.
func NewEvent[T any](payload T) Event { panic("not implemented") }

// NewEventOf builds an Event with an explicit Kind. It is the escape hatch for cases
// where the type-derived kind is not appropriate.
func NewEventOf(kind string, payload any) Event { panic("not implemented") }

// ModuleStarted is emitted by the Kernel after a Module has started successfully.
type ModuleStarted struct {
	ID        string
	Order     int
	StartTook time.Duration
}

// ModuleStateChanged is emitted by a Module to report a health transition. The
// module is identified by Event.Source, which the Kernel validates against the
// registered modules. Modules report only the new state; the Kernel records the
// previous state and validates the transition.
type ModuleStateChanged struct {
	To    ModuleState
	Cause error
}

// KernelStateChanged is emitted by the Kernel on every lifecycle transition. The
// Kernel owns this lifecycle, so it reports both ends of the transition.
// Liveness/readiness/startup probes are ordinary consumers of this event.
type KernelStateChanged struct {
	From, To KernelState
	Cause    error
}

// ============================================================================
// State
// ============================================================================

// ModuleState is the health state of a module.
type ModuleState int32

const (
	ModuleStateUnknown ModuleState = iota
	ModuleStateOK
	ModuleStateTempNOK
	ModuleStateNOK
)

// String renders the state for logs and errors.
func (s ModuleState) String() string { panic("not implemented") }

// IsHealthy reports whether the state is OK.
func (s ModuleState) IsHealthy() bool { panic("not implemented") }

// KernelState is the lifecycle state of the Kernel.
type KernelState int32

const (
	KernelStateUnknown KernelState = iota
	KernelStateStarting
	KernelStateRunning
	KernelStateStopping
	KernelStateStopped
)

// String renders the state for logs and errors.
func (s KernelState) String() string { panic("not implemented") }

// ============================================================================
// Publishing
// ============================================================================

// Publisher sends events on behalf of a single module. The Bus creates it via
// Bus.Publisher and the id is bound, so Event.Source is set by the Bus and cannot be
// spoofed.
type Publisher interface {
	Send(ctx context.Context, e Event) error
}

// ============================================================================
// Bus
// ============================================================================

// Bus is an in-process event bus with per-subscription, bounded, isolated delivery.
// It is the single owner of every subscription channel and of the bounded replay
// history.
//
// Delivery: Send enqueues into each matching subscription without blocking. When a
// subscription ring is full the oldest event is overwritten. A slow subscriber can
// only overflow its own ring; it can never stall Send, another subscriber, or the
// kernel.
// REVIEW: buffering on started consumer could be simply moved to a buffered channel.
// The Bus ring-buffer is mostly there to support replaying event which were emitted before module was started.
// Should we buffer the events ONLY before Start and then omit it completely?
type Bus struct {
	// unexported fields omitted from the draft
}

// NewBus creates a Bus.
func NewBus(opts ...BusOption) (*Bus, error) { panic("not implemented") }

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
	panic("not implemented")
}

// Unsubscribe tears the subscription down and closes its channel. It is safe to call
// more than once.
func (b *Bus) Unsubscribe(id string) { panic("not implemented") }

// Publisher returns a Publisher bound to id. Send sets Event.Source to id and skips
// the subscription with the same id.
func (b *Bus) Publisher(id string) Publisher { panic("not implemented") }

// BusOption configures a Bus.
type BusOption func(*busConfig) error

// WithReplayBuffer sets the size of the Bus replay history. When greater than zero,
// a newly created subscription receives up to WithBacklog of the most recent
// matching events before live delivery begins. The default is 0: subscribers see
// only events sent after they subscribe.
func WithReplayBuffer(n int) BusOption { panic("not implemented") }

// WithBusLogger sets the Bus logger.
func WithBusLogger(l *slog.Logger) BusOption { panic("not implemented") }

// SubOption configures a single subscription.
type SubOption func(*subConfig) error

// WithBacklog sets the subscription's bounded ring size. It also caps how many
// history events are replayed at subscribe time. The default is 0: a rendezvous
// subscription with no buffering.
func WithBacklog(n int) SubOption { panic("not implemented") }

// WithFilter restricts which events are enqueued and replayed for this subscription.
// f runs outside all locks and is panic-recovered; a panic drops the event. A nil
// filter accepts everything.
func WithFilter(f func(Event) bool) SubOption { panic("not implemented") }

// busConfig, subConfig are private option targets.
type busConfig struct {
	replayBuffer int
	log          *slog.Logger
}

type subConfig struct {
	backlog int
	filter  func(Event) bool
}

// ============================================================================
// Kernel
// ============================================================================

// Kernel manages the lifecycle of a set of Modules. It is single-shot: calling Start
// more than once returns ErrKernelStarted.
//
// The Kernel owns no event channels. It publishes lifecycle events through a bound
// Publisher and subscribes to ModuleStateChanged for health, best-effort.
type Kernel struct {
	// unexported fields omitted from the draft
}

// NewKernel creates a Kernel. It validates modules, resolves the dependency graph,
// and rejects cycles.
func NewKernel(opts ...KernelOption) (*Kernel, error) { panic("not implemented") }

// Start starts modules in dependency order and blocks until the context is canceled,
// a module reports NOK, or the kernel becomes idle. A module becomes startable once
// every dependency is ready (TaskModule dependencies must be Done).
//
// It returns ErrKernelStopped (wrapped) on a graceful stop, KernelUnhealthyError when
// a module reported NOK, or the joined shutdown errors.
func (k *Kernel) Start(ctx context.Context) error { panic("not implemented") }

// KernelOption configures a Kernel.
type KernelOption func(*kernelConfig) error

// WithBus sets the event bus. When omitted, the kernel creates one.
func WithBus(b *Bus) KernelOption { panic("not implemented") }

// WithLogger sets the kernel logger.
func WithLogger(l *slog.Logger) KernelOption { panic("not implemented") }

// WithModules registers modules.
func WithModules(modules ...Module) KernelOption { panic("not implemented") }

// WithHealthTick sets the health polling interval.
func WithHealthTick(d time.Duration) KernelOption { panic("not implemented") }

// WithStopTimeout sets the overall shutdown budget. Each module's StopFunc gets a
// context carved out of this budget.
func WithStopTimeout(d time.Duration) KernelOption { panic("not implemented") }

// WithParallelism caps how many modules start/stop concurrently. 1 is sequential; 0
// means GOMAXPROCS.
func WithParallelism(p int) KernelOption { panic("not implemented") }

// WithExitWhenIdle controls auto-shutdown. When true the kernel stops as soon as all
// TaskModules are done, even if long-running modules exist. When false (default) it
// auto-stops only when every module is a TaskModule.
func WithExitWhenIdle(v bool) KernelOption { panic("not implemented") }

type kernelConfig struct {
	bus          *Bus
	log          *slog.Logger
	modules      []Module
	healthTick   time.Duration
	stopTimeout  time.Duration
	parallelism  int
	exitWhenIdle bool
}

// ModRecipe is a declarative recipe for building a Module. When Done is non-nil the
// resulting module also satisfies TaskModule.
//
// NewModule returns the Module interface rather than a concrete type because the
// concrete implementation depends on whether Done is set. Event-consuming modules
// subscribe through the injected Bus inside Start and need no special interface.
type ModRecipe struct {
	ID           string
	Dependencies []string
	Start        StartFunc
	Done         <-chan struct{} // optional; makes the module a TaskModule
}

// NewModule builds a Module from a recipe, applying the same validation as the
// kernel (non-empty, non-reserved id, non-nil Start, defensive copy of dependencies).
func NewModule(recipe ModRecipe) (Module, error) { panic("not implemented") }

// ============================================================================
// Errors
// ============================================================================

var (
	// ErrNilModule is returned when a nil module is provided.
	ErrNilModule = errors.New("nil module")
	// ErrDuplicateModuleID is returned when two modules share an id.
	ErrDuplicateModuleID = errors.New("duplicate module id")
	// ErrUnknownDependency is returned when a module depends on an unknown module.
	ErrUnknownDependency = errors.New("module depends on unknown module")
	// ErrSelfDependency is returned when a module depends on itself.
	ErrSelfDependency = errors.New("module depends on itself")
	// ErrDuplicateDependency is returned when a module lists a dependency twice.
	ErrDuplicateDependency = errors.New("duplicate dependency")
	// ErrCircularDependency is returned when module dependencies form a cycle.
	ErrCircularDependency = errors.New("circular dependency")
	// ErrModuleIDNotSet is returned when a module has an empty id.
	ErrModuleIDNotSet = errors.New("module id not set")
	// ErrModuleIDReserved is returned when a module uses a reserved id.
	ErrModuleIDReserved = errors.New("module id is reserved by the kernel")
	// ErrModuleStartNotSet is returned when a recipe has no Start function.
	ErrModuleStartNotSet = errors.New("start function not set for module")
	// ErrKernelStarted is returned by Start when called on an already-started kernel.
	ErrKernelStarted = errors.New("kernel already started")
	// ErrKernelStopped is returned (wrapped) by Start on a graceful shutdown.
	ErrKernelStopped = errors.New("kernel stopped")
)

// KernelUnhealthyError is returned by Kernel.Start when a module reports NOK.
type KernelUnhealthyError struct {
	// ModuleID is the module that reported the unhealthy state.
	ModuleID string
	// Cause is the underlying reason.
	Cause error
}

// Error implements the error interface.
func (e KernelUnhealthyError) Error() string { panic("not implemented") }

// Unwrap returns the cause.
func (e KernelUnhealthyError) Unwrap() error { panic("not implemented") }
