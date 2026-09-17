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

func TestE2EPingStory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		const rounds = 3
		producerDone := make(chan struct{})
		collectorDone := make(chan struct{})

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
			ID:    "producer",
			Start: producerStart,
			Done:  producerDone,
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

		is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))

		select {
		case got := <-received:
			is.Equal(got, []int{0, 1, 2})
		case <-time.After(never):
			is.Fail()
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
			is.Equal(id, "healthy")
		case <-time.After(never):
			is.Fail()
		}
	})
}
