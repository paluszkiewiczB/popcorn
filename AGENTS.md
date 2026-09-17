# AGENTS.md

## Verify every change with `task ci`

Run this after any code or docs change, before reporting done:

```sh
task ci
```

It must pass. Do not report success on a failing `task ci`.

`task ci` runs the full quality gate in order:

1. `gofumpt` formatting check
2. `go mod tidy -diff`
3. `golangci-lint`
4. `go test` with a coverage profile
5. `covercheck` — the coverage floor

## Coverage floor

`REQUIRED_CODE_COVERAGE` in `Taskfile.yaml` is **100**. `task ci` fails if total
statement coverage drops below it. Never lower the threshold to make the gate
pass: add tests instead.

## When the gate fails

- Formatting: `task fmt`
- Lint: `task fix`
- Coverage: add tests for the uncovered statements

## Notes

- Use `.tools/` binaries via the tasks; do not assume global tool installs.
- `task test:race` uses `-race` and needs cgo. It is not part of `ci`.
- Usage docs live in the package godoc (`doc.go`) and runnable examples in
  `example_test.go`; contributor conventions are in `CONTRIBUTING.md`.
