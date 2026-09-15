<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **popcorn** (293 symbols, 951 relationships, 0 execution flows). Use the GitNexus MCP tools to understand code, assess impact, and navigate safely.

> If any GitNexus tool warns the index is stale, run `npx gitnexus analyze` in terminal first.

## Always Do

- **MUST run impact analysis before editing any symbol.** Before modifying a function, class, or method, run `gitnexus_impact({target: "symbolName", direction: "upstream"})` and report the blast radius (direct callers, affected processes, risk level) to the user.
- **MUST run `gitnexus_detect_changes()` before committing** to verify your changes only affect expected symbols and execution flows.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding with edits.
- When exploring unfamiliar code, use `gitnexus_query({query: "concept"})` to find execution flows instead of grepping. It returns process-grouped results ranked by relevance.
- When you need full context on a specific symbol — callers, callees, which execution flows it participates in — use `gitnexus_context({name: "symbolName"})`.

## Never Do

- NEVER edit a function, class, or method without first running `gitnexus_impact` on it.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis.
- NEVER rename symbols with find-and-replace — use `gitnexus_rename` which understands the call graph.
- NEVER commit changes without running `gitnexus_detect_changes()` to check affected scope.

## Resources

| Resource | Use for |
|----------|---------|
| `gitnexus://repo/popcorn/context` | Codebase overview, check index freshness |
| `gitnexus://repo/popcorn/clusters` | All functional areas |
| `gitnexus://repo/popcorn/processes` | All execution flows |
| `gitnexus://repo/popcorn/process/{name}` | Step-by-step execution trace |

## CLI

| Task | Read this skill file |
|------|---------------------|
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->

# Testing conventions

The suite lives in a black-box `popcorn_test` package using
[matryer/is](https://github.com/matryer/is) as the assertion library.

## Why the tests are "bloated" with trailing comments

Nearly every `is.*` call carries a trailing comment:

```go
is.True(err != nil) // Subscribe must reject an empty id
```

**This is not documentation.** It duplicates the assertion intentionally:
`is` uses the trailing comment at the call site as the **assertion failure
message** — on failure it prints the asserted expression *and* that comment
together with file:line.

```text
^{- this assertion failed. Perhaps you meant: ...}
...Subscribe must reject an empty id; stack: Subscribe("");
[is] give some context instead of logging purely around line-numbers
```

Consequently:
- Treat that comment as part of the assertion itself — it must state the
  *contract being enforced*, keep it short, imperative, and written so it
  reads sanely when pasted into a test-failure log.
- Never delete or "clean up" these comments as redundant. (they are messages,
  not prose).
- Use the same style when adding is-level assertions. Comments that explain
  deeper design rationale sit on preceding lines instead.

Do not rewrite tests to standard `t.Fatalf`/stdtesting idioms; the contract
suite relies on `is` intentionally (see `contract_test.go` for the kit and
rules of engagement).
