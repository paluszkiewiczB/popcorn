package popcorn_test

// The contract kit: shared helpers for black-box tests of the popcorn API.
//
// Implementation is contract-driven — these files are written against
// api.go's documented behavior while every body still panics
// ("not implemented"). The suite is expected to be red at this point.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matryer/is"
	popcorn "github.com/paluszkiewiczB/popcorn"
)

// never is the watchdog for every blocking expectation: if the API under
// contract stalls instead of delivering, we fail fast rather than hang.
const never = 5 * time.Second

// Shared test errors, declared once so the suite does not build dynamic sentinels.
var (
	errBoom              = errors.New("boom")
	errConnectionRefused = errors.New("connection refused")
	errDiskFull          = errors.New("disk full")
	errDiskFullCheck     = errors.New("disk full") // distinct value with the same message
	errPortInUse         = errors.New("port in use")
	errSpoof             = errors.New("spoof")
)

// noStop is a non-nil no-op StopFunc for modules that need no cleanup, keeping the
// suite free of nil-value returns.
func noStop(context.Context) error { return nil }

// noopStart is a Start that does nothing and needs no cleanup.
func noopStart(context.Context) (popcorn.StopFunc, error) { return noStop, nil }

// Common module ids reused across lifecycle specs.
const (
	taskID = "task"
	depID  = "dep"
	fastID = "fast"
)

// within returns a context that outlives the whole test run of one spec.
func within() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), never)
}

// mustRecv reads one event from ch, failing if nothing arrives in time.
func mustRecv(is *is.I, ch <-chan popcorn.Event) popcorn.Event {
	is.Helper()

	c := make(chan popcorn.Event, 1)
	go func() { c <- <-ch }()

	select {
	case e := <-c:
		return e
	case <-time.After(never):
		is.Fail() // timed out waiting for an event
		return popcorn.Event{}
	}
}

// drained reads until ch stays empty for a short quiescence window and returns
// the accumulated events. Used to assert exact delivery sets without racing.
func drained(is *is.I, ch <-chan popcorn.Event) []popcorn.Event {
	is.Helper()

	var out []popcorn.Event
	quiesce := 200 * time.Millisecond
	for {
		select {
		case e := <-ch:
			out = append(out, e)
			quiesce = 200 * time.Millisecond
		case <-time.After(quiesce):
			return out
		}
	}
}

// step runs a named test step inline. A synctest bubble forbids t.Run, so the
// timing-sensitive suites use this runner instead of subtests; the assertion
// file:line still pinpoints failures.
func step(t *testing.T, name string, f func(*testing.T)) {
	t.Helper()
	t.Log("== " + name)
	f(t)
}

// recipe returns a valid Module recipe with the given id.
func recipe(id string, start func(ctx context.Context) (popcorn.StopFunc, error)) *popcorn.ModRecipe {
	return &popcorn.ModRecipe{ID: id, Start: func(ctx context.Context) (popcorn.StopFunc, error) {
		return start(ctx)
	}}
}
