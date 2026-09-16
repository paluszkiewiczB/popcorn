# Contributing to Popcorn

Conventions for contributing to the Popcorn Go microframework. It is intentionally
focused on a small library with a tiny public API surface.

Public usage is documented in the package documentation (`go doc github.com/paluszkiewiczB/popcorn`
or [pkg.go.dev](https://pkg.go.dev/github.com/paluszkiewiczB/popcorn)) and in the
runnable examples in `example_test.go`. Do not duplicate usage guidance here; this
document is for people changing the code.

## Architecture

Popcorn is a microkernel: the core owns bootstrap, dependency ordering, health
watching, and graceful shutdown, and knows nothing about what a module does.

The invariants below are load-bearing. A change that breaks one is a design change,
not a refactor.

- **One owner per channel.** The Bus creates, writes, and closes subscription
  channels; modules only read them. No module writes to a channel it does not own,
  and no module closes a Bus-owned channel.
- **One communication mechanism.** The event Bus carries module→kernel,
  module→module, and kernel→module traffic. Health is just an event; there is no
  side channel.
- **Explicit subscriptions.** A module subscribes itself, when it is ready, through
  the Bus it holds. There are no receiver interfaces and no kernel runtime checks.
- **Identity is attached.** A Publisher is bound to an id, so `Event.Source` is set
  by the Bus and `Send` skips the sender's own subscription. The kernel trusts health
  only from ids it has registered.
- **Bounded everything.** Each subscription has a bounded buffer and the Bus keeps a
  bounded replay history. A slow subscriber can only overflow its own buffer.
- **Health is best-effort.** It travels on the shared Bus like any other event.
- **Events are lightweight values.** No pointers, no per-event wrappers.

## Public API design

- **Tiny public API surface.** Export only what a consumer of the framework needs.
  Implementation details are unexported.
- **Open-closed principle.** The core is closed for modification; extension happens
  through the `Module` interface and event payloads.
- **Consumer-side interfaces.** Define interfaces where they are consumed, not where
  they are implemented.
- **Accept interfaces, return concrete types.** Default to concrete return values;
  let callers wrap in interfaces if needed.
- **No globals.** Dependencies are injected through constructors or functional
  options.
- **Explicit constructors.** Constructors panic only on invalid configuration that
  must be caught at program startup; otherwise return errors.
- **One package per concern.** Keep the core package small.
- **Avoid `any`.** Use generics or specific types. `Event.Payload` is `any` by
  design, and consumers assert it safely.

## Implementation notes

- **Dependency order.** A module starts once every dependency's `Start` has returned.
  Stop order is descending dependency depth: every dependent is stopped before
  anything it depends on.
- **Shutdown budget.** Each `StopFunc` receives a context carved out of the shared
  budget and runs in its own goroutine, so one that ignores its context cannot hang
  shutdown; it is abandoned at the deadline.
- **Lifecycle events under lock.** The kernel publishes `ModuleStarted` and
  `KernelStateChanged` while holding its state lock, so delivered order matches
  lifecycle order. Event filters must be pure and non-blocking: a filter that blocks
  or calls back into the Bus or Kernel can deadlock.
- **Replay seeding is atomic.** `Subscribe` seeds from history and registers for live
  delivery under the Bus lock, so a subscriber sees no gap and no duplicate.
- **Single-shot kernel.** `Start` consumes the kernel even if setup fails; a second
  call returns `ErrKernelStarted`.

## Go style

- Follow [Effective Go](https://go.dev/doc/effective_go) and the
  [Google Go Style Guide](https://google.github.io/styleguide/go/).
- Naming:
  - `ID`, `URL`, `HTTP`, `DB` (not `Id`, `Url`, `Http`, `Db`).
  - `ErrFoo` for exported error variables, `FooError` for error types.
  - No `Get` prefix on getters.
  - Receiver names are 1–2 letter abbreviations of the type (`c` for `Client`, `k`
    for `Kernel`).
- Error handling:
  - Wrap with context: `fmt.Errorf("doing thing: %w", err)`.
  - Use `errors.Is` for sentinels and `errors.AsType` for error types; never match on
    `err.Error()`.
  - Error strings are lowercase, no trailing punctuation.
- `defer` for cleanup. Do not put `defer` inside loops.
- `panic` only for unrecoverable invariant violations.
- `any` over `interface{}` in new code.
- Use `gofumpt` for formatting.

## Concurrency

- Use `context.Context` for cancellation. Pass contexts explicitly; never store them
  in structs.
- `ctx` is always the first parameter.
- Use `chan struct{}` for pure signaling. The Bus uses bounded channels (buffer
  size = backlog, 0 by default); do not "simplify" them away.
- Use `sync.Mutex` for shared state, channels for coordination and ownership
  transfer.
- `wg.Add(n)` (or `wg.Go`) before launching goroutines.
- Do not propagate request-scoped contexts to background goroutines. The kernel uses
  `context.WithoutCancel` where it must publish during shutdown.

## Testing

- Table-driven tests with `map[string]struct{}`.
- Tests alongside code (`kernel.go` → `kernel_test.go`).
- Run `task test`; run `task test:race` for the race detector (requires cgo).
- Use `testing/synctest` for time-dependent tests; avoid `time.Sleep`.
- Use real transports and test doubles over mocks.
- Aim for ≥80% coverage.

## Tooling workflow

- Use `task` for standard workflows (`task test`, `task test:race`, `task lint`,
  `task fix`).
- Tools are managed in `tools/go.mod` and installed into `.tools/`.
- Use `gopls` for navigation, diagnostics, and refactoring:
  - `gopls check <file>` before editing.
  - `gopls references <pos>` for impact analysis.
  - `gopls rename -w <pos> <NewName>` for renames.
  - `gopls format -w <file>` and `gopls imports -w <file>` after edits.
- Run `golangci-lint` via `task lint`.
- Run `go fix ./...` after toolchain upgrades.
- Reindex GitNexus with `npx gitnexus analyze` after significant structural changes.

## Sources

- [Effective Go](https://go.dev/doc/effective_go)
- [Google Go Style Guide](https://google.github.io/styleguide/go/)
- [Google Go Style Decisions](https://google.github.io/styleguide/go/decisions)
- [100 Go Mistakes and How to Avoid Them](https://100go.co/)
- [Don't just check errors, handle them gracefully](https://dave.cheney.net/2016/04/27/dont-just-check-errors-handle-them-gracefully)
