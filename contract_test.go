package popcorn_test

// The contract kit: shared helpers for black-box tests of the popcorn API.

import (
	"context"
	"errors"
	"testing/synctest"
	"time"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

// never is the watchdog for every blocking expectation: if the API under
// contract stalls instead of delivering, we fail fast rather than hang.
const never = 5 * time.Second

// Shared test errors, declared once so the suite does not build dynamic sentinels.
var (
	errBoom              = errors.New("boom")
	errConnectionRefused = errors.New("connection refused")
	errDiskFull          = errors.New("disk full")
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
	taskID    = "task"
	depID     = "dep"
	fastID    = "fast"
	failingID = "failing"
)

// within returns a context that outlives the whole test run of one spec.
func within() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), never)
}

// mustRecv reads one event from ch, failing if nothing arrives in time.
func mustRecv(is *is.I, ch <-chan popcorn.Event) popcorn.Event {
	is.Helper()

	select {
	case e := <-ch:
		return e
	case <-time.After(never):
		is.Fail() // timed out waiting for an event
		return popcorn.Event{}
	}
}

// drained collects everything the subscription can still deliver once every
// producer has settled. It must be called inside a synctest bubble, where Wait
// returns only when the rest of the bubble is durably blocked, making the drain
// exact rather than a quiescence guess.
func drained(is *is.I, ch <-chan popcorn.Event) []popcorn.Event {
	is.Helper()

	var out []popcorn.Event
	for {
		synctest.Wait()
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// recipe returns a valid Module recipe with the given id.
func recipe(id string, start func(ctx context.Context) (popcorn.StopFunc, error)) *popcorn.ModRecipe {
	return &popcorn.ModRecipe{ID: id, Start: start}
}
