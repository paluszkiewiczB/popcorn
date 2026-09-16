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

// stopped marks a module as stopped when its StopFunc runs.
func stopped(ch chan<- string, id string) popcorn.StopFunc {
	return func(context.Context) error {
		ch <- id
		return nil
	}
}

// concurrentStop returns a StopFunc that blocks until its paired StopFunc has
// entered; a wave can only complete if its modules stop concurrently.
func concurrentStop(entered *sync.WaitGroup) popcorn.StopFunc {
	return func(context.Context) error {
		entered.Done()
		entered.Wait()
		return nil
	}
}

// healthStart binds a publisher to id and reports a health transition whose
// payload is the framework-owned ModuleStateChanged type; the Kind is derived
// from that type, which is the only health convention the kernel may rely on.
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

// observation is what the pre-start observer sees on the bus.
type observation struct {
	// starts are ModuleStarted payloads in arrival order.
	starts []popcorn.ModuleStarted
	// transitions are KernelStateChanged payloads in arrival order.
	transitions []popcorn.KernelStateChanged
}

// startObserver subscribes to the bus before Kernel.Start so every
// ModuleStarted and KernelStateChanged is captured (backlog seeds the
// subscription even for events emitted while starting). It returns the whole
// lifecycle up to the terminal Stopped transition.
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

// waitRunning blocks until a kernel reached KernelStateRunning on b.
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
		is.Fail() // kernel never reached Running
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

// closeOnStart returns an already-closed channel: the TaskModule is "done".
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
		is.True(err != nil) // parallelism must not be negative
	})

	t.Run("negative health tick rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithHealthTick(-1))
		is.True(err != nil) // a negative tick must be rejected
	})

	t.Run("negative stop timeout rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithStopTimeout(-1))
		is.True(err != nil) // a negative stop budget must be rejected
	})

	t.Run("logger option accepted", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithLogger(slog.New(slog.DiscardHandler)))
		is.NoErr(err) // a logger must be settable on the kernel
	})

	t.Run("tiny health tick accepted", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)
		_, err := popcorn.NewKernel(popcorn.WithHealthTick(time.Millisecond))
		is.NoErr(err) // small positive tick must be constructible
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
				}, statesOf(o.transitions)) // full lifecycle transitions must be published

				ids := make([]string, len(o.starts))
				orders := make([]int, len(o.starts))
				for i, ms := range o.starts {
					ids[i] = ms.ID
					orders[i] = ms.Order
				}
				is.Equal(ids, []string{taskID}) // the module's start must be announced
				is.Equal(orders, []int{0})      // and be the first start
			case <-time.After(never):
				is.Fail() // lifecycle events never observed
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

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped)) // all-task kernel exits gracefully

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStarted)) // second Start must be rejected
		})
	})

	t.Run("starts without a bus option", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			// With no bus configured the kernel creates one itself.
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

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped)) // default bus must work end to end
		})
	})

	t.Run("health tick drives a long-running loop", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			m, err := popcorn.NewModule(popcorn.ModRecipe{ID: "m", Start: noopStart})
			is.NoErr(err)

			// A non-task module keeps the kernel up until canceled, so the health
			// ticker is the only thing waking the loop.
			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithHealthTick(time.Millisecond),
				popcorn.WithModules(m))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()
			waitRunning(is, b)

			time.Sleep(5 * time.Millisecond) // let several health ticks fire
			cancel()

			err = <-done
			is.True(errors.Is(err, context.Canceled))         // a canceled run returns the context error
			is.True(errors.Is(err, popcorn.ErrKernelStopped)) // and is reported as a graceful stop
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
				is.Equal(got, []string{"a", "b", "c"})                       // start order must follow dependencies
				is.Equal(orders, []int{0, 1, 2})                             // Order must be the start position
				is.Equal(took, []time.Duration{10 * time.Millisecond, 0, 0}) // StartTook must measure the Start body
			case <-time.After(never):
				is.Fail() // ModuleStarted events never observed
			}
		})
	})

	t.Run("a task dependency is ready when Start returns", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			// The dependency's Done stays open: readiness must be Start-return, not
			// Done, or the dependent never runs and the kernel deadlocks.
			openDone := make(chan struct{})
			dependentStarted := make(chan struct{})

			dep, err := popcorn.NewModule(popcorn.ModRecipe{ID: depID, Done: openDone, Start: noopStart})
			is.NoErr(err)
			dep2, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "dep2", Done: openDone, Dependencies: []string{depID},
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
			done := make(chan error, 1)
			go func() { done <- k.Start(ctx) }()

			<-dependentStarted // only closes if Start-return gates readiness
			cancel()
			<-done
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

			is.Equal(<-dependentReady, true) // parallel start must still wait for Start to return
			cancel()
			<-done
		})
	})

	t.Run("independent starts enter concurrently", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			// Each start waits until the other has entered, so the pair can only
			// make progress if the kernel runs them concurrently; serialized
			// starts block forever and synctest reports the deadlock. No
			// wall-clock timeout is involved.
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

			entered.Wait() // returns only once both starts ran concurrently
			cancel()

			is.True(errors.Is(<-done, context.Canceled)) // a canceled run returns the context error
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

			is.True(errors.Is(err, failErr)) // the start failure must surface
			select {
			case id := <-stops:
				is.Equal(id, "earlier") // already-started modules must be stopped on failure
			case <-time.After(never):
				is.Fail() // earlier modules were not stopped on failed startup
			}
		})
	})

	t.Run("long-running module without ExitWhenIdle runs until canceled", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			// The run is synchronized on the module body signaling Start - no
			// guess-sleeps: cancel exactly when the kernel is up.
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
				is.Fail() // long-running module never started
			}
			cancel()

			select {
			case err := <-stopped:
				is.True(errors.Is(err, context.Canceled))         // ctx cancel must surface as the context error
				is.True(errors.Is(err, popcorn.ErrKernelStopped)) // a canceled stop is still a graceful stop
			case <-time.After(never):
				is.Fail() // kernel did not stop after ctx cancel
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
			is.True(ok) // NOK report must stop the kernel
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

			// A genuine NOK after the benign reports must still stop the kernel:
			// had OK/TempNOK stopped it, this could not happen.
			is.NoErr(b.Publisher("flappy").Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
				To:    popcorn.ModuleStateNOK,
				Cause: errDiskFull,
			})))

			err = <-done
			unhealthy, ok := errors.AsType[popcorn.KernelUnhealthyError](err)
			is.True(ok) // benign health must not stop the kernel
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
					// Far more benign transitions than the kernel's health
					// backlog: if any of them were consumed they could evict
					// the later NOK, so the NOK must never be lost.
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
			is.True(ok) // a NOK after benign volume must still be actioned
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

			// A NOK from an unregistered id must be ignored...
			is.NoErr(b.Publisher("not-a-module").Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
				To:    popcorn.ModuleStateNOK,
				Cause: errSpoof,
			})))
			// ...so a subsequent genuine NOK still stops the kernel with the real id.
			is.NoErr(b.Publisher("long").Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
				To:    popcorn.ModuleStateNOK,
				Cause: errDiskFull,
			})))

			err = <-done
			unhealthy, ok := errors.AsType[popcorn.KernelUnhealthyError](err)
			is.True(ok)
			is.Equal(unhealthy.ModuleID, "long") // the spoofed source must be ignored
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

			b.Close() // yanking the health subscription must not spin the loop

			select {
			case err := <-done:
				is.True(!errors.Is(err, popcorn.ErrKernelStopped)) // losing health is a failure, not a graceful stop
			case <-time.After(never):
				is.Fail() // Start did not return after the bus closed
			}
		})
	})

	t.Run("pre-start health history is ignored", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			// A stale NOK lands in the bus history before the kernel even exists.
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
			waitRunning(is, b) // reaching Running already proves the replayed NOK was ignored

			cancel()
			err = <-done
			_, isUnhealthy := errors.AsType[popcorn.KernelUnhealthyError](err)
			is.True(!isUnhealthy) // replayed health must not stop a fresh kernel
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
						checks <- stopCtx.Value(key{}) == "value" // values from the run context survive
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
				is.Fail() // kernel did not stop after cancel
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
				ID: "m2", Done: closeOnStart(),
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
				is.Equal(id, "m1")      // the dependent stops before its dependency
				is.Equal(<-stops, "m2") // then the dependency it relies on
			case <-time.After(never):
				is.Fail() // stop functions never ran
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
			// C depends on B depends on A: stop must walk C, B, A.
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
				is.Fail() // stop functions never ran
			}
			select {
			case <-done:
			case <-time.After(never):
				is.Fail() // kernel did not finish shutdown
			}
		})
	})

	t.Run("same-depth modules stop concurrently", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			// Each StopFunc waits until the other has entered, so the pair can
			// only finish if the kernel runs both concurrently; a serialized
			// wave blocks forever and synctest reports the deadlock.
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

			entered.Wait() // returns only once both stops ran concurrently
			select {
			case <-done:
			case <-time.After(never):
				is.Fail() // kernel did not finish shutdown
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
				stopDeadlines <- ok // each StopFunc gets a stop deadline
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
						stopDeadlines <- ok // a deadline must be present here too
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

			is.True(<-stopDeadlines) // slow module's stop carries a stop deadline
			is.True(<-stopDeadlines) // fast module's stop carries a stop deadline
			select {
			case <-done:
			case <-time.After(never):
				is.Fail() // kernel did not finish shutdown
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
						<-block // ignores ctx and keeps its worker slot
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
				is.Fail() // shutdown must not outlive the stop budget
			}
		})
	})

	t.Run("shutdown is bounded when a StopFunc ignores ctx", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			release := make(chan struct{})
			defer close(release) // unblock the abandoned StopFunc so it cannot leak
			entered := make(chan struct{}, 1)

			m, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "stuck-stop",
				Start: func(context.Context) (popcorn.StopFunc, error) {
					return func(context.Context) error {
						entered <- struct{}{}
						<-release // ignores the stop context
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
				is.Fail() // a runaway StopFunc must not hang shutdown past the budget
			}
		})
	})

	t.Run("exit when idle stops while a peer is still starting", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			block := make(chan struct{})
			defer close(block)
			blocked, err := popcorn.NewModule(popcorn.ModRecipe{
				ID:    "blocked",
				Start: func(context.Context) (popcorn.StopFunc, error) { <-block; return noStop, nil },
			})
			is.NoErr(err)
			task, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: taskID, Done: closeOnStart(), Start: noopStart,
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(2),
				popcorn.WithExitWhenIdle(true),
				popcorn.WithModules(blocked, task))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped)) // a done task ends the run
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
					<-release // outlives the shutdown attempt
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
				is.Fail() // kernel did not stop after cancel
			}

			close(release)
			select {
			case <-stopped:
			case <-time.After(never):
				is.Fail() // a module that finishes starting during shutdown must be stopped
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
						<-block // holds its worker slot, ignoring ctx
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
				is.Fail() // shutdown must not outlive the stop budget
			}
			select {
			case <-stoppedFast:
			case <-time.After(never):
				is.Fail() // a started peer must still be stopped
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
					<-release // park the ModuleStarted publish mid-lifecycle
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
			is.True(stoppingAt >= 0) // the kernel must reach stopping
			for i, ev := range timeline {
				if strings.HasPrefix(ev, "started:") {
					is.True(i < stoppingAt) // ModuleStarted must be delivered before stopping
				}
				if i > stoppingAt {
					is.True(ev != "state:running") // stopping must never regress to running
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
			is.NoErr(err) // the reserved id is ours for this test

			m, err := popcorn.NewModule(popcorn.ModRecipe{ID: "m", Start: noopStart})
			is.NoErr(err)
			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(m))
			is.NoErr(err)

			is.True(k.Start(context.Background()) != nil) // a taken health stream must fail the start
		})
	})

	t.Run("replayed health is discarded at bootstrap", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			// A NOK from before the kernel exists is replayed into the health
			// stream and must be dropped, not treated as a live failure.
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

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped)) // a stale NOK must not fail the run
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
					<-block // never returns, so its dependent is never released
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

			is.True(!errors.Is(k.Start(ctx), popcorn.ErrKernelStopped)) // the NOK must fail the run
		})
	})

	t.Run("a canceled run releases slot waiters", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			block := make(chan struct{})
			defer close(block)

			// With a single start slot, one module holds it and blocks while the
			// other parks waiting for it; canceling the run must release the
			// waiter through the halt signal rather than by freeing the slot.
			mk := func(id string) popcorn.Module {
				m, err := popcorn.NewModule(popcorn.ModRecipe{
					ID: id,
					Start: func(context.Context) (popcorn.StopFunc, error) {
						<-block // ignores cancellation, holds the only slot
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
			synctest.Wait() // one module holds the slot, the other is parked on it
			cancel()

			is.True(errors.Is(<-done, context.Canceled)) // a canceled run returns the context error
		})
	})
}
