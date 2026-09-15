# Popcorn — Bugs

> Target design and bug-resolution mapping: [REDESIGN.md](./REDESIGN.md). This file
> documents the current state only; it intentionally contains no fixes.

A descriptive catalogue of defects found by reading the source and running:

```
go test ./...                     # FAIL: TestBus_Buffer_TOCTOURace
go test -race ./...               # FAIL: data race on Kernel.nokModuleID
.tools/golangci-lint run ./...    # 7 issues
```

(`task test:race` runs `go test -count 1 -race -coverpkg=./... ./...`. The
`.tools/golangci-lint` binary is the one pinned by `tools/go.mod` and used by
`task lint`; the system-installed `golangci-lint` is a different, incompatible
version.)

This document only describes *what is wrong and how it manifests*. It contains no
severity ranking, no ordering by importance, and no proposed fixes. Design and
ergonomics concerns live in [DESIGN.md](./DESIGN.md); things worth preserving live
in [KEEP.md](./KEEP.md).

References are `file:line` as of the current working tree (commit `ebf1262` plus
uncommitted changes). Line numbers may drift as files change.

## Contents

- [Event bus (`bus.go`)](#event-bus-busgo)
- [Events (`events.go`)](#events-eventsgo)
- [Kernel (`kernel.go`)](#kernel-kernelgo)
- [Module / state](#module--state)
- [Examples](#examples)
- [Tests](#tests)
- [Tooling & repository](#tooling--repository)
- [Additional findings](#additional-findings)

## Event bus (`bus.go`)

### B1. The buffering barrier is dead code, so concurrent sends are lost (TOCTOU)

- **Location:** `bus.go:145-153` (`FinishBuffering`), `bus.go:205-214` (`Send`),
  `bus.go:35-40` (fields).
- **What happens:** `TestBus_Buffer_TOCTOURace` (`bus_test.go:213-254`) starts
  buffering, registers a subscriber with a 10-deep channel, launches 20 concurrent
  `Send`s and one `FinishBuffering`, then reads 20 events with a 1s timeout. It
  receives only 10, blocks on event #11, and fails at `bus_test.go:250`.
- **Why:** `Send` increments and decrements `pendingSends` inside a single
  acquisition of `bufMu` (`bus.go:205-211`). When `FinishBuffering` holds `bufMu`,
  `pendingSends` is therefore always `0`, so the loop
  `for b.pendingSends > 0 { b.flushCond.Wait() }` (`bus.go:147-149`) never waits.
  The counter cannot see `Send` goroutines that have not yet acquired the lock.
  `FinishBuffering` sets `buffering = false` before those goroutines run; their
  events are neither appended to `buf` nor reliably delivered live, because direct
  delivery has a shared per-`Send` timeout and the subscriber's channel is full.

### B2. `FinishBuffering` re-delivers events that were already delivered or replayed

- **Location:** `bus.go:205-220` (append + live delivery), `bus.go:107-116`
  (replay on subscribe), `bus.go:150-168` (flush on finish), `kernel.go:264`,
  `kernel.go:280-288` (kernel replay).
- **What happens:** The same event reaches a listener more than once.
  - A listener that existed before a `Send` receives the event live, and
    `FinishBuffering` flushes the same buffered copy to it again: two deliveries.
  - A listener that subscribed after a `Send` receives the event from
    `Subscribe`'s replay and again from the `FinishBuffering` flush: two
    deliveries.
  - In the kernel, `replayStartupEvents` (`kernel.go:280-288`) replays the buffer
    to a module *in addition to* `Subscribe`'s replay, so the same event can be
    counted twice before the flush, making three deliveries.
  Nothing removes an event from `buf` after delivery, and the flush does not check
  whether a listener already received it.

### B3. The buffer is never disabled for the lifetime of a running kernel

- **Location:** `kernel.go:206-207`, `bus.go:207-209`.
- **What happens:** `Kernel.Start` calls `k.bus.StartBuffering()` and disables
  buffering only via `defer k.bus.FinishBuffering()`, which runs when `Start`
  returns. `Start` blocks in `run` for the entire application lifetime, so
  `buffering` stays `true` and every event the application ever sends is appended
  to `buf` without bound. Nothing trims or caps the slice. At shutdown, the
  deferred `FinishBuffering` then spawns one goroutine per buffered event (see B6).

### B4. Startup replay clones the whole buffer per subscription and drops events

- **Location:** `kernel.go:280-288`, `bus.go:185-190`.
- **What happens:** `replayStartupEvents` calls `k.bus.Buffer()`, which clones the
  entire (ever-growing) buffer each time a module is subscribed. It then performs a
  non-blocking send per event with `default: return`; as soon as the module's
  channel is full, the remaining events are silently dropped. With the documented
  preference for unbuffered or cap-1 channels (`CONTRIBUTING.md:37`), this drops
  most or all of the replay.

### B5. `Subscribe`'s replay races with `Send`

- **Location:** `bus.go:99-116`, `bus.go:223-227`.
- **What happens:** The listener is inserted into `listeners` before the buffer
  snapshot and replay. A concurrent `Send` can snapshot the listener (so it is
  eligible for live delivery) and then `Subscribe` replays the same buffered event
  to it. The relative order of the two deliveries is undefined, and the event can
  arrive twice. There is also a gap on the other side: `FinishBuffering` sets
  `buffering = false` and clears `buf` under `bufMu` (`bus.go:150-153`) and only
  then snapshots listeners under `mu` (`bus.go:155-160`). A `Subscribe` that lands
  in between skips replay (buffering is already false) and may or may not be in the
  snapshot, so the flushed events can be lost for it.

### B6. `FinishBuffering` leaks goroutines via unbounded blocking sends

- **Location:** `bus.go:162-168`.
- **What happens:** One goroutine per buffered event runs
  `for _, ch := range chans { ch <- evt }` with no context, timeout, or
  cancellation. If any listener channel is full and never drained, the goroutine
  blocks forever. The only test that both registers a listener and calls
  `FinishBuffering` is `TestBus_Buffer_TOCTOURace` (`bus_test.go:213-254`);
  `cmd/debug/main.go` reproduces the same shape. (`TestBus_Buffer`,
  `bus_test.go:81-100`, has no subscriber, so its flush goroutines range over an
  empty slice and exit immediately — it does not demonstrate the leak.)

### B7. `ClearBuffer` contradicts its own documentation

- **Location:** `bus.go:192-197`.
- **What happens:** The doc comment says "clears all buffered events and disables
  buffering", but the method delegates to `FinishBuffering`, which *delivers* the
  buffered events to all current listeners instead of discarding them.

### B8. `SetBuffering(false)` strands buffered events

- **Location:** `bus.go:174-182`.
- **What happens:** It clears the `buffering` flag without clearing or flushing
  `buf`. The events stay in the slice and are no longer replayed to new subscribers
  (replay only happens while `buffering` is true). They remain reachable through
  `Buffer()` and can still be delivered by a later `FinishBuffering` (which does
  not check the `buffering` flag), but `SetBuffering(false)` on its own leaves them
  in an indeterminate state.

### B9. Delivery is sequential on one shared timeout

- **Location:** `bus.go:216-246`, `bus.go:248-254`.
- **What happens:** All listeners are served sequentially with a single
  context/timeout. One slow or unbuffered listener consumes the timeout; every
  later listener receives an already-expired context and is not delivered to.
  Iteration is over a `map`, so delivery order is nondeterministic.   `Send` returns
  an error whenever the context is expired at the end (`bus.go:241-243`), even if
  every listener actually received the event. Per listener, `sendEvent` is a
  `select` over `ctx.Done()` and `ch <- e` (`bus.go:248-254`); when both are ready,
  Go picks at random, so an event the listener would have accepted can still be
  recorded as a failure. The doc comment `// WithSendTimeout sets the per-listener
  send timeout.` (`bus.go:50`) also describes a per-listener timeout that does not
  exist in the implementation.

### B10. Nil-safety is claimed but mostly absent, and the zero value is unusable

- **Location:** `bus.go:90, 122-125, 136-141, 145-169, 174-197, 200-203`.
- **What happens:** Only `Send` and `Unsubscribe` check `b == nil` (`bus.go:123`,
  `bus.go:201`). `Subscribe`, `StartBuffering`, `FinishBuffering`, `SetBuffering`,
  `Buffer`, and `ClearBuffer` dereference the receiver and panic on a nil `*Bus`,
  contradicting the claim in `CONTRIBUTING.md:38` that the bus is nil-safe. The
  zero value `popcorn.Bus{}` is also unusable: `Subscribe` assigns into a nil map
  (`bus.go:104`), and `Send` reaches `b.flushCond.Broadcast()` with a nil
  `*sync.Cond` (`bus.go:211-213`). By contrast `ModuleStateStore` is deliberately
  zero-value-safe, so the inconsistency is surprising. The zero-value `Kernel` is
  likewise unusable: `k.bus`, `k.state`, and `k.stopFuncs` are nil
  (`kernel.go:79-91`), so it only works when built by `NewKernel`.

### B11. Invalid options are accepted and fail later

- **Location:** `bus.go:51-64`, `kernel.go:113-118`.
- **What happens:** `WithSendTimeout(0)` or a negative duration makes every send
  fail immediately. `WithBusLogger(nil)` / `WithLogger(nil)` store a nil
  `*slog.Logger` in the `plog.Logger` field; later `LogAttrs` calls dereference it
  and panic. `WithBus(nil)` stores a nil bus, and `NewKernel` then silently creates
  a default one (`kernel.go:105-110`, `kernel.go:159-166`), so a caller cannot
  distinguish "no bus provided" from "an explicit nil was passed".

### B12. A nil context panics across the public context-accepting API

- **Location:** `bus.go:216`, `kernel.go:211`, `kernel.go:440`, `kernel.go:469-471`,
  `kernel.go:475-477`.
- **What happens:** `Send`, `Kernel.Start`, `Kernel.stop`, `RetainContext`, and
  `RetainContextCause` all pass a context to `context.WithoutCancel`, which panics
  on a nil parent (`cannot create context from nil parent`). `RetainContext(nil)`
  and `RetainContextCause(nil)` are exported entry points, so the panic is
  reachable directly. There are no nil-context guards.

### B13. `EventLogValue` truncates the timestamp on 32-bit platforms

- **Location:** `events.go:55-59`.
- **What happens:** `strconv.Itoa(int(e.At.UnixNano()))` converts a nanosecond
  epoch (~1.7e18) to `int`. On 32-bit builds `int` is 32 bits and the value
  overflows, producing a wrong timestamp. The log value also omits `Payload`.

## Events (`events.go`)

### B14. `eventKind` yields empty/colliding kinds and panics for interface types

- **Location:** `events.go:36-39`.
- **What happens:** `eventKind` uses `reflect.TypeOf(t).Name()`:
  - It returns `""` for pointer, slice, map, function, and unnamed types, so
    distinct event payloads collapse onto `Kind == ""`.
  - It omits the package path, so same-named types from different packages
    collide.
  - When `T` is an interface type such as `any`, `var t T` is a nil interface,
    `reflect.TypeOf(t)` returns nil, and `.Name()` panics.
  A caller using `BaseEvent[*Foo]` gets `Kind == ""`; a caller using
  `BaseEvent[any]` panics.

## Kernel (`kernel.go`)

### B15. Data race on `nokModuleID`

- **Location:** `kernel.go:461-466` (write in `handleEvent`), `kernel.go:399-402`
  (read in `run`).
- **What happens:** `handleEvent` runs on the listener goroutine and assigns
  `k.nokModuleID`; `run` reads it on the `Start` goroutine when building
  `KernelUnhealthyError`. `go test -race ./...` reports the race and fails
  `TestKernel_ModuleNOKStopsKernel` and `Test_DependentModules`.

### B16. `Start` leaks the listener and skips `StopFunc`s on early error

- **Location:** `kernel.go:202-227`, `kernel.go:439-459`.
- **What happens:** The listener goroutine runs on
  `context.WithoutCancel(ctx)` and is only stopped by `k.cancelListener()`, which
  is called exclusively from `stop` (`kernel.go:454-456`). If the kernel
  subscription fails (`kernel.go:217-219`) or `startModules` returns an error
  (  `kernel.go:222-224`), `Start` returns without calling `stop`. The listener
  goroutine then blocks forever, and any modules already started never have their
  `StopFunc` invoked. Additionally, when `m.Start` returns both a `StopFunc` and a
  non-nil error, `startModules` discards the `StopFunc` (`kernel.go:249-252`); the
  module contract (`CONTRIBUTING.md:23-24`) does not say whether a `StopFunc` is
  honored when `Start` fails.

### B17. `watchTasks` cannot be stopped and leaks on a nil `Done()`

- **Location:** `kernel.go:407-437`.
- **What happens:** One goroutine per `TaskModule` waits on `tm.Done()` with no
  context or cancellation; there is no shutdown path. A `TaskModule` whose `Done()`
  returns a nil channel blocks forever, so the `WaitGroup` never completes and the
  returned `done` channel is never closed.

### B18. The shutdown timeout is shared across all modules

- **Location:** `kernel.go:439-459`.
- **What happens:** A single `stopCtx` with `k.stopTimeout` is reused for every
  `StopFunc` in a sequential loop. A slow module consumes the entire budget, and
  later modules receive an already-expired context and cannot stop cleanly.

### B19. Health only reacts to `NOK`, is passive, and can be spoofed

- **Location:** `kernel.go:399-402`, `kernel.go:461-466`.
- **What happens:** Kernel health changes only when a module emits a
  `ModuleStatusChanged` event with `To == ModuleStateNOK`; `ModuleStateTempNOK` is
  never produced or consumed by the kernel, and `ModuleStateOK` is likewise never
  produced or consumed by the kernel: the kernel state starts at
  `ModuleStateUnknown` and is only ever moved to `NOK` (`kernel.go:461-466`). `handleEvent` does not check `e.Source`, does not
  check `msc.ID` against registered modules, and does not check that the event came
  from the module it names. Any code with bus access can emit
  `ModuleStatusChanged{ID: X, To: ModuleStateNOK}` and cause the kernel to shut
  down while attributing it to module `X` (or to `"kernel"`). The
  `ModuleStatusChanged.From` and `.Cause` fields (`events.go:70-74`) are never read;
  only `To` matters, so a transition is not validated against the previous state.
  There is a single
  global state for the whole kernel, and the offending id is stored outside the
  atomic state store (see B15).

### B20. `NewKernel` accepts empty IDs and typed-nil modules

- **Location:** `kernel.go:168-179`.
- **What happens:** The nil check `m == nil` passes for a typed-nil module (a
  non-nil interface holding a nil pointer); the panic happens in the same loop, on
  `m.ID()` inside the duplicate check (`kernel.go:174`). Empty module IDs are not
  rejected for arbitrary `Module` implementations (only `NewModule` rejects them,
  and custom modules need not use `NewModule`).

### B21. An invalid health tick panics

- **Location:** `kernel.go:129-134`, `kernel.go:387`.
- **What happens:** `WithHealthTick(0)` or a negative duration flows into
  `time.NewTicker` inside `run`, which panics rather than returning an error.

### B22. A kernel cannot be restarted and module subscriptions are never removed

- **Location:** `kernel.go:254-266`, `kernel.go:220`, `kernel.go:439-459`.
- **What happens:** `startModules` subscribes every module under its ID
  (`kernel.go:257-266`), but shutdown only unsubscribes the kernel itself
  (`kernel.go:220`); no module listener is ever removed. A second `Start` on the
  same kernel fails when `bus.Subscribe(id, ch)` returns `ErrDuplicateID`, which
  then triggers the B16 early-error path. `k.started` and `k.stopFuncs` also
  accumulate across calls (`kernel.go:254-255`).

### B23. A timed-out `ModuleStarted` send aborts startup, and pointer payloads are ignored

- **Location:** `kernel.go:268-274`, `kernel.go:461-466`, `events.go:36-39`.
- **What happens:** `startModules` returns an error if `Send(ModuleStarted)` times
  out. Because module event channels are typically tiny (cap 1) and the replay is
  non-blocking (B4), a full channel is reachable and turns a cosmetic delivery
  timeout into a startup abort that then hits the B16 leak path. Separately,
  `handleEvent` asserts the value type `ModuleStatusChanged`; a
  `*ModuleStatusChanged` payload is silently ignored (and `eventKind` gives it
  `Kind == ""` per B14).

## Module / state

### B24. `ErrModuleIDReserved` can be bypassed

- **Location:** `module.go:90-92`, `kernel.go:168-179`.
- **What happens:** `NewModule` rejects `EventSourceKernel`, but a custom `Module`
  implementation (not built via `NewModule`) can return `"kernel"` from `ID()` and
  pass kernel registration, then collide with the kernel's own bus subscription.
  More generally, the validation performed by `NewModule` is not enforced for
  implementations of the `Module` interface.

## Examples

### B25. `HTTPServerListens` is never emitted, so the pinger pings nothing

- **Location:** `examples/main.go:92-96` (type defined), `examples/main.go:159-186`
  (consumed in `waitForAddresses`).
- **What happens:** Nothing in the repository ever constructs or sends an
  `HTTPServerListens` event. `waitForAddresses` therefore never records an address
  and returns early on the pinger's own `ModuleStarted`, so `pingAll` iterates an
  empty slice and the example performs no pings.   `examples/main_test.go` only
  asserts constructors and recipe fields; it never calls `Start`, so this path is
  not exercised by the tests. Separately, the only address the HTTP module could
  expose is `listener.Addr().String()` (`"host:port"`, `examples/main.go:59`) stored
  in `HTTPServerListens.URL`, and `ping` passes that string directly to
  `http.NewRequestWithContext` (`examples/main.go:207`); `"host:port"` is not a
  valid URL (it has no scheme), so a ping would fail even once the event is wired
  up.

### B26. `HTTPModule`'s stop function shuts the server down with the start context

- **Location:** `examples/main.go:52`, `examples/main.go:77-79`.
- **What happens:** The `Start` context is captured by the closure, and the
  returned `StopFunc` ignores its own parameter (`func(_ context.Context) error`)
  and calls `srv.Shutdown(ctx)` with the original startup context instead of the
  fresh shutdown context. This contradicts `CONTRIBUTING.md:24`. In the shipped
  example the start context happens to still be live (30s timeout, ~5s of pings),
  so the defect is latent there rather than immediately visible. Relatedly,
  `pingAll` waits with `time.Sleep(time.Second)` (`examples/main.go:202`), which
  does not observe cancellation and can delay shutdown by up to a second.

### B27. `PingerModule.bus` is injected but never used

- **Location:** `examples/main.go:103`, `examples/main.go:110-121`.
- **What happens:** The `bus` parameter is stored on the struct but never read
  anywhere; the field is dead. `NewPingerModule` accepts a nil bus, masking the
  fact that it is unnecessary. In addition, `ModRecipe` allocates a fresh `evts`
  channel on each call (`examples/main.go:125`), and `Start` closes `m.done` on
  every invocation (`examples/main.go:135`), so two `ModRecipe()` calls produce
  recipes bound to different event channels and a second `Start` panics on the
  closed `done` channel.

## Tests

### B28. `e2e_test.go` has a broken failure branch and unsynchronized shared state

- **Location:** `e2e_test.go:99-104`, `e2e_test.go:165-192`.
- **What happens:** The pattern
  `if !errors.As(err, &unhealthy) { is.NoErr(err) }` does not stop execution;
  when `err == nil` the zero-value `unhealthy` (with a nil `Cause`) is used on the
  next line, formatting as `%!s(<nil>)`. Separately, `eventStore.evts` is appended
  from the store goroutine (`e2e_test.go:183`) and read from the test goroutine
  (`e2e_test.go:106-116`) with no synchronization; nothing orders the append after
  `kernel.Start` returns.

### B29. The `eventStore` callback blocks its own channel until timeout

- **Location:** `e2e_test.go:46-56`, `e2e_test.go:177-189`.
- **What happens:** The callback runs on the single goroutine that reads the
  unbuffered `aChan`, and it synchronously calls `bus.Send`, which tries to deliver
  to that same channel. Delivery blocks until the shared per-`Send` timeout. This is
  visible in test output as repeated
  `failed to send event mod=a err="send failed: context deadline exceeded"`. The
  callback runs on a non-test goroutine (`e2e_test.go:177-189`) and can reach
  `t.Fatal` through the `is` helpers, which is not permitted from a non-test
  goroutine; and `NoCtxErr` (`e2e_test.go:203-213`) explicitly ignores
  `context.Canceled` and `context.DeadlineExceeded`, which is why these warnings
  never fail the test.

### B30. Tests encode the duplicate-delivery semantics

- **Location:** `bus_test.go:190-211`, `bus_test.go:120-151`.
- **What happens:** `TestBus_Buffer_DeliversAndBuffersSimultaneously` asserts both
  that the subscriber receives the event live *and* that the buffer still contains
  it (`bus_test.go:203-210`), and `TestBus_Buffer_ReplayOnSubscribe` relies on the
  buffer being replayed without being consumed. These tests assert the current
  ambiguous "deliver and retain" behaviour described in B2, so they would need to
  change if that behaviour changes — the suite documents the ambiguity rather than
  catching it.

## Tooling & repository

### B31. The advertised race and lint gates fail

- **Location:** `Taskfile.yaml:13-25`, `kernel.go:207`, `CONTRIBUTING.md:70,81`.
- **What happens:**
  - `task test:race` runs `go test -count 1 -race -coverpkg=./... ./...` and fails
    on B15.
  - `.tools/golangci-lint run ./...` (v2.12.2, the pinned tool) reports **7**
    issues: `forbidigo` ×2, `modernize` ×2, `nolintlint` ×1, `revive` ×2.
  - `nolintlint` reports `kernel.go:207:32: directive //nolint:contextcheck is
    unused for linter "contextcheck" (nolintlint)`.
  - `task test` (`Taskfile.yaml:13-17`) does not pass `-race`, while
    `CONTRIBUTING.md:70` says it does.
  - `CONTRIBUTING.md:81` references `task fmt`, which is not defined in
    `Taskfile.yaml`.
  - The pinned toolchain disagrees with itself: `devbox.json:8-9` pins Go 1.27.0
    and golangci-lint 2.13.1, `go.mod:3` declares `go 1.26.3`, `Taskfile.yaml:9`
    sets `GOTOOLCHAIN: go1.26.3+auto`, and `tools/go.mod` pins golangci-lint
    v2.12.2. The devbox files are untracked, so a fresh clone loses the devbox pin
    (though `tools/go.mod` and `GOTOOLCHAIN` still pin the lint tools and Go
    toolchain).
  - `Taskfile.yaml:41-44` defines a task named `deps` while the same file also uses
    `deps:` as a task keyword, and `Taskfile.yaml:14` describes "core packages"
    while `:17` runs `./...`.
  - `.envrc:3` adds the non-existent `tools/bin` to `PATH` (tools install into
    `.tools/`), while `devbox.json:12` adds `bin`.
  - The repository has no `LICENSE` and no `.github/` CI workflow; the only gates
    are local `task` targets.

### B32. `internal.RandomID` is dead code with a misleading comment

- **Location:** `internal/rand.go:11-19`.
- **What happens:** Nothing outside the package's own test calls `RandomID`. The
  comment says `crypto/rand` failures "fall back to zeros", but the code returns
  `fmt.Sprintf("rand-fail-%v", err)` instead.

### B33. Untracked scratch files, stale tracked config, and review annotations

- **Location:** `cmd/debug/main.go`, `BUG.md`, `devbox.json`, `devbox.lock`,
  `review/`, `.golangci.bck.yml`, the tracked symlinks `task` and `.droot.envrc`,
  plus `bus.go:17`, `kernel.go:203, 209, 262, 304, 335, 396`,
  `internal/rand.go:15`, `e2e_test.go:54`, `examples/main_test.go:3`,
  `CONTRIBUTING.md:20,32,71,79`.
- **What happens:**
  - `git status` shows `cmd/`, `BUG.md`, `devbox.json`, `devbox.lock`, and
    `review/` as untracked, so the toolchain pinning is not committed.
    `cmd/debug/main.go` is a scratch reproduction of `TestBus_Buffer_TOCTOURace`
    (it also shadows the builtin `cap` at `cmd/debug/main.go:15`, uses
    `fmt.Printf` at `:54`/`:59`, and uses manual `WaitGroup` calls at `:32`/`:40`).
  - `.golangci.bck.yml` is tracked and contains inline review annotations
    (`:8,30,50,55`) and v1-style keys (`output.formats`,
    `issues.exclude-use-default`, `linters-settings`) that are invalid under the
    checked-in v2 config.
  - The tracked symlinks `task -> ../droot/task` and
    `.droot.envrc -> ../droot/.envrc` (git mode `120000`) point outside the
    repository and dangle on clone; `.envrc` only sources `.envrc.local`, so
    `.droot.envrc` is orphaned. `README.md` has wording errors too.
  - Several source and documentation files contain inline review annotations
    ("CR", "FIXME"), some profane, which ship as part of the package's comments and
    contributor guide.
  - `AGENTS.md` and `CLAUDE.md` are byte-identical GitNexus blocks
    (`AGENTS.md:1-43`, `CLAUDE.md:1-43`) containing hardcoded index statistics
    (`AGENTS.md:4`).
  - The module path contains an uppercase `B` (`go.mod:1`, repeated at
    `examples/main.go:45,112`), which is non-idiomatic and triggers case-escaping
    in the Go module proxy.

## Additional findings

### B34. `Module.Dependencies()` exposes the internal slice

- **Location:** `module.go:71-72`, `module.go:98-103`.
- **What happens:** `module.Dependencies()` returns `m.dependencies` directly
  (`module.go:72`), and `NewModule` stores `recipe.Dependencies` by reference
  (`module.go:100`). A caller that keeps the recipe or the returned slice can
  mutate a module's dependency list after construction, changing what the kernel
  (or a later dependency-graph computation) sees. This contradicts the defensive
  copying used elsewhere (`bus.go:189` clones the buffer, `bus.go:224-227` copies
  the listener map).

### B35. Closing a listener channel panics the sender

- **Location:** `bus.go:165`, `bus.go:252`, `bus.go:122-131`, `module.go:38-42`.
- **What happens:** Sends are plain `ch <- e` / `ch <- evt`. Nothing documents that
  a listener must not close its channel, and `EventReceiver.Events()` returns a
  bidirectional `chan Event`, so a module can close it. If a listener closes while
  the bus still holds it, or `Unsubscribe` races with an in-flight delivery
  snapshot, the send panics the sending goroutine. `Unsubscribe` removes the map
  entry but does not coordinate with deliveries already in progress. The bus also
  does not detect the same channel registered under two ids (`bus.go:104`,
  `bus.go:235`), so such a subscriber receives every event twice.

### B36. Test coverage gaps

- **Location:** `bus_test.go`, `kernel_test.go`, `module_test.go`,
  `internal/rand_test.go`, `events.go`.
- **What happens:** There are no tests for the nil-context panics (B12), invalid
  `WithSendTimeout`/`WithHealthTick` values (B11, B21), nil-safety of bus methods
  other than `Send`/`Unsubscribe` (B10), kernel restart (B22), spoofed or unknown
  `ModuleStatusChanged` (B19), listener/goroutine leaks (B6, B16, B17),
  dependency-slice mutation (B34), `Event.LogValue`/`EventLogValue` (B13), or
  closing a listener channel (B35). `internal/rand_test.go:20-25` does not verify
  the length its name implies.

### B37. Duplicate dependency IDs are accepted and double-counted

- **Location:** `kernel.go:320-332`, `kernel.go:334-347`, `module.go:56`.
- **What happens:** `validateDeps` checks unknown and self dependencies only.
  Listing the same dependency twice is accepted; `buildDegree` counts it twice in
  `inDegree` and appends the dependent twice to `dependents`
  (`kernel.go:340-342`), and `Dependencies()` returns the duplicated slice. The
  sort still converges, but the recorded in-degree and the public
  `Dependencies()` result misreport the graph.
