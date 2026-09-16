package popcorn_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/paluszkiewiczB/popcorn"
)

// Example wires a finite importer module and a server module that depends on
// it, runs the kernel until all tasks finish, and shows that the dependency
// started first.
func Example() {
	// Records the order in which modules start.
	started := make(chan string, 2)

	// A finite module: it does its work and closes Done when finished.
	done := make(chan struct{})
	importer, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "importer",
		Start: func(context.Context) (popcorn.StopFunc, error) {
			// ... import data ...
			started <- "importer"
			close(done)
			return func(context.Context) error { return nil }, nil
		},
		Done: done, // Done makes this a TaskModule.
	})
	if err != nil {
		fmt.Println("module:", err)
		return
	}

	bus, err := popcorn.NewBus()
	if err != nil {
		fmt.Println("bus:", err)
		return
	}
	defer bus.Close()

	// The kernel starts server only after importer's Start has returned.
	server, err := popcorn.NewModule(popcorn.ModRecipe{
		ID:           "server",
		Dependencies: []string{"importer"},
		Start: func(context.Context) (popcorn.StopFunc, error) {
			started <- "server"
			return func(context.Context) error { return nil }, nil
		},
	})
	if err != nil {
		fmt.Println("module:", err)
		return
	}

	k, err := popcorn.NewKernel(popcorn.WithBus(bus), popcorn.WithModules(importer, server))
	if err != nil {
		fmt.Println("kernel:", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// A graceful stop is reported as ErrKernelStopped.
	err = k.Start(ctx)

	close(started)
	for id := range started {
		fmt.Println(id)
	}
	fmt.Println(errors.Is(err, popcorn.ErrKernelStopped))

	// Output:
	// importer
	// server
	// true
}

// ExampleKernel_Start_canceled shows the primary shutdown path: the caller's
// context is canceled, as it is by signal.NotifyContext. The stop is graceful,
// so Start reports ErrKernelStopped while the context error stays inspectable.
func ExampleKernel_Start_canceled() {
	bus, err := popcorn.NewBus()
	if err != nil {
		fmt.Println("bus:", err)
		return
	}
	defer bus.Close()

	started := make(chan struct{})
	server, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "server",
		Start: func(context.Context) (popcorn.StopFunc, error) {
			close(started)
			return func(context.Context) error { return nil }, nil
		},
	})
	if err != nil {
		fmt.Println("module:", err)
		return
	}

	k, err := popcorn.NewKernel(popcorn.WithBus(bus), popcorn.WithModules(server))
	if err != nil {
		fmt.Println("kernel:", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- k.Start(ctx) }()

	<-started // the kernel is up, so cancel is a clean shutdown
	cancel()
	err = <-done

	fmt.Println(errors.Is(err, popcorn.ErrKernelStopped))
	fmt.Println(errors.Is(err, context.Canceled))

	// Output:
	// true
	// true
}

// ExampleBus_Subscribe subscribes with a backlog and a filter, so the
// subscription buffers matching events and ignores the rest.
func ExampleBus_Subscribe() {
	bus, err := popcorn.NewBus()
	if err != nil {
		fmt.Println("bus:", err)
		return
	}
	defer bus.Close()

	events, err := bus.Subscribe("consumer",
		popcorn.WithBacklog(4),
		popcorn.WithFilter(func(e popcorn.Event) bool {
			return e.Kind == "temperature"
		}),
	)
	if err != nil {
		fmt.Println("subscribe:", err)
		return
	}

	ctx := context.Background()
	pub := bus.Publisher("producer")
	_ = pub.Send(ctx, popcorn.NewEventOf("temperature", 21))
	_ = pub.Send(ctx, popcorn.NewEventOf("noise", 2))
	_ = pub.Send(ctx, popcorn.NewEventOf("temperature", 22))

	for range 2 {
		e := <-events
		fmt.Println(e.Kind, e.Payload)
	}

	// Output:
	// temperature 21
	// temperature 22
}

// ExampleBus_Publisher shows a module reporting its health. The publisher is
// bound to the module's id, so Event.Source identifies the reporter.
func ExampleBus_Publisher() {
	bus, err := popcorn.NewBus()
	if err != nil {
		fmt.Println("bus:", err)
		return
	}
	defer bus.Close()

	// A probe subscribes to health reports.
	health, err := bus.Subscribe("probe",
		popcorn.WithBacklog(4),
		popcorn.WithFilter(func(e popcorn.Event) bool {
			_, ok := e.Payload.(popcorn.ModuleStateChanged)
			return ok
		}),
	)
	if err != nil {
		fmt.Println("subscribe:", err)
		return
	}

	// The database module reports that it is healthy.
	_ = bus.Publisher("database").Send(context.Background(), popcorn.NewEvent(
		popcorn.ModuleStateChanged{To: popcorn.ModuleStateOK},
	))

	e := <-health
	change, ok := e.Payload.(popcorn.ModuleStateChanged)
	if !ok {
		fmt.Println("unexpected payload")
		return
	}
	fmt.Printf("%s is %s\n", e.Source(), change.To)

	// Output: database is ok
}

// ExampleKernel_Start watches the kernel lifecycle as events: every state
// change is published as a KernelStateChanged.
func ExampleKernel_Start() {
	bus, err := popcorn.NewBus()
	if err != nil {
		fmt.Println("bus:", err)
		return
	}
	defer bus.Close()

	states, err := bus.Subscribe("probe",
		popcorn.WithBacklog(8),
		popcorn.WithFilter(func(e popcorn.Event) bool {
			_, ok := e.Payload.(popcorn.KernelStateChanged)
			return ok
		}),
	)
	if err != nil {
		fmt.Println("subscribe:", err)
		return
	}

	done := make(chan struct{})
	job, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "job",
		Start: func(context.Context) (popcorn.StopFunc, error) {
			close(done)
			return func(context.Context) error { return nil }, nil
		},
		Done: done,
	})
	if err != nil {
		fmt.Println("module:", err)
		return
	}

	k, err := popcorn.NewKernel(popcorn.WithBus(bus), popcorn.WithModules(job))
	if err != nil {
		fmt.Println("kernel:", err)
		return
	}

	err = k.Start(context.Background())
	fmt.Println("stopped:", errors.Is(err, popcorn.ErrKernelStopped))

	// Start has returned, so the whole lifecycle is buffered. Close the
	// subscription and drain it.
	bus.Unsubscribe("probe")
	for e := range states {
		change, ok := e.Payload.(popcorn.KernelStateChanged)
		if !ok {
			continue
		}
		fmt.Println(change.To)
	}

	// Output:
	// stopped: true
	// starting
	// running
	// stopping
	// stopped
}
