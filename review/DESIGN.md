# Popcorn — Design

> Target design: [REDESIGN.md](./REDESIGN.md). This file documents the current
> design problems only; it intentionally contains no fixes.

This document is about **design and ergonomics**, not correctness. Concrete
defects (races, lost events, leaks) are in [BUGS.md](./BUGS.md). This file explains
which design choices make the package hard to use, hard to extend, and hard to
reason about, with the assumption that the package may be redesigned from scratch.
It deliberately does not propose fixes.

[KEEP.md](./KEEP.md) records the parts that are worth carrying into a redesign.

## Contents

- [Buffering is a public concept with no single contract](#buffering-is-a-public-concept-with-no-single-contract)
- [Delivery semantics are all-or-nothing and non-isolated](#delivery-semantics-are-all-or-nothing-and-non-isolated)
- [The kernel is just another bus subscriber](#the-kernel-is-just-another-bus-subscriber)
- [Context ownership is surprising](#context-ownership-is-surprising)
- [Health is passive, global, and unverifiable](#health-is-passive-global-and-unverifiable)
- [Module interface and construction ergonomics](#module-interface-and-construction-ergonomics)
- [The logger abstraction cannot be substituted](#the-logger-abstraction-cannot-be-substituted)
- [Constructor and options ergonomics](#constructor-and-options-ergonomics)
- [Public API surface contradicts the stated goal](#public-api-surface-contradicts-the-stated-goal)
- [The error model is inconsistent](#the-error-model-is-inconsistent)
- [The lifecycle model is under-specified](#the-lifecycle-model-is-under-specified)
- [Documentation and documented contracts](#documentation-and-documented-contracts)
- [Internal consistency and naming](#internal-consistency-and-naming)
- [Test-suite contract](#test-suite-contract)

## Buffering is a public concept with no single contract

The bus exposes five buffering methods:

- `StartBuffering()`
- `FinishBuffering()`
- `SetBuffering(bool)` (deprecated)
- `ClearBuffer()` (deprecated)
- `Buffer() []Event`

Two of them are deprecated forwarders whose behaviour differs from their own
names, and there is no documented contract that says whether the buffer is a
*replay log for late subscribers* or a *pending-delivery queue flushed on finish*.
The implementation attempts to be both at once, which produces the duplicate
delivery and lost-event behaviour in [BUGS.md B1, B2, B4, B5, B7, B8](./BUGS.md).
A consumer cannot tell, from the API, when an event is delivered, whether it will
be delivered again, or what `FinishBuffering` guarantees. The concept also leaks
internal mechanics (`SetBuffering`, `ClearBuffer`, `Buffer`) into the public API,
which the stated goal of a "tiny public API surface" does not support.

A related contract gap: because buffering is enabled for the whole kernel lifetime
([BUGS.md B3](./BUGS.md)), the "replay" a module receives is not limited to
startup traffic — it is every event sent before that module subscribed. "Replay
the bootstrap events" and "replay everything up to subscription" are different
contracts, and the code does not state which it intends.

Buffering is also global mutable state on the bus, not scoped to a kernel.
`WithBus` explicitly supports sharing a bus (`kernel.go:105`), but `Kernel.Start`
unconditionally calls `StartBuffering()`, which clears any existing buffer
(`bus.go:137-140`), and every kernel subscribes under the reserved `"kernel"` id
(`kernel.go:217`). Two kernels sharing a bus reset each other's replay, and the
second `Start` fails with `ErrDuplicateID` ([BUGS.md B22](./BUGS.md)).

## Delivery semantics are all-or-nothing and non-isolated

`Bus.Send` delivers to listeners sequentially with one shared timeout and reports a
single error for the whole operation. There is no way for a caller to:

- distinguish "no listeners" from "all listeners received it" from "some listeners
  timed out";
- isolate a slow listener so it does not affect the others;
- observe per-listener results.

Because delivery iterates a map, ordering is nondeterministic, and because a single
expired context makes the whole `Send` return an error, the returned error
over-reports failure. Consumer-side, this means a module cannot rely on the error
to make a meaningful decision. See [BUGS.md B9](./BUGS.md).

## The kernel is just another bus subscriber

The kernel subscribes its own cap-1 channel to the bus under the id `"kernel"`
(`kernel.go:210-217`) and receives **every** event on the bus, filtering only by
payload type (`kernel.go:461-466`); `deliverToAll` does not consult
`Event.Source` (`bus.go:235-239`). Health therefore travels over the same
best-effort, timeout-bounded, lossy channel as ordinary traffic: a busy bus can
starve or delay health delivery, and a dropped health event is never noticed. The
subscription id `"kernel"` also implies a filtering that does not exist. This
"kernel is a normal subscriber" model is central design context and is the reason
health can be spoofed ([BUGS.md B19](./BUGS.md)). The dependency is also concrete:
`Kernel` holds a `*Bus` (`kernel.go:84`) and `WithBus` accepts `*Bus`
(`kernel.go:105`), so the bus cannot be substituted or faked at the kernel
boundary, contrary to `CONTRIBUTING.md:11` ("Define interfaces where they are
consumed").

## Context ownership is surprising

`Send` takes a `context.Context` and then strips its cancellation with
`context.WithoutCancel`, replacing it with an internal per-send timeout. The
documented intent is that delayed events are acceptable, but the effect is that a
caller can never cancel a `Send`, and a caller's own deadline is ignored. A caller
passing a context with a shorter deadline will not get the expected behaviour.

Separately, the module contract is asymmetric: `Module.Start` receives the kernel's
context, while `StopFunc` receives a different, freshly created shutdown context.
The kernel documents this, but nothing in the type system enforces that a module
must not retain the start context; `RetainContext` / `RetainContextCause` are
opt-in escape hatches rather than the default. The example module gets this wrong
([BUGS.md B26](./BUGS.md)), which suggests the contract is easy to misread.

## Health is passive, global, and unverifiable

The kernel does not inspect modules for health. It only reacts to
`ModuleStatusChanged` events that a module must voluntarily publish on the bus, and
only when `To == ModuleStateNOK` ([BUGS.md B19](./BUGS.md)). `ModuleStateTempNOK`
exists but is never produced or consumed. There is a single global kernel state, so
there is no way to express partial degradation, per-module status, or recovery.
`ModuleState` has no `String()` method (`kernel.go:33-48`), so health is neither
printable nor loggable in a human-readable form. There is also no exported accessor
for the current state: `Kernel` exposes only `Start`, and `k.state` is an
unexported field (`kernel.go:88`). A consumer cannot query liveness or readiness at
all except by consuming events — even though `README.md:11` states that "Popcorn
Core exposes its current state (liveness, readiness) through the probes and events".
No probes exist anywhere in the package.

## Module interface and construction ergonomics

- `EventReceiver.Events()` returns a bidirectional `chan Event`
  (`module.go:38-42`), while the kernel only sends into it; the signature does not
  express which side is the sender, and a consumer can also send into the channel.
  Because it is bidirectional, a module can also close it, but nothing defines
  whether it may or what happens when it does ([BUGS.md B35](./BUGS.md)).
- `NewModule` returns the `Module` interface rather than a concrete type, which
  contradicts the project's own style guide.
- `ModRecipe` and the `Module` interface duplicate the same shape (id, dependencies,
  start), so there are two ways to describe a module with different capabilities
  (the recipe adds an optional events channel; the interface adds `Done()` only via
  `TaskModule`). `ModRecipe` has no field for `Done()`, so `NewModule` can produce a
  `Module` or `EventReceiver` but never a `TaskModule`; a finite-work module must
  bypass `NewModule` and implement the interfaces by hand.
- Validation lives in `NewModule` (empty id, reserved id, nil start), but a custom
  `Module` implementation bypasses all of it; the kernel only re-checks nil and
  duplicate ([BUGS.md B20, B24](./BUGS.md)).
- The docs recommend unbuffered or cap-1 event channels (`CONTRIBUTING.md:37`),
  while the replay mechanism ([BUGS.md B4](./BUGS.md)) needs more capacity to work
  at all. The recommended usage and the framework's behaviour are in tension.
- `Event` is an open, mutable struct: `Kind`, `Source`, `Payload`, and `At` are all
  exported, `NewEvent` does not validate `Kind` (`events.go:42-47`), and `Send`
  accepts any hand-built `Event{}`. The "kind is derived from the payload type"
  guarantee is therefore not enforced; a user can bypass `eventKind` entirely.
- `Event.Source` has a documented convention (`EventSourceKernel` or a module id)
  but nothing enforces it, and `ModuleStarted` / `ModuleStatusChanged` carry their
  own `ID` fields independent of `Event.Source`, so the same identity is expressed
  in two places.

## The logger abstraction cannot be substituted

`plog.Logger` is a narrow interface, which is the right instinct, but:

- `Logger.With(attrs ...any) *slog.Logger` (`plog/slog.go:15`) requires any
  implementation to return a concrete `*slog.Logger`; an independent logger cannot
  return its own type, so the abstraction cannot be carried end-to-end. The method
  is also never called anywhere in the repository, so the interface's only
  multi-method contract point is exercised by nothing.
- The option constructors accept `*slog.Logger`, not `plog.Logger`
  (`bus.go:59`, `kernel.go:113`), so the abstraction leaks at every entry point.
- A nil logger is accepted and only fails later at call time
  ([BUGS.md B11](./BUGS.md)).

As written, the abstraction does not buy testability or substitutability; it only
adds a layer.

## Constructor and options ergonomics

- `BusOption` and `KernelOption` are `func(*config) error`, but no option can ever
  return a non-nil error; the error plumbing is dead scaffolding that forces every
  caller to handle an impossible failure.
- Configuration is construct-time only. There is no way to register a module after
  `NewKernel`, to inspect the resolved startup order, or to reuse a kernel across
  runs. `Kernel` accumulates `started` state, a second `Start` is not supported,
  and module subscriptions are never removed ([BUGS.md B22](./BUGS.md)).
- Several invalid configurations (zero/negative tick, zero/negative timeout) are
  accepted at construction and only surface later, either as a panic or as
  permanent timeouts ([BUGS.md B11, B21](./BUGS.md)).
- `WithModules` appends to a slice held in the config; the option model mixes
  "set a scalar" and "append to a collection" without distinguishing the two.

## Public API surface contradicts the stated goal

`CONTRIBUTING.md` states that only what a consumer needs should be exported. In
practice the package exports the entire `ModuleStateStore` (a
`type ModuleStateStore atomic.Int32` with `Set`/`CAS`/`Get`) and `ModuleState`, a
plain `int32` with an unexported `asInt`, plus the buffering methods
`SetBuffering` and `ClearBuffer`, plus `EventLogValue` and the `RetainContext`
helpers. `SetBuffering` and `ClearBuffer` have no callers anywhere
in the repository (including tests and examples); `plog/attr.TypeOf`, `NamedErr`,
and `Strings` are used only by their own tests; and `EventLogValue`/`Event.LogValue`
is not exercised by any test. There is no clear line between the supported consumer
API and internal machinery.

## The error model is inconsistent

- `KernelUnhealthyError` is returned as a value (not a pointer); its `Error` method
  formats a nil `Cause` as `%!s(<nil>)` (`kernel.go:23-25`).
- `ErrContextCanceled` is a sentinel that is then wrapped together with
  `ctx.Err()` using two `%w` verbs (`kernel.go:394`), so the same failure surfaces
  under two identities; callers must know to test for either.
- On normal shutdown, `Kernel.Start` returns the joined errors from `StopFunc`s
  (`kernel.go:439-459`), which makes a successful run with a benign cleanup error
  look like a startup failure to the caller.
- The bus logs per-listener failures at warn level and also returns an aggregate
  error (`bus.go:236-243`), so failures are reported twice through two different
  channels.
- `KernelUnhealthyError` carries only `Cause error` (`kernel.go:16-25`); the
  offending module id is interpolated into a formatted string (`kernel.go:401`),
  with no typed cause or sentinel (`errModuleNOK` is an unexported const,
  `kernel.go:71`). Consumers are pushed toward string matching, which the tests do
  (`e2e_test.go:104`, `kernel_test.go:157`), contradicting `CONTRIBUTING.md:50`.

## The lifecycle model is under-specified

- The interaction between `TaskModule` (finite work) and ordinary long-running
  modules is undefined: if a kernel mixes both, completion of the tasks shuts the
  whole kernel down, which is likely not what a long-running service wants. This is
  already noted inline at `kernel.go:396`.
- Goroutines are created implicitly by the kernel (`listener`, `watchTasks`) and by
  `FinishBuffering`, but none of them have a documented owner or a cancellation
  path that a consumer can drive ([BUGS.md B6, B16, B17](./BUGS.md)).
- There is no defined state machine for a `Kernel` or a `Module` (unknown →
  starting → running → stopping → stopped). `ModuleState` describes *health*, not
  *lifecycle*, and the only lifecycle transition observable by a consumer is the
  `ModuleStarted` event.

## Documentation and documented contracts

- `doc.go` says "See the README for a quick-start example", but the README has no
  example, only a short prose description.
- `README.md:11` claims a probes-based liveness/readiness surface that does not
  exist in the code (see the health section above).
- `CONTRIBUTING.md` mixes contributor guidance (tooling, indexing) with
  consumer-facing framework usage ("how to write a module", "how to use the bus"),
  as its own inline comments note (`CONTRIBUTING.md:20,32,71,79`).
- `CONTRIBUTING.md` makes claims about the workflow that the `Taskfile` does not
  satisfy: `:70` says `task test` includes the race detector (it does not), and
  `:81` references a `task fmt` that is not defined. It also prescribes cap-1
  channels, an injected time source, and no `time.Sleep` in tests, none of which the
  bus or tests follow.
- The README has wording errors ("ment", "self-containing").
- There is no documented contract for event ordering, delivery guarantees, buffer
  capacity, or what `Send`'s error means, even though these are the questions a
  user of an event bus asks first.
- `Event.At` is documented as usable "to drop outdated `Event` by the listener"
  (`events.go:17-20`), but nothing in the package or the examples reads `At` for
  that purpose, and `Send` imposes its own delay semantics, so the documented advice
  has no supporting helper or example.

## Internal consistency and naming

- Nil-safety is inconsistent: only `Send` and `Unsubscribe` handle a nil receiver,
  several other methods panic, and the zero-value `Bus` is unusable
  ([BUGS.md B10](./BUGS.md)), yet the documentation claims the bus is nil-safe.
- The documented preference for cap-1 channels (`CONTRIBUTING.md:37`) is stated as
  if the bus supports it, while the buffering design requires larger buffers.
- `ModuleState` is an `int32` with an unexported conversion method; the atomic
  store is a type definition over `atomic.Int32` and is exported even though
  consumers only need the `ModuleState` value.
- Deprecated methods remain part of the public API with behaviour that differs from
  their names, so their names and semantics disagree.
- `RetainContext` and `RetainContextCause` return named results and a bare `cancel`
  of different types, inconsistent with the rest of the API's error-first style.

## Test-suite contract

The tests document the current ambiguous semantics rather than a contract. In
particular, `TestBus_Buffer_DeliversAndBuffersSimultaneously` asserts that an event
is delivered live *and* retained in the buffer, and `TestBus_Buffer_ReplayOnSubscribe`
relies on the buffer being replayed without being consumed
([BUGS.md B30](./BUGS.md)). Any redesign of buffering has to decide and re-encode
that contract, because the existing suite locks it in.
