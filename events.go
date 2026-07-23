package popcorn

import (
	"log/slog"
	"reflect"
	"strconv"
	"time"
)

// Event is an asynchronous message sent over the [Bus].
// Listeners filter by [Event.Kind] and assert [Event.Payload] to the expected type.
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

// LogValue implements [slog.LogValuer].
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

// NewEvent creates a BaseEvent and fills its [Event.Payload].
func NewEvent[T any](src string, payload T) Event {
	e := BaseEvent[T](src)
	e.Payload = payload
	return e
}

func eventAttr(e Event) slog.Attr {
	return slog.Any("evt", e)
}

// ModuleStarted is emitted by the [Kernel] after a [Module] has started successfully.
type ModuleStarted struct {
	ID        string
	Order     int
	StartTook time.Duration
}

// ModuleStatusChanged is emitted by a [Module] when its health state changes.
type ModuleStatusChanged struct {
	ID       string
	From, To ModuleState
	Cause    string
}
