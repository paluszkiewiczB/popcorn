# Popcorn — Redesign

This is the frozen redesign plan. It is the target the other documents feed into:
[BUGS.md](./BUGS.md) catalogues the current defects, [DESIGN.md](./DESIGN.md)
catalogues the design problems, [KEEP.md](./KEEP.md) records what must survive, and
[api.go](./api.go) drafts the new public surface. Example usages live under
[`examples/`](./examples).

## Contents

- [Principles](#principles)
- [Decisions](#decisions)
- [Target public API](#target-public-api)
- [Completely internal](#completely-internal)
- [Event model](#event-model)
- [Bus model](#bus-model)
- [Dependency scheduler](#dependency-scheduler)
- [Context lifecycle](#context-lifecycle)
- [Keep / redesign / delete](#keep--redesign--delete)
- [Functionality preservation](#functionality-preservation)
- [Bug resolution map](#bug-resolution-map)
- [Migration outline](#migration-outline)

## Principles

- Exactly one owner per channel: the bus creates, writes, and closes subscription
  channels; modules only read.
- One mechanism for communication: the event bus covers module→kernel, module→module,
  and kernel→module. Health is just an event.
- Explicit subscriptions: a module subscribes itself, when it is ready, through the
  injected bus. No receiver interfaces, no kernel runtime checks.
- Identity is enforced at the seam: publishers are bound to an id, so `Event.Source`
  is trustworthy and cannot be set by callers.
- Bounded everything: per-subscription rings and a bounded bus replay history; a slow
  subscriber only overflows its own ring.
- Health is best-effort: it travels on the shared bus like any other event.
- Lightweight events: small copyable values, no pointers, no wrappers.

## Decisions

- **Kernel lifetime:** single-shot. A second `Start` returns `ErrKernelStarted`.
- **Normal shutdown:** `Start` returns `ErrKernelStopped` (wrapped) on a graceful
  stop, so callers use `errors.Is` instead of special-casing `context.Canceled`.
- **Module events:** no `EventReceiver`/`EventFilterer`. A module calls
  `bus.Subscribe(id, WithBacklog(n), WithFilter(f))` itself, typically inside `Start`.
- **Replay:** the bus owns a bounded history (`WithReplayBuffer(n)`, default off).
  `Subscribe` atomically seeds a new subscription with matching recent events, then
  registers it live — no gap, no duplicate.
- **Publishing:** `bus.Publisher(id)` returns a bound `Publisher`; `Send` sets
  `Event.Source` itself and skips the sender's own subscription.
- **Overflow:** fixed; the oldest event is overwritten. There is no configurable
  policy.
- **Health:** ordinary events on the shared bus, best-effort. The kernel subscribes
  and validates `Event.Source` against registered modules; invalid or spoofed events
  are logged and dropped.
- **State:** two separate, self-contained enums and events — `ModuleState` /
  `ModuleStateChanged` for health, `KernelState` / `KernelStateChanged` for
  lifecycle. No unified enum, no tagged union. Modules report only the new state; the
  kernel records and validates transitions.
- **Readiness:** a dependency is ready when a normal module's `Start` returns, or
  when a `TaskModule`'s `Done` closes. Dependents start only after all dependencies
  are ready.
- **Exit:** the kernel auto-stops when every module is a `TaskModule` and all are
  done. If any long-running module is present it runs until cancellation or failure,
  unless `WithExitWhenIdle(true)` is set.
- **Probes:** not built in. Liveness/readiness/startup endpoints are an ordinary
  module consuming `KernelStateChanged`/`ModuleStateChanged`.
- **Logging:** `*slog.Logger`. The `plog` package is removed.
- **Context helpers:** `RetainContext`/`RetainContextCause` are removed. The kernel
  derives all contexts from the run context, so values are preserved.
- **Parallelism:** `WithParallelism(p)`; `1` is sequential, `0` means `GOMAXPROCS`.
- **Dependency algorithm:** DFS/Tarjan for cycle detection (reporting the cycle
  path) plus Kahn's algorithm with a deterministic tie-break for ordering.

## Target public API

The authoritative draft is [api.go](./api.go). Summary:

```go
// Modules
type Module interface { ID() string; Dependencies() []string; Start(ctx context.Context) (StopFunc, error) }
type TaskModule interface { Module; Done() <-chan struct{} }
type StartFunc func(context.Context) (StopFunc, error)
type StopFunc func(context.Context) error
func StopFuncFromCloser(io.Closer) StopFunc

// Events
type Event struct { Kind string; At time.Time; Payload any } // source is private
func (e Event) Source() string
func NewEvent[T any](payload T) Event
func NewEventOf(kind string, payload any) Event
type ModuleStarted struct{ ID string; Order int; StartTook time.Duration }
type ModuleStateChanged struct{ To ModuleState; Cause error }
type KernelStateChanged struct{ From, To KernelState; Cause error }

// Bus
type Bus struct{ /* private */ }
func NewBus(opts ...BusOption) (*Bus, error)
func (b *Bus) Subscribe(id string, opts ...SubOption) (<-chan Event, error)
func (b *Bus) Unsubscribe(id string)
func (b *Bus) Publisher(id string) Publisher
type Publisher interface { Send(ctx context.Context, e Event) error }

// Kernel
type Kernel struct{ /* private */ }
func NewKernel(opts ...KernelOption) (*Kernel, error)
func (k *Kernel) Start(ctx context.Context) error
```

## Completely internal

- `depGraph`, cycle detection, topological ordering, parallel scheduler.
- The concrete module representation and the recipe builder internals.
- The atomic state store and the per-module state map.
- Subscription ring buffers, delivery goroutines, listener map, snapshots, and the
  replay history ring.
- The kernel listener/task watchers and lifecycle state machine internals.
- `eventKind`, log attribute helpers.

## Event model

- `Event` is a small value type. `Kind` is derived from the payload type by
  `NewEvent[T]`; `NewEventOf` is the explicit escape hatch. The derivation must be
  robust for pointer, unnamed, and interface types (the current implementation
  panics for `any` and collapses unnamed types to `""` — BUGS B14).
- `Source` is private and set only by the bound `Publisher`. `Source()` reads it.
- `ModuleStateChanged` carries `To` + `Cause`; the kernel derives and records the
  previous state. `KernelStateChanged` carries `From`/`To`/`Cause`.
- `ModuleStarted{ID, Order, StartTook}` is kernel-emitted, so it carries `ID`.

## Bus model

- One subscription = one bounded ring + one bus-owned output channel + one delivery
  goroutine.
- `Subscribe(id, WithBacklog(n), WithFilter(f))` creates the subscription and returns
  the receive-only channel. `Unsubscribe` stops the goroutine and closes the channel.
- The bus keeps a bounded replay history when `WithReplayBuffer(n) > 0`. `Subscribe`
  seeds the new subscription from history and registers it live atomically.
- `Publisher(id).Send(ctx, e)` enqueues into each matching subscription without
  blocking, skips the sender's own subscription, and overwrites the oldest event on
  overflow.
- Filters run outside all locks and are panic-recovered.
- Health is best-effort: a burst can overflow the kernel's subscription too.

## Dependency scheduler

- **Validation:** nil and typed-nil modules, empty ids, duplicate ids, unknown
  dependencies, self-dependencies, and duplicate dependency entries.
- **Cycle detection:** DFS three-coloring or Tarjan SCC; the error includes the cycle
  path (`a -> b -> a`).
- **Ordering:** Kahn with a deterministic ready queue (tie-break by id).
- **Readiness:** a node becomes startable when every dependency is ready — normal
  module when `Start` returns, `TaskModule` when `Done` closes.
- **Parallel start:** a worker pool of size `p`. On the first failure, scheduling
  stops, already-started modules are stopped in reverse dependency order, and errors
  are joined.
- **Parallel stop:** reverse dependency order; the same pool.
- **Exit:** all-task kernels exit when idle; long-running kernels run until
  cancellation; `WithExitWhenIdle` overrides.

## Context lifecycle

- `Kernel.Start(ctx)` is the single source of context values (tracing, baggage).
- Each `Module.Start(ctx)` receives a context derived from the run context and scoped
  to that module's lifetime; it is canceled as part of that module's shutdown.
- `StopFunc(ctx)` receives a fresh context derived from the run context with
  `context.WithoutCancel` plus the stop timeout, so **values are preserved** and only
  cancellation/deadline are reset.
- `Publisher.Send(ctx, ...)` passes the caller's context through unchanged.
- Modules that need their own detached-but-valued context call
  `context.WithoutCancel` directly.

## Keep / redesign / delete

- **Keep (~35%):** functional-options pattern, `Module`/`TaskModule`, generic
  type-derived events, lightweight `Event`, reverse-order shutdown, `errors.Join`,
  sensible defaults.
- **Redesign (~50%):** dependency scheduler, bus ownership/delivery/replay/filters,
  kernel lifecycle state machine, health validation, explicit subscription model,
  validation coverage.
- **Delete (~15%):** all buffering methods, exported `ModuleStateStore`,
  `EventReceiver`/`EventFilterer`, `OverflowPolicy`, health wrappers,
  `EventLogValue`, `ErrContextCanceled`, `RetainContext`/`RetainContextCause`,
  `plog`, `internal/rand.go`, `cmd/debug`.

## Functionality preservation

| Current capability | New home |
|---|---|
| Dependency-order startup | Kahn ordering + parallel scheduler |
| Reverse-order graceful stop, `errors.Join` | Same, now parallel with `WithParallelism` |
| `TaskModule` (finite work) | Unchanged interface; also gates dependents via `Done` |
| Module event reception | Explicit `bus.Subscribe` inside `Start`; bus-owned channel |
| Per-listener send timeout | Per-subscription bounded ring + replay history |
| Startup / late-subscriber replay | `WithReplayBuffer(n)` + `WithBacklog(n)` |
| `NOK` stops the kernel | Validated `ModuleStateChanged` → per-module state → shutdown |
| Liveness / readiness / startup | `KernelStateChanged`/`ModuleStateChanged` consumers |
| Cross-module messaging | Generic bus + per-subscription filter |
| Structured logging | `*slog.Logger` directly |
| Nil/zero-value safety | All methods nil-safe; zero values rejected clearly |

## Bug resolution map

| Bug | Resolution |
|---|---|
| B1–B8 (buffering/TOCTOU/replay/leaks) | Buffering API removed; replaced by bounded subscription rings + bus history |
| B9 (sequential delivery, shared timeout) | Per-subscription delivery, non-blocking enqueue, drop-oldest |
| B10 (nil/zero-value safety) | Validation in constructors; all bus methods nil-safe |
| B11–B12 (invalid options, nil ctx) | Option validation at construction; ctx guards |
| B13 (EventLogValue overflow) | `EventLogValue` removed; events log as values |
| B14 (eventKind) | Reimplemented type-name derivation (package path + non-named handling) |
| B15 (nokModuleID race) | Per-module state map guarded by the kernel; no id-on-the-side |
| B16–B17 (Start leaks, watchTasks) | Lifecycle-scoped contexts, owned goroutines, single-shot kernel |
| B18 (shared stop timeout) | Per-module stop contexts from one budget |
| B19 (health spoofing, unused states) | Bound source + validation; OK/TempNOK given defined semantics |
| B20–B21 (validation, tick panic) | Constructor validation; no panics for invalid config |
| B22 (restart) | Explicit `ErrKernelStarted`; module subscriptions owned and closed |
| B23 (ModuleStarted abort, pointer payload) | Startup events published without aborting; payload typing documented |
| B24 (reserved id bypass) | Validation centralized for all `Module` implementations |
| B25–B27 (examples) | Examples rebuilt (see `examples/`) |
| B28–B30, B36 (tests) | Test suite rewritten against the new contract |
| B31–B33, B34, B35, B37 (tooling, exports, aliasing, channels, deps) | Bound channels, defensive copies, duplicate-dep validation, repo hygiene |

## Migration outline

1. Land the new event type and bus (ownership, rings, history, filters, bound
   publisher).
2. Replace the dependency graph with validation + Tarjan/Kahn + parallel scheduler.
3. Rework the kernel lifecycle (state machine, contexts, single-shot, health
   validation, probes as a module, readiness/idle exit).
4. Rebuild the examples and the test suite against the new contract.
5. Delete the dead surface (`plog`, buffering methods, `internal/rand.go`,
   `cmd/debug`) and finish the tooling/repo cleanup (BUGS B31–B33).
