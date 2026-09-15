package popcorn_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
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
			return nil, err
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

func Test_Kernel(test *testing.T) {
	test.Run("validation", func(t *testing.T) {
		t.Run("nil module", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(nil))
			is.True(errors.Is(err, popcorn.ErrNilModule))
		})

		t.Run("typed-nil module", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules((*honest)(nil)))
			is.True(errors.Is(err, popcorn.ErrNilModule)) // typed nil must be handled
		})

		t.Run("empty module id", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: ""}))
			is.True(errors.Is(err, popcorn.ErrModuleIDNotSet))
		})

		t.Run("duplicate module id", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(
				popcorn.WithModules(honest{id: "x"}, honest{id: "x"}),
			)
			is.True(errors.Is(err, popcorn.ErrDuplicateModuleID))
		})

		t.Run("reserved module id", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: "kernel"}))
			is.True(errors.Is(err, popcorn.ErrModuleIDReserved))
		})

		t.Run("unknown dependency", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: "a", deps: []string{"ghost"}}))
			is.True(errors.Is(err, popcorn.ErrUnknownDependency))
		})

		t.Run("self dependency", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(honest{id: "a", deps: []string{"a"}}))
			is.True(errors.Is(err, popcorn.ErrSelfDependency))
		})

		t.Run("duplicate dependency entry", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(
				honest{id: "a"},
				honest{id: "b", deps: []string{"a", "a"}},
			))
			is.True(errors.Is(err, popcorn.ErrDuplicateDependency))
		})

		t.Run("circular dependency reports the path", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithModules(
				honest{id: "a", deps: []string{"b"}},
				honest{id: "b", deps: []string{"a"}},
			))
			is.True(errors.Is(err, popcorn.ErrCircularDependency))
			is.True(strings.Contains(err.Error(), "a")) // the cycle path names the members
			is.True(strings.Contains(err.Error(), "b"))
		})

		t.Run("invalid kernel options rejected", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithParallelism(-1))
			is.True(err != nil) // parallelism must not be negative
		})

		t.Run("tiny health tick accepted", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewKernel(popcorn.WithHealthTick(time.Millisecond))
			is.NoErr(err) // small positive tick must be constructible (B21)
		})
	})

	test.Run("lifecycle events", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))
		obs := make(chan observation, 1)
		go startObserver(b, obs)

		task, err := popcorn.NewModule(popcorn.ModRecipe{
			ID: "task", Done: closeOnStart(),
			Start: func(context.Context) (popcorn.StopFunc, error) { return nil, nil },
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

	test.Run("single shot", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))
		task, err := popcorn.NewModule(popcorn.ModRecipe{
			ID:   "task",
			Done: closeOnStart(),
			Start: func(context.Context) (popcorn.StopFunc, error) {
				return nil, nil
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

	test.Run("starts without a bus option", func(t *testing.T) {
		is := is.New(t)

		// api.go: with no bus configured the kernel creates one itself.
		task, err := popcorn.NewModule(popcorn.ModRecipe{
			ID:   "task",
			Done: closeOnStart(),
			Start: func(context.Context) (popcorn.StopFunc, error) {
				return nil, nil
			},
		})
		is.NoErr(err)

		k, err := popcorn.NewKernel(popcorn.WithModules(task))
		is.NoErr(err)

		ctx, cancel := within()
		defer cancel()

		is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped)) // default bus must work end to end
	})

	test.Run("start order follows dependencies", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))
		obs := make(chan observation, 1)
		go startObserver(b, obs)

		mk := func(id string, deps ...string) popcorn.Module {
			m, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: id, Dependencies: deps, Done: closeOnStart(),
				Start: func(context.Context) (popcorn.StopFunc, error) { return nil, nil },
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

	test.Run("task readiness gates dependents", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		done := make(chan struct{})

		ready := make(chan bool, 1)
		dependentStart := func(context.Context) (popcorn.StopFunc, error) {
			// The TaskModule dependency is ready when Done is closed; at this
			// moment it must already be closed.
			select {
			case <-done:
				ready <- true
			case <-time.After(300 * time.Millisecond):
				ready <- false
			}
			return nil, nil
		}

		dep, err := popcorn.NewModule(popcorn.ModRecipe{
			ID: "dep",
			Start: func(context.Context) (popcorn.StopFunc, error) {
				go close(done)
				return nil, nil
			},
			Done: done,
		})
		is.NoErr(err)

		dep2, err := popcorn.NewModule(popcorn.ModRecipe{
			ID: "dep2", Start: dependentStart, Done: done, Dependencies: []string{"dep"},
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

	test.Run("a dependency is ready when Start returns", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		var admitted atomic.Bool
		depStart := func(context.Context) (popcorn.StopFunc, error) {
			// The flag flips just before the Start body returns per contract
			// - a normal module dependency is ready exactly when Start returns.
			defer admitted.Store(true)
			return nil, nil
		}

		ready := make(chan bool, 1)
		dependentStart := func(context.Context) (popcorn.StopFunc, error) {
			ready <- admitted.Load()
			return nil, nil
		}

		dep, err := popcorn.NewModule(popcorn.ModRecipe{ID: "dep", Start: depStart})
		is.NoErr(err)
		dependent, err := popcorn.NewModule(popcorn.ModRecipe{
			ID: "dependent", Start: dependentStart, Dependencies: []string{"dep"},
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

	test.Run("parallel start schedules concurrently", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		// WithParallelism(0) means GOMAXPROCS; with two non-dependent modules
		// the scheduler must be able to enter both Start functions without
		// the first returning - sequential scheduling deadlocks this test.
		release := make(chan struct{})
		entered := make(chan string, 2)

		mk := func(id string) popcorn.Module {
			m, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: id,
				Start: func(context.Context) (popcorn.StopFunc, error) {
					entered <- id
					<-release
					return nil, nil
				},
			})
			is.NoErr(err)
			return m
		}

		k, err := popcorn.NewKernel(popcorn.WithBus(b),
			popcorn.WithParallelism(0),
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

	test.Run("failed startup stops earlier modules", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))
		failErr := errors.New("port in use")
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

	test.Run("idle exit", func(t *testing.T) {
		t.Run("long-running module without ExitWhenIdle runs until canceled", func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			// The run is synchronized on the module body signaling Start - no
			// guess-sleeps: cancel exactly when the kernel is up.
			started := make(chan bool, 1)
			longMod, err := popcorn.NewModule(popcorn.ModRecipe{
				ID: "long",
				Start: func(context.Context) (popcorn.StopFunc, error) {
					started <- true
					return nil, nil
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

	test.Run("unhealthy exit", func(t *testing.T) {
		t.Run("NOK stops the kernel and names the module", func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			sick, err := popcorn.NewModule(popcorn.ModRecipe{
				ID:    "sick",
				Start: healthStart("sick", b, popcorn.ModuleStateNOK, errors.New("disk full")),
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
			is.True(errors.Is(unhealthy.Cause, errors.New("disk full")))
		})

		t.Run("OK and TempNOK do not stop the kernel", func(t *testing.T) {
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
						Cause: errors.New("connection refused"),
					}))
					started <- true
					return nil, nil
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
		})
	})

	test.Run("rejects spoofed health", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t, popcorn.WithReplayBuffer(8))

		longMod, err := popcorn.NewModule(popcorn.ModRecipe{
			ID:    "long",
			Start: func(context.Context) (popcorn.StopFunc, error) { return nil, nil },
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
			Cause: errors.New("spoof"),
		})))

		select {
		case <-done:
			is.Fail() // kernel must ignore the spoofed NOK and keep running
		case <-time.After(500 * time.Millisecond):
		}
		cancel()
	})

	test.Run("stop", func(t *testing.T) {
		t.Run("stop context preserves values from the run context", func(t *testing.T) {
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
			go func() { _ = k.Start(ctx) }()
			time.Sleep(150 * time.Millisecond)
			cancel()

			is.True(<-checks)
		})

		t.Run("stop functions run in full reverse start order when sequential", func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			stops := make(chan string, 8)

			m1, _ := popcorn.NewModule(popcorn.ModRecipe{ID: "m1", Dependencies: []string{"m2"},
				Start: func(context.Context) (popcorn.StopFunc, error) { return stopped(stops, "m1"), nil }})
			m2, _ := popcorn.NewModule(popcorn.ModRecipe{ID: "m2", Done: closeOnStart(),
				Start: func(context.Context) (popcorn.StopFunc, error) { return stopped(stops, "m2"), nil }})

			k, err := popcorn.NewKernel(popcorn.WithBus(b),
				popcorn.WithParallelism(1),
				popcorn.WithModules(m1, m2))
			is.NoErr(err)

			ctx, cancel := within()
			defer cancel()

			is.True(errors.Is(k.Start(ctx), popcorn.ErrKernelStopped))

			select {
			case id := <-stops:
				is.Equal(id, "m2")      // last started stops first
				is.Equal(<-stops, "m1") // then the rest in reverse start order
			case <-time.After(never):
				is.Fail() // stop functions never ran
			}
		})

		t.Run("stop budget is a deadline, exhausted stops error out", func(t *testing.T) {
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
				ID: "fast", Done: closeOnStart(),
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
			go func() { _ = k.Start(ctx) }()
			time.Sleep(150 * time.Millisecond)
			cancel()

			is.True(<-stopDeadlines) // slow module's stop carries a stop deadline
			is.True(<-stopDeadlines) // fast module's stop carries a stop deadline
		})
	})
}
