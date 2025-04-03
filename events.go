package popcorn

import (
	"log/slog"
	"reflect"
	"strconv"
	"time"
)

type Event struct {
	// Kind should be globally unique to allow listeners distinguish between different Events.
	Kind string
	// Source is [EventSourceKernel] when its source is the [Kernel], otherwise must be ModID of the [Module]
	Source string
	// When the Event has happened. It does not have to be time when it's submitted to the [Bus].
	// Delays are acceptable.
	// It can be used to drop outdated [Event] by the listener.
	At time.Time
	// Payload is Kind-specific, thus a listener needs to assert the content type.
	Payload any
}

func (e Event) LogValue() slog.Value {
	return slog.StringValue("{Kind:" + e.Kind + ", Source:" + e.Source + ", At:" + strconv.Itoa(int(e.At.UnixNano())) + "}")
}

// BaseEvent has the [Event.Kind] based on T, and current timestamp.
func BaseEvent[T any](src string) Event {
	kind := eventKind[T]()
	return Event{
		Kind:   kind,
		Source: src,
		At:     time.Now(),
	}
}

func eventKind[T any]() string {
	var t T
	return reflect.TypeOf(t).Name()
}

// NewEvent creates a BaseEvent and fills its [Event.Payload]
func NewEvent[T any](src string, payload T) Event {
	e := BaseEvent[T](src)
	e.Payload = payload
	return e
}

func newKernelEvent[T any](payload T) Event {
	e := BaseEvent[T](EventSourceKernel)
	e.Payload = payload
	return e
}

func eventAttr(e Event) slog.Attr {
	return slog.Any("evt", e)
}

// Those are predefined Event types, which the Kernel knows and listens for.
type (
	ModuleStarted struct {
		ID        string
		Order     int
		StartTook time.Duration
	}

	ModuleStatusChanged struct {
		ID       string
		From, To ModuleState
		Cause    string
	}
)
