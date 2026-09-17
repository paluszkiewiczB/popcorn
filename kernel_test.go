package popcorn_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

func stopped(ch chan<- string, id string) popcorn.StopFunc {
	return func(context.Context) error {
		ch <- id
		return nil
	}
}

func concurrentStop(entered *sync.WaitGroup) popcorn.StopFunc {
	return func(context.Context) error {
		entered.Done()
		entered.Wait()
		return nil
	}
}

func healthStart(
	id string,
	b *popcorn.Bus,
	st popcorn.ModuleState,
	cause error,
) func(ctx context.Context) (popcorn.StopFunc, error) {
	return func(ctx context.Context) (popcorn.StopFunc, error) {
		pub := b.Publisher(id)
		if err := pub.Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{To: st, Cause: cause})); err != nil {
			return nil, fmt.Errorf("report health: %w", err)
		}
		return stopped(make(chan string, 1), id), nil
	}
}

type observation struct {
	starts      []popcorn.ModuleStarted
	transitions []popcorn.KernelStateChanged
}

func startObserver(b *popcorn.Bus, out chan<- observation) {
	ch, err := b.Subscribe("observe", popcorn.WithBacklog(32), popcorn.WithFilter(func(e popcorn.Event) bool {
		switch e.Payload.(type) {
		case popcorn.ModuleStarted, popcorn.KernelStateChanged:
			return true
		default:
			return false
		}
	}))
	if err != nil {
		out <- observation{}
		return
	}

	var obs observation
	for e := range ch {
		switch p := e.Payload.(type) {
		case popcorn.ModuleStarted:
			obs.starts = append(obs.starts, p)
		case popcorn.KernelStateChanged:
			obs.transitions = append(obs.transitions, p)
			if p.To == popcorn.KernelStateStopped {
				out <- obs
				return
			}
		}
	}
}

func waitRunning(is *is.I, b *popcorn.Bus) {
	signaled := make(chan bool, 1)
	go func() {
		ch, err := b.Subscribe("observe-running", popcorn.WithBacklog(4), popcorn.WithFilter(func(e popcorn.Event) bool {
			if ksc, ok := e.Payload.(popcorn.KernelStateChanged); ok {
				return ksc.To == popcorn.KernelStateRunning
			}
			return false
		}))
		if err != nil {
			signaled <- false
			return
		}
		<-ch
		signaled <- true
	}()

	if !mustBool(is, signaled) {
		is.Fail()
	}
}

func mustBool(is *is.I, ch <-chan bool) bool {
	is.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(never):
		return false
	}
}

func closeOnStart() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func statesOf(ts []popcorn.KernelStateChanged) []popcorn.KernelState {
	out := make([]popcorn.KernelState, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.To)
	}
	return out
}

type key struct{}

func TestKernelValidation(t *testing.T) {
	t.Parallel()
	t.Run("nil module", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithModules(nil))
		is.True(errors.Is(err, popcorn.ErrNilModule))
	})

	t.Run("empty module id", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: ""}))
		is.True(errors.Is(err, popcorn.ErrModuleIDNotSet))
	})

	t.Run("duplicate module id", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(
			popcorn.WithModules(honest{id: "x"}, honest{id: "x"}),
		)
		is.True(errors.Is(err, popcorn.ErrDuplicateModuleID))
	})

	t.Run("reserved module id", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: "kernel"}))
		is.True(errors.Is(err, popcorn.ErrModuleIDReserved))
	})

	t.Run("unknown dependency", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: "a", deps: []string{"ghost"}}))
		is.True(errors.Is(err, popcorn.ErrUnknownDependency))
	})

	t.Run("self dependency", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: "a", deps: []string{"a"}}))
		is.True(errors.Is(err, popcorn.ErrSelfDependency))
	})

	t.Run("duplicate dependency entry", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithModules(
			honest{id: "a"},
			honest{id: "b", deps: []string{"a", "a"}},
		))
		is.True(errors.Is(err, popcorn.ErrDuplicateDependency))
	})

	t.Run("circular dependency rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithModules(
			honest{id: "a", deps: []string{"b"}},
			honest{id: "b", deps: []string{"a"}},
		))
		is.True(errors.Is(err, popcorn.ErrCircularDependency))
	})

	t.Run("invalid kernel options rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithParallelism(-1))
		is.True(err != nil)
	})

	t.Run("negative stop timeout rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithStopTimeout(-1))
		is.True(err != nil)
	})

	t.Run("logger option accepted", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithLogger(slog.New(slog.DiscardHandler)))
		is.NoErr(err)
	})

	t.Run("health tick channel accepted", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithHealthTick(make(chan time.Time)))
		is.NoErr(err)
	})
}

func TestKernelLifecycle(t *testing.T) {
	t.Parallel()
	t.Run("lifecycle events", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			obs := make(chan observation, 1)
			go startObserver(b, obs)

			task, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: taskID, Done: closeOnStart(),
				Start: noopStart,
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(task))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))

			select {
			case o := <-obs:
				is.Equal([]popcorn.KernelState{
					popcorn.KernelStateStarting,
					popcorn.KernelStateRunning,
					popcorn.KernelStateStopping,
					popcorn.KernelStateStopped,
				}, statesOf(o.transitions))

				ids := make([]string, len(o.starts))
				orders := make([]int, len(o.starts))
				for i, ms := range o.starts {
					ids[i] = ms.ID
					orders[i] = ms.Order
				}
				is.Equal(ids, []string{taskID})
				is.Equal(orders, []int{0})
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("single shot", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			task, err := popcorn.NewModule(popcorn.ModRecipe{
				ID:   taskID,
				Done: closeOnStart(),
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return noStop, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(task))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStarted))
		})
	})

	t.Run("starts without a bus option", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			task, err := popcorn.NewModule(popcorn.ModRecipe{
				ID:   taskID,
				Done: closeOnStart(),
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return noStop, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithModules(task))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))
		})
	})

	t.Run("health tick wakes a long-running loop", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			m, err := popcorn.NewModule(popcorn.ModRecipe{ID: "m", Start: noopStart})
			is.NoErr(err)

			tick := make(chan time.Time, 1)
			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithHealthTick(tick),
				popcorn.WithModules(m))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)

			tick <- time.Now()
			time.Sleep(time.Millisecond)
			cancel()

			err = <-done
			is.True(errors.Is(err, context.Canceled))
			is.True(errors.Is(err, popcorn.ErrKernelStopped))
		})
	})
}

func TestKernelStartOrder(t *testing.T) {
	t.Parallel()
	t.Run("start order follows dependencies", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			obs := make(chan observation, 1)
			go startObserver(b, obs)

			mk := func(id string, start popcorn.StartFunc, deps ...string) popcorn.Module {
				m, err := popcorn.NewModule(popcorn.ModRecipe{
					ID: id, Dependencies: deps, Done: closeOnStart(),
					Start: start,
				})
				is.NoErr(err)
				return m
			}

			slow := func(context.Context) (popcorn.StopFunc, error) {
				time.Sleep(10 * time.Millisecond)
				return noStop, nil
			}

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(1),
				popcorn.WithModules(mk("a", slow), mk("b", noopStart, "a"), mk("c", noopStart, "b")))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))

			select {
			case o := <-obs:
				got := make([]string, 0, len(o.starts))
				orders := make([]int, 0, len(o.starts))
				took := make([]time.Duration, 0, len(o.starts))
				for _, ms := range o.starts {
					got = append(got, ms.ID)
					orders = append(orders, ms.Order)
					took = append(took, ms.StartTook)
				}
				is.Equal(got, []string{"a", "b", "c"})
				is.Equal(orders, []int{0, 1, 2})
				is.Equal(took, []time.Duration{10 * time.Millisecond, 0, 0})
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("a task dependency is ready when Done closes", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			done := make(chan struct{})
			dependentStarted := make(chan struct{})

			dep, err := popcorn.NewModule(popcorn.ModRecipe{ID: depID, Done: done, Start: noopStart})
			is.NoErr(err)
			dep2, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "dep2", Dependencies: []string{depID},
				Start: func(context.Context) (popcorn.StopFunc, error) {
					close(dependentStarted)
					return noStop, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithParallelism(1),
				popcorn.WithModules(dep, dep2))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = k.Start(ctx) }()

			select {
			case <-dependentStarted:
				is.Fail()
			case <-time.After(10 * time.Millisecond):
			}

			close(done)
			select {
			case <-dependentStarted:
			case <-time.After(never):
				is.Fail()
			}
			cancel()
		})
	})

	t.Run("a dependency is ready when Start returns", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			var depReturned atomic.Bool
			dependentReady := make(chan bool, 1)

			dep, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: depID,
				Start: func(context.Context) (popcorn.StopFunc, error) {
					defer depReturned.Store(true)
					return noStop, nil
				},
			})
			is.NoErr(err)
			dependent, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "dependent", Dependencies: []string{depID},
				Start: func(context.Context) (popcorn.StopFunc, error) {
					dependentReady <- depReturned.Load()
					return noStop, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(2),
				popcorn.WithModules(dep, dependent))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()

			is.Equal(<-dependentReady, true)
			cancel()
			<-done
		})
	})

	t.Run("independent starts enter concurrently", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			var entered sync.WaitGroup
			entered.Add(2)
			start := func(context.Context) (popcorn.StopFunc, error) {
				entered.Done()
				entered.Wait()
				return noStop, nil
			}

			first, err := popcorn.NewModule(popcorn.ModRecipe{ID: "first", Start: start})
			is.NoErr(err)
			second, err := popcorn.NewModule(popcorn.ModRecipe{ID: "second", Start: start})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(2),
				popcorn.WithModules(first, second))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()

			entered.Wait()
			cancel()

			is.True(errors.Is(<-done, context.Canceled))
		})
	})
}

func TestKernelFailures(t *testing.T) {
	t.Parallel()
	t.Run("failed startup stops earlier modules", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			failErr := errPortInUse
			stops := make(chan string, 8)

			earlier, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "earlier",
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return stopped(stops, "earlier"), nil
				},
			})
			is.NoErr(err)

			boom, err := popcorn.NewModule(popcorn.ModRecipe{
				ID:           "boom",
				Dependencies: []string{"earlier"},
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return nil, failErr
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(1),
				popcorn.WithModules(earlier, boom))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			err = k.Start(ctx)

			is.True(errors.Is(err, failErr))
			select {
			case id := <-stops:
				is.Equal(id, "earlier")
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("long-running module runs until canceled", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			started := make(chan bool, 1)
			longMod, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "long",
				Start: func(context.Context) (popcorn.StopFunc, error) {
					started <- true
					return noStop, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(longMod))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			stopped := make(chan error, 1)
			go func() { stopped <- k.Start(ctx) }()

			if !mustBool(is, started) {
				is.Fail()
			}
			cancel()

			select {
			case err := <-stopped:
				is.True(errors.Is(err, context.Canceled))
				is.True(errors.Is(err, popcorn.ErrKernelStopped))
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("NOK stops the kernel and names the module", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			sick, err := popcorn.NewModule(popcorn.ModRecipe{
				ID:    "sick",
				Start: healthStart("sick", b, popcorn.ModuleStateNOK, errDiskFull),
				Done:  closeOnStart(),
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(sick))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			err = k.Start(ctx)
			unhealthy, ok := errors.AsType[popcorn.KernelUnhealthyError](err)
			is.True(ok)
			is.Equal(unhealthy.ModuleID, "sick")
			is.True(errors.Is(unhealthy.Cause, errDiskFull))
		})
	})

	t.Run("OK and TempNOK do not stop the kernel", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			started := make(chan struct{})
			flappy, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "flappy",
				Start: func(ctx context.Context) (popcorn.StopFunc, error) {
					pub := b.Publisher("flappy")
					_ = pub.Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{To: popcorn.ModuleStateOK}))
					_ = pub.Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
						To:    popcorn.ModuleStateTempNOK,
						Cause: errConnectionRefused,
					}))
					close(started)
					return noStop, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(flappy))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()

			<-started

			is.NoErr(b.Publisher("flappy").Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
				To:    popcorn.ModuleStateNOK,
				Cause: errDiskFull,
			})))

			err = <-done
			unhealthy, ok := errors.AsType[popcorn.KernelUnhealthyError](err)
			is.True(ok)
			is.Equal(unhealthy.ModuleID, "flappy")
			is.True(errors.Is(unhealthy.Cause, errDiskFull))
		})
	})

	t.Run("NOK survives a flood of benign transitions", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			started := make(chan struct{})
			flappy, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "flappy",
				Start: func(ctx context.Context) (popcorn.StopFunc, error) {
					pub := b.Publisher("flappy")
					for range 20 {
						_ = pub.Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{To: popcorn.ModuleStateOK}))
						_ = pub.Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{To: popcorn.ModuleStateTempNOK}))
					}
					close(started)
					return noStop, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(flappy))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			<-started

			is.NoErr(b.Publisher("flappy").Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
				To:    popcorn.ModuleStateNOK,
				Cause: errDiskFull,
			})))

			err = <-done
			unhealthy, ok := errors.AsType[popcorn.KernelUnhealthyError](err)
			is.True(ok)
			is.Equal(unhealthy.ModuleID, "flappy")
			is.True(errors.Is(unhealthy.Cause, errDiskFull))
		})
	})

	t.Run("rejects spoofed health", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			longMod, err := popcorn.NewModule(popcorn.ModRecipe{ID: "long", Start: noopStart})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(longMod))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)

			is.NoErr(b.Publisher("not-a-module").Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
				To:    popcorn.ModuleStateNOK,
				Cause: errSpoof,
			})))
			is.NoErr(b.Publisher("long").Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
				To:    popcorn.ModuleStateNOK,
				Cause: errDiskFull,
			})))

			err = <-done
			unhealthy, ok := errors.AsType[popcorn.KernelUnhealthyError](err)
			is.True(ok)
			is.Equal(unhealthy.ModuleID, "long")
			is.True(errors.Is(unhealthy.Cause, errDiskFull))
		})
	})
}

func TestKernelHealthLoss(t *testing.T) {
	t.Parallel()
	t.Run("closing the bus stops a running kernel", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			m, err := popcorn.NewModule(popcorn.ModRecipe{ID: "m", Start: noopStart})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(m))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)

			b.Close()

			select {
			case err := <-done:
				is.True(!errors.Is(err, popcorn.ErrKernelStopped))
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("pre-start health history is ignored", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			is.NoErr(b.Publisher("sick").Send(context.Background(), popcorn.NewEvent(popcorn.ModuleStateChanged{
				To:    popcorn.ModuleStateNOK,
				Cause: errDiskFull,
			})))

			sick, err := popcorn.NewModule(popcorn.ModRecipe{ID: "sick", Start: noopStart})
			is.NoErr(err)
			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(sick))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)

			cancel()
			err = <-done
			_, isUnhealthy := errors.AsType[popcorn.KernelUnhealthyError](err)
			is.True(!isUnhealthy)
			is.True(errors.Is(err, context.Canceled))
		})
	})
}

func TestKernelStop(t *testing.T) {
	t.Parallel()
	t.Run("stop context preserves values from the run context", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			checks := make(chan bool, 1)

			m, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "m",
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return func(stopCtx context.Context) error {
						checks <- stopCtx.Value(key{}) == "value"
						return nil
					}, nil
				},
			})
			is.NoErr(err)

			runCtx := context.WithValue(context.Background(), key{}, "value")
			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(m))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(runCtx)
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)
			cancel()

			is.True(<-checks)
			select {
			case <-done:
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("stop functions run in reverse dependency order when sequential", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			stops := make(chan string, 8)

			m1, _ := popcorn.NewModule(popcorn.ModRecipe{
				ID: "m1", Dependencies: []string{"m2"},
				Start: func(context.Context) (popcorn.StopFunc, error) { return stopped(stops, "m1"), nil },
			})
			m2, _ := popcorn.NewModule(popcorn.ModRecipe{
				ID:    "m2",
				Start: func(context.Context) (popcorn.StopFunc, error) { return stopped(stops, "m2"), nil },
			})

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(1),
				popcorn.WithModules(m1, m2))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))

			select {
			case id := <-stops:
				is.Equal(id, "m1")
				is.Equal(<-stops, "m2")
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("a finished task is stopped on completion", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			stops := make(chan string, 2)
			done := make(chan struct{})

			task, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: taskID, Done: done,
				Start: func(context.Context) (popcorn.StopFunc, error) { return stopped(stops, taskID), nil },
			})
			is.NoErr(err)
			peer, err := popcorn.NewModule(popcorn.ModRecipe{ID: peerID, Start: noopStart})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(task, peer))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			run := make(chan error, 1)
			go func() { run <- k.Start(ctx) }()
			waitRunning(is, b)

			close(done)
			select {
			case id := <-stops:
				is.Equal(id, taskID)
			case <-time.After(never):
				is.Fail()
			}
			select {
			case <-run:
				is.Fail()
			default:
			}

			cancel()
			select {
			case err := <-run:
				is.True(errors.Is(err, context.Canceled))
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("stop order follows a dependency chain under parallelism", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			stops := make(chan string, 8)

			mk := func(id string, deps ...string) popcorn.Module {
				m, err := popcorn.NewModule(popcorn.ModRecipe{
					ID: id, Dependencies: deps,
					Start: func(context.Context) (popcorn.StopFunc, error) { return stopped(stops, id), nil },
				})
				is.NoErr(err)
				return m
			}
			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(2),
				popcorn.WithModules(mk("c", "b"), mk("b", "a"), mk("a")))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)
			cancel()

			select {
			case id := <-stops:
				is.Equal(id, "c")
				is.Equal(<-stops, "b")
				is.Equal(<-stops, "a")
			case <-time.After(never):
				is.Fail()
			}
			select {
			case <-done:
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("same-depth modules stop concurrently", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			var entered sync.WaitGroup
			entered.Add(2)
			stop := concurrentStop(&entered)

			mk := func(id string) popcorn.Module {
				m, err := popcorn.NewModule(popcorn.ModRecipe{
					ID:    id,
					Start: func(context.Context) (popcorn.StopFunc, error) { return stop, nil },
				})
				is.NoErr(err)
				return m
			}
			first := mk("first")
			second := mk("second")

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(2),
				popcorn.WithModules(first, second))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)
			cancel()

			entered.Wait()
			select {
			case <-done:
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("stop budget is a deadline, exhausted stops error out", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			stopDeadlines := make(chan bool, 2)

			slowStop := func(stopCtx context.Context) error {
				_, ok := stopCtx.Deadline()
				stopDeadlines <- ok
				select {
				case <-time.After(2 * never):
					return nil
				case <-stopCtx.Done():
					return stopCtx.Err()
				}
			}

			deferred, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "deferred",
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return slowStop, nil
				},
			})
			is.NoErr(err)

			fast, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: fastID, Done: closeOnStart(),
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return func(stopCtx context.Context) error {
						_, ok := stopCtx.Deadline()
						stopDeadlines <- ok
						return nil
					}, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithStopTimeout(300*time.Millisecond),
				popcorn.WithParallelism(1),
				popcorn.WithModules(deferred, fast))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)
			cancel()

			is.True(<-stopDeadlines)
			is.True(<-stopDeadlines)
			select {
			case <-done:
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("shutdown stays within the stop budget when Start ignores ctx", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			block := make(chan struct{})
			defer close(block)
			entered := make(chan struct{}, 2)
			mk := func(id string) popcorn.Module {
				m, err := popcorn.NewModule(popcorn.ModRecipe{
					ID: id,
					Start: func(context.Context) (popcorn.StopFunc, error) {
						entered <- struct{}{}
						<-block
						return noStop, nil
					},
				})
				is.NoErr(err)
				return m
			}

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(2),
				popcorn.WithStopTimeout(100*time.Millisecond),
				popcorn.WithModules(mk("a"), mk("b")))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			<-entered
			<-entered
			cancel()

			select {
			case <-done:
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("shutdown is bounded when a StopFunc ignores ctx", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			release := make(chan struct{})
			defer close(release)
			entered := make(chan struct{}, 1)

			m, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "stuck-stop",
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return func(context.Context) error {
						entered <- struct{}{}
						<-release
						return nil
					}, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(1),
				popcorn.WithStopTimeout(100*time.Millisecond),
				popcorn.WithModules(m))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)
			cancel()
			<-entered

			select {
			case <-done:
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("a finished task does not stop a long-running peer", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			peer, err := popcorn.NewModule(popcorn.ModRecipe{ID: peerID, Start: noopStart})
			is.NoErr(err)
			task, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: taskID, Done: closeOnStart(), Start: noopStart,
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(peer, task))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)

			select {
			case <-done:
				is.Fail()
			default:
			}
			cancel()
			select {
			case err := <-done:
				is.True(errors.Is(err, context.Canceled))
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("cancellation releases a dependent waiting for a task", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			done := make(chan struct{})
			task, err := popcorn.NewModule(popcorn.ModRecipe{ID: taskID, Done: done, Start: noopStart})
			is.NoErr(err)
			dependent, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "dependent", Dependencies: []string{taskID}, Start: noopStart,
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(task, dependent))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			run := make(chan error, 1)
			go func() { run <- k.Start(ctx) }()
			synctest.Wait()

			cancel()
			select {
			case err := <-run:
				is.True(errors.Is(err, context.Canceled))
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("a task stop failure is logged", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			var logBuf strings.Builder
			done := make(chan struct{})
			task, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: taskID, Done: done,
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return func(context.Context) error { return errBoom }, nil
				},
			})
			is.NoErr(err)
			peer, err := popcorn.NewModule(popcorn.ModRecipe{ID: peerID, Start: noopStart})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithLogger(slog.New(slog.NewTextHandler(&logBuf, nil))),
				popcorn.WithModules(task, peer))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			run := make(chan error, 1)
			go func() { run <- k.Start(ctx) }()
			waitRunning(is, b)

			close(done)
			synctest.Wait()
			is.True(strings.Contains(logBuf.String(), "task stop failed"))

			cancel()
			<-run
		})
	})
}

func TestKernelLateFinish(t *testing.T) {
	t.Parallel()
	t.Run("a module finishing Start during shutdown is still stopped", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			stopped := make(chan struct{}, 1)
			m, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "late",
				Start: func(context.Context) (popcorn.StopFunc, error) {
					entered <- struct{}{}
					<-release
					return func(context.Context) error { stopped <- struct{}{}; return nil }, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(1),
				popcorn.WithModules(m))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			<-entered
			cancel()
			select {
			case <-done:
			case <-time.After(never):
				is.Fail()
			}

			close(release)
			select {
			case <-stopped:
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("stop is not starved by modules stuck in Start", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			block := make(chan struct{})
			defer close(block)
			entered := make(chan struct{}, 2)
			stoppedFast := make(chan struct{}, 1)

			fast, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: fastID,
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return func(context.Context) error { stoppedFast <- struct{}{}; return nil }, nil
				},
			})
			is.NoErr(err)
			stuck := func(id string) popcorn.Module {
				m, err := popcorn.NewModule(popcorn.ModRecipe{
					ID: id, Dependencies: []string{fastID},
					Start: func(context.Context) (popcorn.StopFunc, error) {
						entered <- struct{}{}
						<-block
						return noStop, nil
					},
				})
				is.NoErr(err)
				return m
			}

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(2),
				popcorn.WithStopTimeout(50*time.Millisecond),
				popcorn.WithModules(fast, stuck("s1"), stuck("s2")))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			<-entered
			<-entered
			cancel()

			select {
			case <-done:
			case <-time.After(never):
				is.Fail()
			}
			select {
			case <-stoppedFast:
			case <-time.After(never):
				is.Fail()
			}
		})
	})

	t.Run("a late start cannot move the kernel back to running", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			var mu sync.Mutex
			var timeline []string
			_, err := b.Subscribe("observe", popcorn.WithBacklog(32), popcorn.WithFilter(func(e popcorn.Event) bool {
				mu.Lock()
				switch p := e.Payload.(type) {
				case popcorn.ModuleStarted:
					timeline = append(timeline, "started:"+p.ID)
				case popcorn.KernelStateChanged:
					timeline = append(timeline, "state:"+p.To.String())
				}
				mu.Unlock()
				return true
			}))
			is.NoErr(err)

			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			_, err = b.Subscribe("blocker", popcorn.WithBacklog(1), popcorn.WithFilter(func(e popcorn.Event) bool {
				if _, ok := e.Payload.(popcorn.ModuleStarted); ok {
					select {
					case entered <- struct{}{}:
					default:
					}
					<-release
				}
				return true
			}))
			is.NoErr(err)

			m, err := popcorn.NewModule(popcorn.ModRecipe{ID: "m", Start: noopStart})
			is.NoErr(err)
			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(m))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			<-entered
			cancel()
			close(release)
			<-done

			mu.Lock()
			defer mu.Unlock()
			stoppingAt := -1
			for i, ev := range timeline {
				if ev == "state:stopping" {
					stoppingAt = i
				}
			}
			is.True(stoppingAt >= 0)
			for i, ev := range timeline {
				if strings.HasPrefix(ev, "started:") {
					is.True(i < stoppingAt)
				}
				if i > stoppingAt {
					is.True(ev != "state:running")
				}
			}
		})
	})
}

func TestKernelCoordination(t *testing.T) {
	t.Parallel()

	t.Run("a bus that already owns the kernel subscription is rejected", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			_, err := b.Subscribe("kernel", popcorn.WithBacklog(1))
			is.NoErr(err)

			m, err := popcorn.NewModule(popcorn.ModRecipe{ID: "m", Start: noopStart})
			is.NoErr(err)
			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(m))
			is.NoErr(err)

			is.True(k.Start(context.Background()) != nil)
		})
	})

	t.Run("replayed health is discarded at bootstrap", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			is.NoErr(b.Publisher("ghost").Send(context.Background(),
				popcorn.NewEvent(popcorn.ModuleStateChanged{To: popcorn.ModuleStateNOK, Cause: errBoom})))

			task, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: taskID, Done: closeOnStart(), Start: noopStart,
			})
			is.NoErr(err)
			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(task))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))
		})
	})

	t.Run("a halted dependency wait aborts", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			block := make(chan struct{})
			defer close(block)

			stuck, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "stuck",
				Start: func(context.Context) (popcorn.StopFunc, error) {
					<-block
					return noStop, nil
				},
			})
			is.NoErr(err)
			waiter, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "waiter", Dependencies: []string{"stuck"}, Start: noopStart,
			})
			is.NoErr(err)
			failing, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: failingID, Start: healthStart(failingID, b, popcorn.ModuleStateNOK, errBoom),
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(stuck, waiter, failing))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			is.True(!errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))
		})
	})

	t.Run("a canceled run releases slot waiters", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			block := make(chan struct{})
			defer close(block)

			mk := func(id string) popcorn.Module {
				m, err := popcorn.NewModule(popcorn.ModRecipe{
					ID: id,
					Start: func(context.Context) (popcorn.StopFunc, error) {
						<-block
						return noStop, nil
					},
				})
				is.NoErr(err)
				return m
			}

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(1),
				popcorn.WithModules(mk("a"), mk("b")))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			synctest.Wait()
			cancel()

			is.True(errors.Is(<-done, context.Canceled))
		})
	})
}
