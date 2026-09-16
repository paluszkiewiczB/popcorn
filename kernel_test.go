package popcorn_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/matryer/is"
	popcorn "github.com/paluszkiewiczB/popcorn"
)

// stopped marks a module as stopped when its StopFunc runs.
func stopped(ch chan<- string, id string) popcorn.StopFunc {
	return func(context.Context) error {
		ch <- id
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
// subscription even for events emitted while starting).
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
	quiesce := 300 * time.Millisecond
	for {
		select {
		case e := <-ch:
			switch p := e.Payload.(type) {
			case popcorn.ModuleStarted:
				obs.starts = append(obs.starts, p)
			case popcorn.KernelStateChanged:
				obs.transitions = append(obs.transitions, p)
			}
			quiesce = 300 * time.Millisecond
		case <-time.After(quiesce):
			out <- obs
			return
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

func Test_Kernel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, testKernel)
}

func testKernel(test *testing.T) {
	testKernelValidation(test)
	testKernelLifecycle(test)
	testKernelStartOrder(test)
	testKernelFailures(test)
	testKernelHealthLoss(test)
	testKernelStop(test)
	testKernelLateFinish(test)
}

func testKernelValidation(t *testing.T) {
	t.Helper()
	step(t, "validation", func(t *testing.T) {
		t.Helper()
		step(t, "nil module", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(nil))
			is.True(errors.Is(err, popcorn.ErrNilModule))
		})

		step(t, "typed-nil module", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules((*honest)(nil)))
			is.True(errors.Is(err, popcorn.ErrNilModule)) // typed nil must be handled
		})

		step(t, "empty module id", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: ""}))
			is.True(errors.Is(err, popcorn.ErrModuleIDNotSet))
		})

		step(t, "duplicate module id", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(
				popcorn.WithModules(honest{id: "x"}, honest{id: "x"}),
			)
			is.True(errors.Is(err, popcorn.ErrDuplicateModuleID))
		})

		step(t, "reserved module id", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: "kernel"}))
			is.True(errors.Is(err, popcorn.ErrModuleIDReserved))
		})

		step(t, "unknown dependency", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: "a", deps: []string{"ghost"}}))
			is.True(errors.Is(err, popcorn.ErrUnknownDependency))
		})

		step(t, "self dependency", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: "a", deps: []string{"a"}}))
			is.True(errors.Is(err, popcorn.ErrSelfDependency))
		})

		step(t, "duplicate dependency entry", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(
				honest{id: "a"},
				honest{id: "b", deps: []string{"a", "a"}},
			))
			is.True(errors.Is(err, popcorn.ErrDuplicateDependency))
		})

		step(t, "circular dependency reports the path", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(
				honest{id: "a", deps: []string{"b"}},
				honest{id: "b", deps: []string{"a"}},
			))
			is.True(errors.Is(err, popcorn.ErrCircularDependency))
			is.True(strings.Contains(err.Error(), "a")) // the cycle path names the members
			is.True(strings.Contains(err.Error(), "b"))
		})

		step(t, "invalid kernel options rejected", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithParallelism(-1))
			is.True(err != nil) // parallelism must not be negative
		})

		step(t, "tiny health tick accepted", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithHealthTick(time.Millisecond))
			is.NoErr(err) // small positive tick must be constructible (B21)
		})
	})
}

func testKernelLifecycle(t *testing.T) {
	t.Helper()
	step(t, "lifecycle events", func(t *testing.T) {
		t.Helper()
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
			is.Equal(len(o.starts), 1) // the module's start must be announced
		case <-time.After(never):
			is.Fail() // lifecycle events never observed
		}
	})

	step(t, "single shot", func(t *testing.T) {
		t.Helper()
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

	step(t, "starts without a bus option", func(t *testing.T) {
		t.Helper()
		is := is.New(t)

		// api.go: with no bus configured the kernel creates one itself.
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
}

func testKernelStartOrder(t *testing.T) {
	t.Helper()
	step(t, "start order follows dependencies", func(t *testing.T) {
		t.Helper()
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))
		obs := make(chan observation, 1)
		go startObserver(b, obs)

		mk := func(id string, deps ...string) popcorn.Module {
			m, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: id, Dependencies: deps, Done: closeOnStart(),
				Start: noopStart,
			})
			is.NoErr(err)
			return m
		}

		k, err := popcorn.NewKernel(popcorn.WithBus(b),
			popcorn.WithParallelism(1),
			popcorn.WithModules(mk("a"), mk("b", "a"), mk("c", "b")))
		is.NoErr(err)

		ctx, cancel := within()
		defer cancel()

		is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))

		select {
		case o := <-obs:
			got := make([]string, 0, len(o.starts))
			for i, ms := range o.starts {
				got = append(got, ms.ID)
				is.Equal(ms.Order, i)      // Order must be the start position
				is.True(ms.StartTook >= 0) // StartTook must be stamped
			}
			is.Equal(got, []string{"a", "b", "c"}) // start order must follow dependencies
		case <-time.After(never):
			is.Fail() // ModuleStarted events never observed
		}
	})

	step(t, "a task dependency is ready when Start returns", func(t *testing.T) {
		t.Helper()
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		done := make(chan struct{})

		ready := make(chan bool, 1)
		dependentStart := func(context.Context) (popcorn.StopFunc, error) {
			// Readiness is Start-return; Done is closed just after, so the
			// dependent may legitimately observe it open and then closed.
			select {
			case <-done:
				ready <- true
			case <-time.After(300 * time.Millisecond):
				ready <- false
			}
			return noStop, nil
		}

		dep, err := popcorn.NewModule(popcorn.ModRecipe{
			ID: depID,
			Start: func(context.Context) (popcorn.StopFunc, error) {
				go close(done)
				return noStop, nil
			},
			Done: done,
		})
		is.NoErr(err)

		dep2, err := popcorn.NewModule(popcorn.ModRecipe{
			ID: "dep2", Start: dependentStart, Done: done, Dependencies: []string{depID},
		})
		is.NoErr(err)

		k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithParallelism(1),
			popcorn.WithModules(dep, dep2))
		is.NoErr(err)

		ctx, cancel := within()
		defer cancel()

		is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped)) // readiness specs end gracefully
		is.True(<-ready)                                           // dependent must start only after Done closed
	})

	step(t, "a dependency is ready when Start returns", func(t *testing.T) {
		t.Helper()
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		var admitted atomic.Bool
		depStart := func(context.Context) (popcorn.StopFunc, error) {
			// The flag flips just before the Start body returns per contract
			// - a normal module dependency is ready exactly when Start returns.
			defer admitted.Store(true)
			return noStop, nil
		}

		ready := make(chan bool, 1)
		dependentStart := func(context.Context) (popcorn.StopFunc, error) {
			ready <- admitted.Load()
			return noStop, nil
		}

		dep, err := popcorn.NewModule(popcorn.ModRecipe{ID: depID, Start: depStart})
		is.NoErr(err)
		dependent, err := popcorn.NewModule(popcorn.ModRecipe{
			ID: "dependent", Start: dependentStart, Dependencies: []string{depID},
		})
		is.NoErr(err)

		k, err := popcorn.NewKernel(popcorn.WithBus(b),
			popcorn.WithParallelism(2),
			popcorn.WithModules(dep, dependent))
		is.NoErr(err)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(300 * time.Millisecond)
			cancel()
		}()

		_ = k.Start(ctx)
		is.True(<-ready) // with parallel start the dependent must still wait for Start to return
	})

	step(t, "parallel start schedules concurrently", func(t *testing.T) {
		t.Helper()
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		// Two non-dependent modules: the scheduler must enter both Start
		// functions before the first returns. Parallelism 2 keeps this
		// independent of GOMAXPROCS.
		release := make(chan struct{})
		entered := make(chan string, 2)

		mk := func(id string) popcorn.Module {
			m, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: id,
				Start: func(context.Context) (popcorn.StopFunc, error) {
					entered <- id
					<-release
					return noStop, nil
				},
			})
			is.NoErr(err)
			return m
		}

		k, err := popcorn.NewKernel(popcorn.WithBus(b),
			popcorn.WithParallelism(2),
			popcorn.WithModules(mk("p1"), mk("p2")))
		is.NoErr(err)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stopped := make(chan error, 1)
		go func() { stopped <- k.Start(ctx) }()

		for range 2 {
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				is.Fail() // modules must be able to start concurrently
			}
		}

		close(release)
		cancel()

		select {
		case err := <-stopped:
			is.True(err == nil || errors.Is(err, popcorn.ErrKernelStopped) || ctx.Err() != nil)
		case <-time.After(never):
			is.Fail() // kernel did not return after release + cancel
		}
	})
}

func testKernelFailures(t *testing.T) {
	t.Helper()
	step(t, "failed startup stops earlier modules", func(t *testing.T) {
		t.Helper()
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

	step(t, "idle exit", func(t *testing.T) {
		t.Helper()
		step(t, "long-running module without ExitWhenIdle runs until canceled", func(t *testing.T) {
			t.Helper()
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
				is.True(err != nil)                                // ctx cancel must return an error
				is.True(!errors.Is(err, popcorn.ErrKernelStopped)) // cancel is not a graceful stop
			case <-time.After(never):
				is.Fail() // kernel did not stop after ctx cancel
			}
		})
	})

	step(t, "unhealthy exit", func(t *testing.T) {
		t.Helper()
		step(t, "NOK stops the kernel and names the module", func(t *testing.T) {
			t.Helper()
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
			var unhealthy popcorn.KernelUnhealthyError
			is.True(errors.As(err, &unhealthy)) // NOK report must stop the kernel
			is.Equal(unhealthy.ModuleID, "sick")
			is.True(errors.Is(unhealthy.Cause, errDiskFullCheck))
		})

		step(t, "OK and TempNOK do not stop the kernel", func(t *testing.T) {
			t.Helper()
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			started := make(chan bool, 1)

			flappy, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "flappy",
				Start: func(ctx context.Context) (popcorn.StopFunc, error) {
					pub := b.Publisher("flappy")
					_ = pub.Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{To: popcorn.ModuleStateOK}))
					_ = pub.Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
						To:    popcorn.ModuleStateTempNOK,
						Cause: errConnectionRefused,
					}))
					started <- true
					return noStop, nil
				},
			})
			is.NoErr(err)

			k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(flappy))
			is.NoErr(err)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			res := make(chan error, 1)
			go func() { res <- k.Start(ctx) }()

			if !mustBool(is, started) {
				is.Fail() // module never started (kernel deaf?)
			}

			// Neither OK nor TempNOK may stop the kernel.
			select {
			case <-res:
				is.Fail() // a benign health report must not stop the kernel
			case <-time.After(500 * time.Millisecond):
			}
			cancel()
			select {
			case <-res:
			case <-time.After(never):
				is.Fail() // kernel did not stop after cancel
			}
		})
	})

	step(t, "rejects spoofed health", func(t *testing.T) {
		t.Helper()
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		longMod, err := popcorn.NewModule(popcorn.ModRecipe{
			ID:    "long",
			Start: noopStart,
		})
		is.NoErr(err)

		k, err := popcorn.NewKernel(popcorn.WithBus(b), popcorn.WithModules(longMod))
		is.NoErr(err)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- k.Start(ctx) }()

		// Synchronize on the kernel being up (no sleep-luck), then try to stop
		// it from an id that was never a registered module.
		waitRunning(is, b)

		fake := b.Publisher("not-a-module")
		is.NoErr(fake.Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
			To:    popcorn.ModuleStateNOK,
			Cause: errSpoof,
		})))

		select {
		case <-done:
			is.Fail() // kernel must ignore the spoofed NOK and keep running
		case <-time.After(500 * time.Millisecond):
		}
		cancel()
		select {
		case <-done:
		case <-time.After(never):
			is.Fail() // kernel did not stop after cancel
		}
	})
}

func testKernelHealthLoss(t *testing.T) {
	t.Helper()
	step(t, "closing the bus stops a running kernel", func(t *testing.T) {
		t.Helper()
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
			is.True(err != nil) // losing health must end Start
		case <-time.After(never):
			is.Fail() // Start did not return after the bus closed
		}
	})

	step(t, "pre-start health history is ignored", func(t *testing.T) {
		t.Helper()
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
		waitRunning(is, b)

		select {
		case <-done:
			is.Fail() // replayed health must not stop a fresh kernel
		case <-time.After(100 * time.Millisecond):
		}
		cancel()
		<-done
	})
}

func testKernelStop(t *testing.T) {
	t.Helper()
	step(t, "stop", func(t *testing.T) {
		t.Helper()
		step(t, "stop context preserves values from the run context", func(t *testing.T) {
			t.Helper()
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

		step(t, "stop functions run in reverse declaration order when sequential", func(t *testing.T) {
			t.Helper()
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
				is.Equal(id, "m2")      // the module declared last stops first
				is.Equal(<-stops, "m1") // then the rest in reverse declaration order
			case <-time.After(never):
				is.Fail() // stop functions never ran
			}
		})

		step(t, "stop budget is a deadline, exhausted stops error out", func(t *testing.T) {
			t.Helper()
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

		step(t, "shutdown stays within the stop budget when Start ignores ctx", func(t *testing.T) {
			t.Helper()
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

		step(t, "exit when idle stops while a peer is still starting", func(t *testing.T) {
			t.Helper()
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

func testKernelLateFinish(t *testing.T) {
	t.Helper()
	step(t, "a module finishing Start during shutdown is still stopped", func(t *testing.T) {
		t.Helper()
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

	step(t, "stop is not starved by modules stuck in Start", func(t *testing.T) {
		t.Helper()
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

	step(t, "a late start cannot move the kernel back to running", func(t *testing.T) {
		t.Helper()
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
}
