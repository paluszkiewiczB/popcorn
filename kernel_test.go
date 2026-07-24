package popcorn_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

var (
	errBoom    = errors.New("boom")
	errCleanup = errors.New("cleanup")
)

func TestKernel_StartsModulesInDependencyOrder(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var order []string

	modA, err := popcorn.NewModule(popcorn.ModRecipe{
		ID:           "a",
		Dependencies: []string{"b"},
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			order = append(order, "a")
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)

	modB, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "b",
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			order = append(order, "b")
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)

	kernel, err := popcorn.NewKernel(popcorn.WithModules(modA, modB))
	is.NoErr(err)

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err = kernel.Start(ctx)
	is.True(errors.Is(err, context.Canceled))
	is.Equal(order, []string{"b", "a"})
}

func TestKernel_CircularDependency(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	modA, err := popcorn.NewModule(popcorn.ModRecipe{
		ID:           "a",
		Dependencies: []string{"b"},
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)

	modB, err := popcorn.NewModule(popcorn.ModRecipe{
		ID:           "b",
		Dependencies: []string{"a"},
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)

	_, err = popcorn.NewKernel(popcorn.WithModules(modA, modB))
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "circular dependency detected"))
}

func TestKernel_MissingDependency(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	modA, err := popcorn.NewModule(popcorn.ModRecipe{
		ID:           "a",
		Dependencies: []string{"missing"},
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)

	_, err = popcorn.NewKernel(popcorn.WithModules(modA))
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "depends on unknown module"))
}

func TestKernel_StartFailure(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx := context.Background()

	modA, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "a",
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return nil, errBoom
		},
	})
	is.NoErr(err)

	kernel, err := popcorn.NewKernel(popcorn.WithModules(modA))
	is.NoErr(err)

	err = kernel.Start(ctx)
	is.True(err != nil)
}

func TestKernel_ModuleNOKStopsKernel(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bus, err := popcorn.NewBus()
	is.NoErr(err)

	modA, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "a",
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			go func() {
				_ = bus.Send(ctx, popcorn.NewEvent[popcorn.ModuleStatusChanged]("a", popcorn.ModuleStatusChanged{
					ID: "a", From: popcorn.ModuleStateOK, To: popcorn.ModuleStateNOK, Cause: "fail",
				}))
			}()

			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)

	kernel, err := popcorn.NewKernel(popcorn.WithBus(bus), popcorn.WithModules(modA))
	is.NoErr(err)

	err = kernel.Start(ctx)

	var unhealthy popcorn.KernelUnhealthyError
	is.True(errors.As(err, &unhealthy))
	is.True(errors.Is(err, unhealthy))
	is.True(strings.Contains(err.Error(), "module NOK"))
}

func TestKernel_TaskModuleCompletion(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})

	task := &taskModule{
		id: "task",
		start: func(_ context.Context) (popcorn.StopFunc, error) {
			go func() {
				close(done)
			}()

			return func(_ context.Context) error { return nil }, nil
		},
		done: done,
	}

	kernel, err := popcorn.NewKernel(popcorn.WithModules(task))
	is.NoErr(err)

	err = kernel.Start(ctx)
	is.True(err == nil || errors.Is(err, context.Canceled))
}

type taskModule struct {
	id    string
	start popcorn.StartFunc
	done  chan struct{}
}

func (m *taskModule) ID() string             { return m.id }
func (m *taskModule) Dependencies() []string { return nil }
func (m *taskModule) Start(ctx context.Context) (popcorn.StopFunc, error) {
	return m.start(ctx)
}
func (m *taskModule) Done() <-chan struct{} { return m.done }

func TestKernel_DuplicateModuleID(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	modA, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "a",
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)

	modB, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "a",
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)

	_, err = popcorn.NewKernel(popcorn.WithModules(modA, modB))
	is.True(err != nil)
}

func TestKernel_NilModule(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	_, err := popcorn.NewKernel(popcorn.WithModules(nil))
	is.True(err != nil)
}

func TestKernel_WithHealthTickAndStopTimeout(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mod, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "a",
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)

	kernel, err := popcorn.NewKernel(
		popcorn.WithModules(mod),
		popcorn.WithHealthTick(100*time.Millisecond),
		popcorn.WithStopTimeout(time.Second),
	)
	is.NoErr(err)

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	err = kernel.Start(ctx)
	is.True(errors.Is(err, context.Canceled))
}

func TestRetainContext(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	retained, cancelRetained := popcorn.RetainContext(ctx)
	defer cancelRetained()

	is.True(retained.Err() == nil)
}

func TestRetainContextCause(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	retained, cancelRetained := popcorn.RetainContextCause(ctx)
	defer cancelRetained(errCleanup)

	is.True(retained.Err() == nil)
}

func TestErrKernelUnhealthy_Unwrap(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	cause := errBoom
	err := popcorn.KernelUnhealthyError{Cause: cause}
	is.True(errors.Is(err, cause))
}
