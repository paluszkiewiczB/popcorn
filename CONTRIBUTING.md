# Contributing to Popcorn

This document defines the conventions for the Popcorn Go microframework. It is intentionally focused on a small library with a tiny public API surface.

For code-intelligence rules, see [AGENTS.md](./AGENTS.md).

## Architecture & public API design

- **Tiny public API surface.** Only export what a consumer of the framework absolutely needs. Implementation details live in `internal/` or are unexported.
- **Open-closed principle.** The core is closed for modification; extension happens through the `Module` interface and generic event payloads.
- **Consumer-side interfaces.** Define interfaces where they are consumed, not where they are implemented.
- **Accept interfaces, return concrete types.** Default to concrete return values; let callers wrap in interfaces if needed.
- **No globals.** All dependencies are injected through constructors or functional options.
- **Explicit constructors.** Constructors panic only on invalid configuration that must be caught at program startup; otherwise return errors.
- **One package per concern.** Keep the core package small; `plog` and `plog/attr` are narrow helpers.
- **Avoid `any`.** Use generics or specific types. `Event.Payload` is `any` by design, but assert it safely.

## Module lifecycle

-- CR: those appear to be guidelines on how to use the framework, not how to fucking contribute to its fucking code?

- A `Module` is a self-contained unit that knows how to start and stop itself.
- `Start` receives a `context.Context`. It returns a `StopFunc` and an error.
- `Stop` receives a fresh context for shutdown, not the original start context.
- Do not retain the start context. Use `popcorn.RetainContext` or `popcorn.RetainContextCause` if values must outlive it.
- Never start a goroutine without knowing how it will stop.
- For finite work (CLI-style), implement `TaskModule` and close `Done()` when the work is finished.
- For long-running work (HTTP servers), return a `StopFunc` that blocks until clean shutdown.

## Event bus

-- CR: isn't all the info already in fucking `go doc` ?

- `Event.Kind` must be globally unique. Use `popcorn.NewEvent[T]` so the kind is derived from the payload type.
- `Event.Source` is `popcorn.EventSourceKernel` for kernel events, otherwise the module ID.
- Listeners receive all events and filter locally.
- Prefer unbuffered channels. If a buffer is needed, cap it at 1.
- The bus is nil-safe: `Send` on a nil bus returns `nil`.

## Go style

- Follow [Effective Go](https://go.dev/doc/effective_go) and the [Google Go Style Guide](https://google.github.io/styleguide/go/).
- Naming:
  - `ID`, `URL`, `HTTP`, `DB` (not `Id`, `Url`, `Http`, `Db`).
  - `ErrFoo` for exported error variables, `FooError` for error types.
  - No `Get` prefix on getters.
  - Receiver names are 1–2 letter abbreviations of the type (`c` for `Client`, `k` for `Kernel`).
- Error handling:
  - Wrap with context: `fmt.Errorf("doing thing: %w", err)`.
  - Use `errors.Is` and `errors.As`, never `err.Error()` string matching.
  - Error strings are lowercase, no trailing punctuation.
- `defer` for cleanup. Do not put `defer` inside loops.
- `panic` only for unrecoverable invariant violations.
- `any` over `interface{}` in new code.
- Use `gofumpt` for formatting.

## Concurrency

- Use `context.Context` for cancellation. Pass contexts explicitly; never store them in structs.
- `ctx` is always the first parameter.
- Channels are unbuffered or size 1. Use `chan struct{}` for pure signaling.
- Use `sync.Mutex` for shared state, channels for coordination and ownership transfer.
- `wg.Add(n)` before launching goroutines.
- Do not propagate request-scoped contexts to background goroutines.

## Testing

- Table-driven tests with `map[string]struct{}`.
- Tests alongside code (`kernel.go` → `kernel_test.go`).
- Always run `task test` which includes the race detector when cgo is available.
  -- CR: what fucking business logic? IT'S A FRAMEWORK!
- Inject the time source; do not rely on `time.Now()` in business logic.
- Avoid `time.Sleep` in tests. Use channels, wait groups, or synchronization.
- Use real transports and test doubles over mocks.
- Aim for ≥80% coverage.

## Tooling workflow

-- CR: isn't it AI agent instructions? which should go to AGENTS.md?

- Use `task` for all standard workflows (`task test`, `task lint`, `task fmt`).
- Tools are managed in `tools/go.mod` and installed into `.tools/`.
- Use `gopls` for navigation, diagnostics, and refactoring:
  - `gopls check <file>` before editing.
  - `gopls references <pos>` for impact analysis.
  - `gopls rename -w <pos> <NewName>` for renames.
  - `gopls format -w <file>` and `gopls imports -w <file>` after edits.
- Run `golangci-lint` via `task lint`.
- Reindex GitNexus with `npx gitnexus analyze` after significant structural changes.

## Sources

- [Effective Go](https://go.dev/doc/effective_go)
- [Google Go Style Guide](https://google.github.io/styleguide/go/)
- [Google Go Style Decisions](https://google.github.io/styleguide/go/decisions)
- [100 Go Mistakes and How to Avoid Them](https://100go.co/)
- [Don't just check errors, handle them gracefully](https://dave.cheney.net/2016/04/27/dont-just-check-errors-handle-them-gracefully)
