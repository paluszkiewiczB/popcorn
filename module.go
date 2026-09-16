package popcorn

import (
	"context"
	"io"
	"slices"
)

// Module is a self-contained unit of functionality managed by the Kernel.
type Module interface {
	// ID is the unique, non-empty identifier of the module. By convention it is the
	// module's import path. The kernel reserves "kernel" for itself.
	ID() string

	// Dependencies returns the ids of modules that must be ready before this one
	// starts. A dependency is ready when its Start returns, for a TaskModule just
	// like any other module.
	Dependencies() []string

	// Start initializes the module. A module that wants events subscribes itself
	// through the injected Bus, for example:
	//
	//	events, err := bus.Subscribe(m.ID(), popcorn.WithBacklog(64), popcorn.WithFilter(f))
	//
	// Start returns a StopFunc that the kernel invokes during shutdown; the StopFunc
	// may be nil.
	Start(ctx context.Context) (StopFunc, error)
}

// TaskModule is a Module that performs finite work and signals completion.
//
// Readiness: TaskModule.Done does not gate dependents; a dependent starts once this
// module's Start returns, like with any dependency.
//
// Exit: the kernel auto-stops once there is at least one TaskModule and all of them
// are done. When exitWhenIdle is false it also waits for every module to have
// started.
type TaskModule interface {
	Module

	// Done is closed when the module's work is finished.
	Done() <-chan struct{}
}

// StartFunc is a function-based Module body.
type StartFunc func(ctx context.Context) (StopFunc, error)

// StopFunc cleans up a module. It receives a fresh shutdown context derived from the
// run context (values preserved, own deadline).
type StopFunc func(ctx context.Context) error

// StopFuncFromCloser adapts an io.Closer to a StopFunc. It returns nil for a nil
// closer, so it is safe to write `return popcorn.StopFuncFromCloser(res), nil`.
func StopFuncFromCloser(c io.Closer) StopFunc {
	if c == nil {
		return nil
	}
	return func(context.Context) error { return c.Close() }
}

// ModRecipe is a declarative recipe for building a Module. When Done is non-nil the
// resulting module also satisfies TaskModule.
//
// NewModule returns the Module interface rather than a concrete type because the
// concrete implementation depends on whether Done is set. Event-consuming modules
// subscribe through the injected Bus inside Start and need no special interface.
type ModRecipe struct {
	ID           string
	Dependencies []string
	Start        StartFunc
	Done         <-chan struct{} // optional; makes the module a TaskModule
}

// NewModule builds a Module from a recipe, applying the same validation as the
// kernel (non-empty, non-reserved id, non-nil Start, defensive copy of dependencies).
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
