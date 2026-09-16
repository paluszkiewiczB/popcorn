package popcorn_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

// The hermetic end-to-end contracts: whole-system behavior inside one
// process - no network, no example code.

func TestE2EPingStory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		const rounds = 3
		producerDone := make(chan struct{})
		collectorDone := make(chan struct{})

		// producer: sends `rounds` pings, then signals done.
		producerStart := func(ctx context.Context) (popcorn.StopFunc, error) {
			pub := b.Publisher("producer")
			for i := range rounds {
				if err := pub.Send(ctx, popcorn.NewEvent(tick{N: i})); err != nil {
					return nil, fmt.Errorf("producer send: %w", err)
				}
			}
			close(producerDone)
			return noStop, nil
		}

		// collector: subscribes into the bus itself, acknowledges health,
		// then finishes once a full round is seen.
		received := make(chan []int, 1)

		collectorStart := func(ctx context.Context) (popcorn.StopFunc, error) {
			ch, err := b.Subscribe("collector",
				popcorn.WithBacklog(rounds),
				popcorn.WithFilter(func(e popcorn.Event) bool {
					_, ok := e.Payload.(tick)
					return ok
				}))
			if err != nil {
				return nil, fmt.Errorf("collector subscribe: %w", err)
			}

			pub := b.Publisher("collector")
			if err := pub.Send(ctx, popcorn.NewEvent(
				popcorn.ModuleStateChanged{To: popcorn.ModuleStateOK},
			)); err != nil {
				return nil, fmt.Errorf("collector health: %w", err)
			}

			go func() {
				var got []int
				for e := range ch {
					if tk, ok := e.Payload.(tick); ok {
						got = append(got, tk.N)
						if len(got) == rounds {
							break
						}
					}
				}
				close(collectorDone)
				received <- got
			}()

			return func(context.Context) error { return nil }, nil
		}

		producer, err := popcorn.NewModule(popcorn.ModRecipe{
			ID:           "producer",
			Dependencies: []string{"collector"},
			Start:        producerStart,
			Done:         producerDone,
		})
		is.NoErr(err)

		collector, err := popcorn.NewModule(popcorn.ModRecipe{
			ID:    "collector",
			Start: collectorStart,
			Done:  collectorDone,
		})
		is.NoErr(err)

		k, err := popcorn.NewKernel(popcorn.WithBus(b),
			popcorn.WithParallelism(1),
			popcorn.WithModules(collector, producer))
		is.NoErr(err)

		ctx, cancel := within()
		defer cancel()

		// Both modules are TaskModules that report Done; the kernel must exit
		// gracefully once both are done.
		is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))

		select {
		case got := <-received:
			is.Equal(got, []int{0, 1, 2}) // collector must see every published ping exactly once
		case <-time.After(never):
			is.Fail() // collector never received a full round
		}
	})
}

func TestE2EFailurePath(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))
		stops := make(chan string, 8)

		failing, err := popcorn.NewModule(popcorn.ModRecipe{
			ID: failingID,
			Start: func(context.Context) (popcorn.StopFunc, error) {
				return stopped(stops, failingID), nil
			},
		})
		is.NoErr(err)

		// Healthy long-running peer: must receive a stop call once the fleet
		// shuts down due to the NOK report - failures shut the fleet down,
		// they do not abandon it.
		healthy, err := popcorn.NewModule(popcorn.ModRecipe{
			ID:           "healthy",
			Dependencies: []string{failingID},
			Start: func(context.Context) (popcorn.StopFunc, error) {
				return stopped(stops, "healthy"), nil
			},
		})
		is.NoErr(err)

		k, err := popcorn.NewKernel(popcorn.WithBus(b),
			popcorn.WithParallelism(1),
			popcorn.WithModules(failing, healthy))
		is.NoErr(err)

		ctx, cancel := within()
		defer cancel()

		// Let the whole fleet come up and only then report the failure, so the
		// NOK genuinely tests shutdown rather than start cancellation.
		done := make(chan error, 1)
		go func() { done <- k.Start(ctx) }()
		waitRunning(is, b)

		is.NoErr(b.Publisher(failingID).Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
			To:    popcorn.ModuleStateNOK,
			Cause: errConnectionRefused,
		})))

		unhealthy, ok := errors.AsType[popcorn.KernelUnhealthyError](<-done)
		is.True(ok)
		is.Equal(unhealthy.ModuleID, failingID)

		select {
		case id := <-stops:
			is.Equal(id, "healthy") // the healthy peer must be stopped on NOK shutdown
		case <-time.After(never):
			is.Fail() // healthy peer never got a stop call
		}
	})
}
