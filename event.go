package popcorn

import (
	"fmt"
	"log/slog"
	"reflect"
	"time"
)

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

// NewEvent builds an Event whose Kind is the name of T. The source is filled in by
// the Publisher when the event is sent; callers never set it.
func NewEvent[T any](payload T) Event {
	return newEvent(kindOf(payload), payload)
}

// NewEventOf builds an Event with an explicit Kind. It is the escape hatch for cases
// where the type-derived kind is not appropriate.
func NewEventOf(kind string, payload any) Event {
	return newEvent(kind, payload)
}

func newEvent(kind string, payload any) Event {
	return Event{Kind: kind, At: time.Now(), Payload: payload}
}

// kindOf derives a Kind from a payload value: the type name for named types, the
// full type string for unnamed ones. It never returns an empty kind.
func kindOf(payload any) string {
	t := reflect.TypeOf(payload)
	if t == nil {
		return "nil"
	}
	if name := t.Name(); name != "" {
		return name
	}
	return t.String()
}

// Source returns the id of the publisher. It is set by the bound Publisher when the
// event is sent and cannot be set by the caller.
func (e Event) Source() string { return e.source }

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

// ModuleState is the health state of a module.
type ModuleState int32

// ModuleState values describe a module's health.
const (
	ModuleStateUnknown ModuleState = iota
	ModuleStateOK
	ModuleStateTempNOK
	ModuleStateNOK
)

// String renders the state for logs and errors.
func (s ModuleState) String() string {
	switch s {
	case ModuleStateOK:
		return "ok"
	case ModuleStateTempNOK:
		return "temp-nok"
	case ModuleStateNOK:
		return "nok"
	case ModuleStateUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("module-state(%d)", int32(s))
	}
}

// IsHealthy reports whether the state is OK.
func (s ModuleState) IsHealthy() bool { return s == ModuleStateOK }

// KernelState is the lifecycle state of the Kernel.
type KernelState int32

// KernelState values describe the kernel lifecycle.
const (
	KernelStateUnknown KernelState = iota
	KernelStateStarting
	KernelStateRunning
	KernelStateStopping
	KernelStateStopped
)

// String renders the state for logs and errors.
func (s KernelState) String() string {
	switch s {
	case KernelStateStarting:
		return "starting"
	case KernelStateRunning:
		return "running"
	case KernelStateStopping:
		return "stopping"
	case KernelStateStopped:
		return "stopped"
	case KernelStateUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("kernel-state(%d)", int32(s))
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
