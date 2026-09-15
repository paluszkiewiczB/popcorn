# Popcorn — Keep

> Target design: [REDESIGN.md](./REDESIGN.md). Everything below is carried into the
> redesign.

This document records the decisions, patterns, and implementations that are worth
**preserving** through a redesign. It is the counterpart to
[BUGS.md](./BUGS.md) and [DESIGN.md](./DESIGN.md): those catalogue what is wrong,
this catalogues what is right and why.

## Contents

- [Microkernel architecture and file layout](#microkernel-architecture-and-file-layout)
- [Dependency injection with functional options](#dependency-injection-with-functional-options)
- [Dependency graph and startup ordering](#dependency-graph-and-startup-ordering)
- [Error taxonomy](#error-taxonomy)
- [Context retention helpers](#context-retention-helpers)
- [Atomic state storage](#atomic-state-storage)
- [Generic, type-derived events](#generic-type-derived-events)
- [Lifecycle extension via `TaskModule`](#lifecycle-extension-via-taskmodule)
- [Graceful shutdown](#graceful-shutdown)
- [Observability primitives](#observability-primitives)
- [Concurrency hygiene](#concurrency-hygiene)
- [The HTTP module pattern](#the-http-module-pattern)
- [Testing approach](#testing-approach)
- [Tooling and reproducibility](#tooling-and-reproducibility)

## Microkernel architecture and file layout

The core split is sound and worth keeping:

- `Kernel` owns lifecycle, ordering, health, and shutdown.
- `Module` is a self-contained unit with `ID`, `Dependencies`, and `Start`.
- The `Bus` is a separate concern with its own options.
- `events.go`, `state.go`, `module.go`, and `kernel.go` each hold one concern.

Optional capabilities are expressed as small interfaces (`TaskModule`,
`EventReceiver`) rather than flags or base structs, so a module opts into exactly
what it needs. This is the "open-closed" shape the design claims, and it holds up.

## Dependency injection with functional options

`NewBus(...BusOption)` and `NewKernel(...KernelOption)` both follow the same
pattern: a private config struct populated with defaults, options that mutate it,
and a single construction point. There are no package-level singletons for the
framework's own dependencies; the bus, modules, tick interval, and stop timeout are
all injected. `NewKernel` additionally creates a default `Bus` when `WithBus` is
omitted (`kernel.go:159-166`), which gives sensible zero-config ergonomics without
hiding the injection point. (The logger defaults to the process-global
`slog.Default()` at `bus.go:70` and `kernel.go:148`, which is a deliberate
convenience rather than an injected dependency.) Omitted options fall back to
sensible defaults: a 1s bus send timeout (`bus.go:69`) and a 1s health tick with a
5s stop timeout (`kernel.go:74-75`). `NewModule` also reserves `EventSourceKernel`,
so a module cannot claim the kernel's id (`module.go:90-92`).

## Dependency graph and startup ordering

The dependency subsystem (`kernel.go:294-382`) does real work that is easy to get
right and easy to get wrong:

- It validates unknown dependencies and self-dependencies before sorting
  (`validateDeps`, `kernel.go:320-332`).
- It computes in-degrees and a dependents adjacency list in one pass
  (`buildDegree`, `kernel.go:334-347`).
- `topologicalSort` keeps the ready set sorted (`slices.Sort`) each time it grows,
  so startup order among independent modules is **deterministic** rather than
  map-order dependent (`kernel.go:349-382`).
- The cycle check via `len(order) != len(modules)` is correct under the
  already-enforced unique-ID precondition.

The decomposition into `buildIndex` / `validateDeps` / `buildDegree` /
`topologicalSort` is itself a maintainability win. Deterministic startup order is a
genuinely valuable property for a framework and should survive the redesign.

## Error taxonomy

The package defines a sentinel for each distinct configuration failure
(`ErrNilModule`, `ErrDuplicateModuleID`, `ErrUnknownDependency`, `ErrSelfDependency`,
`ErrCircularDependency`, `ErrEmptyID`, `ErrNilChannel`, `ErrDuplicateID`,
`ErrModuleIDNotSet`, `ErrModuleIDReserved`, `ErrModuleStartNotSet`) and wraps them
with `%w` and context (`fmt.Errorf("starting module %s: %w", ...)`). Consumers can
use `errors.Is` / `errors.As` rather than string matching. `KernelUnhealthyError`
implements `Unwrap` (`kernel.go:28-30`), so the cause participates in
`errors.Is`/`errors.As`; this is exercised in `kernel_test.go:288-294`. The
foundation is right; the caveat is that enforcement is incomplete for custom
`Module` implementations ([BUGS.md B20, B24](./BUGS.md)), which is a coverage
question rather than a flaw in the pattern.

## Context retention helpers

`RetainContext` and `RetainContextCause` (`kernel.go:468-478`) wrap
`context.WithoutCancel` plus a fresh cancel, giving modules an explicit way to
detach work from the startup context. The *intent* — don't let a module accidentally
cancel long-lived work when the start sequence ends, and don't retain the request
context — is good; the helpers make the supported pattern discoverable.

## Atomic state storage

`ModuleStateStore` (`state.go`) is a thin, allocation-free type over
`atomic.Int32` with `Set`, `Get`, and `CAS`. Using lock-free primitives for a
single hot state value is appropriate, and exposing `CAS` (even if currently
underused) supports compare-and-swap state transitions. It is also deliberately
zero-value-safe, which sets a good bar the export surface should match. The idea of
a dedicated store, rather than scattered atomics on the kernel, is worth keeping.

## Generic, type-derived events

`Event` with a generic `NewEvent[T]` / `BaseEvent[T]` pair (`events.go`) removes a
whole class of stringly-typed bugs: the event kind is derived from the payload
type instead of being passed by hand, and `Payload any` is paired with the
instruction to assert safely. The `Event.LogValue` / `LogValuer` integration means
events log cleanly with `slog`. The concept is right; the `reflect.Type.Name()`
edge cases ([BUGS.md B14](./BUGS.md)) are an implementation detail, not a reason to
abandon it. `Event` is a value struct with an `any` payload (`events.go:12-23`), so
events copy and compare safely, which is why tests can assert with `is.Equal`.

## Lifecycle extension via `TaskModule`

`TaskModule` (`module.go:31-35`) is a clean, additive way to express finite work:
implement `Done()`, close the channel, and the kernel shuts down when all tasks
finish. It lets the same framework serve both servers and CLI-style programs
without a separate mode or flag. Keeping this as a separate interface rather than a
field on `Module` is the correct call.

## Graceful shutdown

Despite the timeout bug ([BUGS.md B18](./BUGS.md)), the shutdown shape is right:
stop modules in **reverse start order** (`slices.Backward`), invoke their
`StopFunc`s, aggregate errors with `errors.Join`, and give shutdown a context built
with `context.WithoutCancel` plus its own timeout (`kernel.go:439-458`). Reverse
order teardown and error aggregation are the right semantics for a module system
and should be preserved. The `watchTasks` helper's use of a nil channel to disable
the `taskDone` select case when there are no task modules (`kernel.go:395`,
`kernel.go:425-427`) is also a correct, idiomatic trick worth keeping.

## Observability primitives

- The narrowed `plog.Logger` interface (`LogAttrs` + `With`) is the right *idea*:
  the kernel should depend on a minimal logging surface, not all of `slog`.
- `plog/attr` provides consistent, reusable attributes (`ModID`, `Err`,
  `NamedErr`, `Strings`, `TypeOf`) so log lines are structured rather than
  formatted strings. `TypeOf` correctly handles typed-nil pointers
  (`plog/attr/attrs.go:39-43`).
- The compile-time assertion `var _ Logger = (*slog.Logger)(nil)`
  (`plog/slog.go:10`) documents the compatibility contract.
- `Event.LogValue` implements `slog.LogValuer` (`events.go:50-53`), so events
  render as structured log values through the standard logging pipeline (the value
  itself currently omits `Payload`, [BUGS.md B13](./BUGS.md)).
- `ModuleStarted` carries `Order` and `StartTook`, which is exactly the kind of
  startup telemetry a framework user wants without instrumenting anything.

(The `With` method and the logger-return concrete type are the parts called out in
[DESIGN.md](./DESIGN.md); the narrowed interface and the `attr` helpers are the
parts worth keeping.)

## Concurrency hygiene

Several small habits keep concurrent access honest and are worth carrying forward:

- `deliverToAll` snapshots the listener map with `maps.Copy` before iterating, so
  the read lock is not held during sends and callers cannot mutate internal state
  (`bus.go:224-227`).
- `Buffer` returns `slices.Clone(b.buf)`, so callers cannot mutate the internal
  buffer (`bus.go:185-190`).
- `Send` uses `context.WithoutCancel` plus its own timeout rather than mutating the
  caller's context (`bus.go:216-218`).
- The selected `plog`/`attr` boundaries keep logging reentrant and side-effect free.
- Listeners are typed `chan<- Event` at the bus boundary (`bus.go:31`,
  `bus.go:90`), encoding the send direction in the type.
- `sendEvent` bounds each listener send with a `select` over `ctx.Done()` and the
  channel (`bus.go:248-254`), so no single send can block without a timeout (the
  sequencing and error-reporting issues are separate,
  [BUGS.md B9](./BUGS.md)).

## The HTTP module pattern

The example's `HTTPModule` (`examples/main.go`) demonstrates the right server
lifecycle: bind the listener first (so the address is known and errors are returned
from `Start`), start `Serve` in a goroutine, and drain the serve error in the
returned `StopFunc`, normalizing `http.ErrServerClosed` to nil
(`examples/main.go:52-87`). The **ordering** — listen, then serve, then drain —
is worth promoting as the canonical long-running-module example, even though the
shutdown context and the missing address event are wrong as written
([BUGS.md B25, B26](./BUGS.md)).

## Testing approach

- Tests use **real channels and real components**, not mocks, which matches the
  project's stated preference and keeps tests close to real behaviour.
- `github.com/matryer/is` gives compact, readable assertions.
- There is an end-to-end test that exercises dependency ordering, event receipt,
  and unhealthy-kernel propagation together, plus targeted unit tests per file.
- The `Taskfile` provides separate `test` and `test:race` tasks, so the intent to
  run everything under the race detector is already encoded (the two tasks and
  `CONTRIBUTING.md:70` currently disagree about which one is the default gate).

## Tooling and reproducibility

- `Taskfile.yaml` centralizes the developer workflow (`test`, `test:race`, `run`,
  `test:reports`, `lint`, `fix`) in one place.
- `tools/go.mod` pins tool versions and installs them into `.tools/`, which is the
  right mechanism for avoiding "works on my machine" drift. It is a separate module
  using the Go `tool` directive (`tools/go.mod:5-9`), so tooling dependencies stay
  out of the library's module graph; the root `go.mod` requires only `matryer/is`
  (`go.mod:5`).
- `devbox` provides a pinned shell environment and GitNexus indexes the repository
  for code-intelligence queries. The *intent* to pin a reproducible toolchain is
  what is worth keeping; the versions currently disagree across `devbox.json`,
  `go.mod`, `Taskfile.yaml`, and `tools/go.mod`, and the devbox files are untracked
  ([BUGS.md B31, B33](./BUGS.md)).
- The repository already has clear conventions documents (`CONTRIBUTING.md`,
  `AGENTS.md`) and a package doc comment (`doc.go`); the foundation is there, it
  just needs to match the code.
