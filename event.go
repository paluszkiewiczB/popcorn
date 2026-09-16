package popcorn

import (
	"fmt"
	"log/slog"
	"reflect"
	"time"
)

// Event is an asynchronous message sent over the Bus. It is a lightweight value:
// it is copied by value and is safe to compare and store.
//
// A subscriber receives every event and filters locally, by Kind or by
// type-asserting Payload. Source is set by the Publisher when the event is sent
// and cannot be set by the caller.
type Event struct {
	// Kind identifies the event. NewEvent derives it from the payload type;
	// NewEventOf sets it explicitly. It should be globally unique.
	Kind string

	// At is when the event was created. Listeners may use it to drop stale events.
	At time.Time

	// Payload is the event-specific value. Listeners assert its concrete type.
	Payload any

	source string
}

// NewEvent builds an Event whose Kind is the name of the payload type T. The
// source is filled in by the Publisher on Send; callers never set it.
func NewEvent[T any](payload T) Event {
	return newEvent(kindOf(payload), payload)
}

// NewEventOf builds an Event with an explicit Kind. It is the escape hatch for
// cases where the type-derived kind is not appropriate.
func NewEventOf(kind string, payload any) Event {
	return newEvent(kind, payload)
}

func newEvent(kind string, payload any) Event {
	return Event{Kind: kind, At: time.Now(), Payload: payload}
}

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

// ModuleStarted is emitted by the Kernel after a Module has started
// successfully.
type ModuleStarted struct {
	ID        string
	Order     int
	StartTook time.Duration
}

// ModuleStateChanged reports a module's health. A module publishes it through a
// Publisher bound to its own id, which the kernel validates against the
// registered modules:
//
//	if err := bus.Publisher(id).Send(ctx, popcorn.NewEvent(
//		popcorn.ModuleStateChanged{To: popcorn.ModuleStateOK},
//	)); err != nil {
//		// handle the send failure
//	}
//
// The kernel treats [ModuleStateNOK] as a shutdown trigger; the other states are
// meant for independent subscribers such as probes.
type ModuleStateChanged struct {
	To    ModuleState
	Cause error
}

// KernelStateChanged reports a kernel lifecycle transition. The kernel publishes
// both ends of every transition. Readiness, liveness, and startup probes consume
// it like any other event.
type KernelStateChanged struct {
	From, To KernelState
	Cause    error
}

// ModuleState is the health state a module reports.
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

// IsHealthy reports whether the state is [ModuleStateOK].
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
