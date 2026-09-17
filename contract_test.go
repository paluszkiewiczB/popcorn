package popcorn_test

import (
	"context"
	"errors"
	"testing/synctest"
	"time"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

const never = 5 * time.Second

var (
	errBoom              = errors.New("boom")
	errConnectionRefused = errors.New("connection refused")
	errDiskFull          = errors.New("disk full")
	errPortInUse         = errors.New("port in use")
	errSpoof             = errors.New("spoof")
)

func noStop(context.Context) error { return nil }

func noopStart(context.Context) (popcorn.StopFunc, error) { return noStop, nil }

const (
	taskID    = "task"
	depID     = "dep"
	fastID    = "fast"
	failingID = "failing"
	peerID    = "peer"
)

func within() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), never)
}

func mustRecv(is *is.I, ch <-chan popcorn.Event) popcorn.Event {
	is.Helper()

	select {
	case e := <-ch:
		return e
	case <-time.After(never):
		is.Fail()
		return popcorn.Event{}
	}
}

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

func recipe(id string, start func(ctx context.Context) (popcorn.StopFunc, error)) *popcorn.ModRecipe {
	return &popcorn.ModRecipe{ID: id, Start: start}
}
