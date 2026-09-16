// Package popcorn is a microframework for building modular Go applications out
// of independent, dependency-ordered components called [Module] values.
//
// A Module is a self-contained unit that knows how to start and stop itself.
// The [Kernel] starts modules in dependency order, watches their health, and
// shuts the whole set down gracefully. Modules never call each other directly:
// they publish and subscribe to events on a [Bus].
//
// # Getting started
//
// Build each module with [NewModule] and a [ModRecipe], create a [Bus] for the
// modules to share, register the modules with [WithModules], then run the kernel:
//
//	bus, err := popcorn.NewBus()
//	if err != nil {
//		return err
//	}
//	defer bus.Close()
//
//	db, err := popcorn.NewModule(popcorn.ModRecipe{
//		ID: "database",
//		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
//			conn, err := sql.Open("postgres", dsn) // dsn comes from config
//			if err != nil {
//				return nil, err
//			}
//			return popcorn.StopFuncFromCloser(conn), nil
//		},
//	})
//	if err != nil {
//		return err
//	}
//
//	k, err := popcorn.NewKernel(popcorn.WithBus(bus), popcorn.WithModules(db))
//	if err != nil {
//		return err
//	}
//	return k.Start(ctx)
//
// [Kernel.Start] blocks until the context is canceled, a module fails to start,
// a module reports itself unhealthy, or every [TaskModule] has finished.
//
// # Modules
//
// A module has an id and a list of dependency ids. The kernel starts a module
// only after every dependency's Start has returned, whether the dependency is
// long-running or a finite TaskModule. A dependency is identified by id, and
// the kernel rejects unknown, duplicate, self, and circular dependencies.
//
// Setting [ModRecipe.Done] makes the module a TaskModule: one that performs
// finite work and closes Done when it is finished. The kernel exits once there
// is at least one TaskModule and all of them are done; see [WithExitWhenIdle].
//
// # Events
//
// [Bus] is the single communication mechanism: modules publish events and
// subscribe to the events they care about. A subscriber receives every event
// and filters locally, by [Event.Kind] or by the concrete payload type:
//
//	type ConfigReloaded struct{ Path string } // an application payload
//
//	events, err := bus.Subscribe("httpserver",
//		popcorn.WithBacklog(64),
//		popcorn.WithFilter(func(e popcorn.Event) bool {
//			_, ok := e.Payload.(ConfigReloaded)
//			return ok
//		}))
//	if err != nil {
//		return nil, err
//	}
//	go func() {
//		for e := range events {
//			cfg, ok := e.Payload.(ConfigReloaded)
//			if !ok {
//				continue
//			}
//			_ = cfg // apply the new configuration
//		}
//	}()
//
// Publish through a bound [Publisher], which stamps [Event.Source] with the
// publisher's id and skips that same subscription:
//
//	if err := bus.Publisher("httpserver").Send(ctx, popcorn.NewEvent(ConfigReloaded{})); err != nil {
//		// handle the send failure
//	}
//
// [NewEvent] derives the kind from the payload type; [NewEventOf] sets it
// explicitly for cases where the type-derived kind is not appropriate.
//
// # Health
//
// Health is just an event. A module reports its state by sending a
// [ModuleStateChanged] event. The kernel watches these from registered ids and
// shuts down when one reports [ModuleStateNOK]:
//
//	cause := errors.New("disk full")
//	if err := bus.Publisher("database").Send(ctx, popcorn.NewEvent(
//		popcorn.ModuleStateChanged{To: popcorn.ModuleStateNOK, Cause: cause},
//	)); err != nil {
//		// handle the send failure
//	}
//
// The kernel publishes its own lifecycle as [KernelStateChanged] and each
// successful start as [ModuleStarted]. Readiness, liveness, and startup probes
// subscribe to these events like any other subscriber; the framework ships no
// probe API.
//
// # Shutdown
//
// When a run ends, the kernel stops modules in waves, with every dependent
// stopped before the dependencies it relies on. Each [StopFunc] receives a
// shutdown context carved out of a shared budget; see [WithStopTimeout].
// A [Bus] the kernel created itself is closed on shutdown; a Bus supplied with
// [WithBus] is owned by the caller, who should close it with [Bus.Close].
//
// [Kernel.Start] returns:
//
//   - [ErrKernelStopped] (wrapped) after a graceful stop, whether ctx was
//     canceled or the kernel became idle,
//   - a [KernelUnhealthyError] when a module reported NOK,
//   - the error returned by a module's Start, when a module fails to start,
//   - the joined errors from module StopFuncs, if any.
//
// A canceled stop also matches context.Canceled or context.DeadlineExceeded
// through [errors.Is], so the cause stays inspectable alongside the sentinel.
//
// Classify the result with [errors.Is] and [errors.AsType]:
//
//	err := k.Start(ctx)
//	if errors.Is(err, popcorn.ErrKernelStopped) {
//		// normal exit
//	}
//	if unhealthy, ok := errors.AsType[popcorn.KernelUnhealthyError](err); ok {
//		slog.Error("module failed", "module", unhealthy.ModuleID, "cause", unhealthy.Cause)
//	}
//
// # Wiring
//
// [NewKernel] creates its own Bus when [WithBus] is omitted, but that Bus is not
// exposed to modules. To let modules publish or subscribe, create a Bus with
// [NewBus], hold it on each module or capture it in Start, and pass it to
// [WithBus]. The kernel closes only a Bus it created itself; close your own with
// [Bus.Close].
//
// See the examples below for runnable, tested usage.
package popcorn
