package popcorn

import (
	"context"
	"io"
	"slices"
)

// Module is a self-contained unit of functionality managed by the Kernel. The
// Kernel starts a module only after every dependency's Start has returned, and
// calls its StopFunc during shutdown.
type Module interface {
	// ID is the module's unique, non-empty identifier. By convention it is the
	// module's import path. The kernel reserves "kernel" for itself.
	ID() string

	// Dependencies returns the ids of the modules that must be started before
	// this one. A dependency is started once its Start returns, for a TaskModule
	// just like any other module.
	Dependencies() []string

	// Start initializes the module and returns the StopFunc that the kernel
	// invokes during shutdown; the StopFunc may be nil.
	//
	// A module that consumes events holds the Bus like any other dependency and
	// subscribes itself:
	//
	//	func (m *Server) Start(ctx context.Context) (popcorn.StopFunc, error) {
	//		// m holds the *popcorn.Bus it was built with.
	//		events, err := m.bus.Subscribe(m.ID(), popcorn.WithBacklog(64))
	//		if err != nil {
	//			return nil, err
	//		}
	//		go m.consume(events)
	//		return m.stop, nil
	//	}
	//
	// Start should return once its setup is done: every dependent waits for it.
	// Long-running work belongs in a goroutine that the returned StopFunc can
	// stop. A Start that blocks delays its dependents; on shutdown the kernel
	// stops the modules whose Start has returned and does not wait for the rest.
	Start(ctx context.Context) (StopFunc, error)
}

// TaskModule is a Module that performs finite work and signals completion.
//
// The kernel exits once there is at least one TaskModule and all of them are
// done; see [WithExitWhenIdle]. Done does not gate dependents: a dependent
// starts once this module's Start returns, like with any dependency.
type TaskModule interface {
	Module

	// Done is closed when the module's work is finished.
	Done() <-chan struct{}
}

// StartFunc is the function form of a module's Start method.
type StartFunc func(ctx context.Context) (StopFunc, error)

// StopFunc cleans up a module. It receives a fresh shutdown context derived from
// the run context (values preserved, own deadline).
type StopFunc func(ctx context.Context) error

// StopFuncFromCloser adapts an io.Closer to a StopFunc. It returns nil for a nil
// closer, so `return popcorn.StopFuncFromCloser(c), nil` is safe.
func StopFuncFromCloser(c io.Closer) StopFunc {
	if c == nil {
		return nil
	}
	return func(context.Context) error { return c.Close() }
}

// ModRecipe is a declarative recipe for building a Module with [NewModule].
//
// When Done is non-nil the resulting module is also a [TaskModule].
type ModRecipe struct {
	// ID is the module's unique, non-empty identifier. "kernel" is reserved.
	ID string
	// Dependencies lists the ids of the modules that must start before this one.
	Dependencies []string
	// Start initializes the module and returns the StopFunc invoked on shutdown.
	Start StartFunc
	// Done, when non-nil, makes the module a TaskModule; it must be closed when
	// the module's work is finished.
	Done <-chan struct{}
}

// NewModule builds a Module from a recipe. It rejects an empty or reserved ID
// ([ErrModuleIDNotSet], [ErrModuleIDReserved]) and a missing Start
// ([ErrModuleStartNotSet]), and copies Dependencies.
func NewModule(recipe ModRecipe) (Module, error) {
	if recipe.ID == "" {
		return nil, ErrModuleIDNotSet
	}
	if recipe.ID == kernelID {
		return nil, ErrModuleIDReserved
	}
	if recipe.Start == nil {
		return nil, ErrModuleStartNotSet
	}
	base := recipeModule{
		id:    recipe.ID,
		deps:  slices.Clone(recipe.Dependencies),
		start: recipe.Start,
	}
	if recipe.Done != nil {
		return &taskModule{recipeModule: base, done: recipe.Done}, nil
	}
	return &base, nil
}

type recipeModule struct {
	id    string
	deps  []string
	start StartFunc
}

func (m *recipeModule) ID() string             { return m.id }
func (m *recipeModule) Dependencies() []string { return slices.Clone(m.deps) }
func (m *recipeModule) Start(ctx context.Context) (StopFunc, error) {
	return m.start(ctx)
}

type taskModule struct {
	recipeModule

	done <-chan struct{}
}

func (m *taskModule) Done() <-chan struct{} { return m.done }
