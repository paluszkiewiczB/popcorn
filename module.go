package popcorn

import (
	"context"
	"fmt"
)

// Module is a self-contained unit of functionality managed by the [Kernel].
type Module interface {
	// ID returns the unique identifier of the module.
	// It is recommended to use the module's full import path as a prefix.
	ID() string
	// Dependencies returns the IDs of modules that must be started before this one.
	Dependencies() []string
	// Start initializes the module and returns a [StopFunc] for cleanup.
	Start(ctx context.Context) (StopFunc, error)
}

// TaskModule is a module that performs finite work and then signals completion.
// When all [TaskModule]s in a kernel are done, the kernel initiates graceful shutdown.
type TaskModule interface {
	Module
	// Done returns a channel that is closed when the module's work is finished.
	Done() <-chan struct{}
}

// EventReceiver is a module that wants to receive in-app events.
type EventReceiver interface {
	Module
	// Events returns the channel on which the module receives events.
	Events() chan Event
}

// StartFunc is called to start a [Module].
// If the returned [StopFunc] is non-nil, it will be invoked during shutdown.
type StartFunc func(ctx context.Context) (StopFunc, error)

// StopFunc is invoked to stop a module and clean up its resources.
type StopFunc func(ctx context.Context) error

// ModRecipe is a recipe for creating a [Module].
type ModRecipe struct {
	// ID of the module. Must be unique and non-empty.
	ID string
	// Dependencies are IDs of modules that must start before this one.
	Dependencies []string
	// Start is called to start the module. Must be non-nil.
	Start StartFunc
	// EventsChan is an optional channel that receives in-app events.
	// If set, the module implements [EventReceiver].
	EventsChan chan Event
}

type module struct {
	id           string
	dependencies []string
	start        StartFunc
	events       chan Event
}

func (m *module) ID() string                                  { return m.id }
func (m *module) Dependencies() []string                      { return m.dependencies }
func (m *module) Start(ctx context.Context) (StopFunc, error) { return m.start(ctx) }
func (m *module) Events() chan Event {
	if m.events == nil {
		return nil
	}
	return m.events
}

// NewModule creates a [Module] from the given [ModRecipe].
func NewModule(recipe ModRecipe) (Module, error) {
	if recipe.ID == "" {
		return nil, fmt.Errorf("module id not set")
	}
	if recipe.ID == EventSourceKernel {
		return nil, fmt.Errorf("module id %q is reserved by the kernel", EventSourceKernel)
	}
	if recipe.Start == nil {
		return nil, fmt.Errorf("start function not set for module %q", recipe.ID)
	}
	return &module{
		id:           recipe.ID,
		dependencies: recipe.Dependencies,
		start:        recipe.Start,
		events:       recipe.EventsChan,
	}, nil
}
