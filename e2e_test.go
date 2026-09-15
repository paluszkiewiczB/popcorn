package popcorn_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matryer/is"
	popcorn "github.com/paluszkiewiczB/popcorn"
)

// The hermetic end-to-end contracts: whole-system behavior inside one
// process - no network, no example code.
func Test_E2E(test *testing.T) {
	test.Run("ping story", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		const rounds = 3
		producerDone := make(chan struct{})
		collectorDone := make(chan struct{})

		// producer: sends `rounds` pings, then signals done.
		producerStart := func(ctx context.Context) (popcorn.StopFunc, error) {
			pub := b.Publisher("producer")
			for i := 0; i < rounds; i++ {
				if err := pub.Send(ctx, popcorn.NewEvent(tick{N: i})); err != nil {
					return nil, err
				}
			}
			close(producerDone)
			return nil, nil
		}

		// collector: subscribes into the bus itself, acknowledges health,
		// then finishes once a full round is seen.
		received := make(chan []int, 1)

		collectorStart := func(ctx context.Context) (popcorn.StopFunc, error) {
			ch, err := b.Subscribe("collector", popcorn.WithBacklog(rounds))
			if err != nil {
				return nil, err
			}

			pub := b.Publisher("collector")
			if err := pub.Send(ctx, popcorn.NewEvent(
				popcorn.ModuleStateChanged{To: popcorn.ModuleStateOK})); err != nil {
				return nil, err
			}

			go func() {
				var got []int
				for len(got) < rounds {
					e := <-ch
					if tk, ok := e.Payload.(tick); ok {
						got = append(got, tk.N)
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

	test.Run("failure path", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))
		stops := make(chan string, 8)

		failing, err := popcorn.NewModule(popcorn.ModRecipe{
			ID:    "failing",
			Start: healthStart("failing", b, popcorn.ModuleStateNOK, errors.New("connection refused")),
			Done:  closeOnStart(),
		})
		is.NoErr(err)

		// Healthy long-running peer: must receive a stop call once the fleet
		// shuts down due to the NOK report - failures shut the fleet down,
		// they do not abandon it.
		healthy, err := popcorn.NewModule(popcorn.ModRecipe{
			ID:           "healthy",
			Dependencies: []string{"failing"},
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

		err = k.Start(ctx)
		var unhealthy popcorn.KernelUnhealthyError
		is.True(errors.As(err, &unhealthy))
		is.Equal(unhealthy.ModuleID, "failing")

		select {
		case id := <-stops:
			is.Equal(id, "healthy") // the healthy peer must be stopped on NOK shutdown
		case <-time.After(never):
			is.Fail() // healthy peer never got a stop call
		}
	})
}
