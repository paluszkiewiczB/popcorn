---
name: gopls
description: Use gopls CLI for all Go symbol queries, diagnostics, and refactoring — never read files for structural information
---

# gopls for AI Agents

Use `gopls` CLI for **all** Go symbol exploration, diagnostics, and refactoring.
Never read a file to answer a question gopls can answer with a symbolic query.

## Position format

```
file.go:line:col        # 1-indexed line, byte column
file.go:#offset         # 0-indexed byte offset
file.go:10:5-10:12      # range span
file.go:#100-#200       # offset range
```

## Exploration (read-only)

| Command                          | Use case                                                         |
| -------------------------------- | ---------------------------------------------------------------- |
| `gopls workspace_symbol <query>` | Fuzzy search across all packages — find anything by name         |
| `gopls symbols <file>`           | File outline (name, kind, range) — get structure without reading |
| `gopls definition <pos>`         | Resolve any symbol to its declaration                            |
| `gopls references <pos>`         | All references across workspace — impact analysis                |
| `gopls implementation <pos>`     | Concrete types ←→ interfaces                                     |
| `gopls call_hierarchy <pos>`     | Incoming/outgoing call tree — trace execution flow               |
| `gopls signature <pos>`          | Function signature + docs                                        |
| `gopls highlight <pos>`          | All occurrences within file                                      |
| `gopls semtok <file>`            | Semantic token classifications                                   |
| `gopls folding_ranges <file>`    | Collapsible regions — understand block structure                 |
| `gopls links <file>`             | URLs in doc comments                                             |
| `gopls prepare_rename <pos>`     | Validate rename before executing                                 |

## Diagnostics

| Command                                                         | Use case                   |
| --------------------------------------------------------------- | -------------------------- |
| `gopls check <files...> [-severity hint\|info\|warning\|error]` | Find errors before editing |

## Editing (write to filesystem)

Flag behavior: no flag → stdout, `-w` → write back, `-d` → diff, `-l` → list filenames

| Command                                              | Use case                                    |
| ---------------------------------------------------- | ------------------------------------------- |
| `gopls rename -w <pos> <newname>`                    | Safe symbol/package rename across workspace |
| `gopls format -w <files...>`                         | Canonical formatting                        |
| `gopls imports -w <file>`                            | Add missing, remove unused, sort imports    |
| `gopls codeaction <file>[:range]`                    | List available code actions                 |
| `gopls codeaction -exec -kind <kind> <file>[:range]` | Execute a code action                       |

### Code action kinds

**Extract:** `refactor.extract.function`, `refactor.extract.method`, `refactor.extract.variable`,
`refactor.extract.variable-all`, `refactor.extract.constant`, `refactor.extract.toNewFile`

**Inline:** `refactor.inline.call`, `refactor.inline.variable`

**Rewrite:** `refactor.rewrite.fillStruct`, `refactor.rewrite.fillSwitch`,
`refactor.rewrite.invertIf`, `refactor.rewrite.changeQuote`, `refactor.rewrite.splitLines`,
`refactor.rewrite.joinLines`, `refactor.rewrite.moveParamLeft`,
`refactor.rewrite.moveParamRight`, `refactor.rewrite.removeUnusedParam`,
`refactor.rewrite.addTags`, `refactor.rewrite.removeTags`,
`refactor.rewrite.implementInterface`

**Quickfix:** `CreateUndeclared`, `StubMissingInterfaceMethods`, `StubMissingCalledFunction`,
`fillreturns`

**Test generation:** `source.addTest` — generates table-driven test for a function

## Code lenses

| Command                                                   | Use case              |
| --------------------------------------------------------- | --------------------- |
| `gopls codelens <file>[:line]`                            | List available lenses |
| `gopls codelens -exec <file>:<line> "run test"`           | Run test at position  |
| `gopls codelens -exec <file>:<line> "tidy"`               | Run `go mod tidy`     |
| `gopls codelens -exec <file>:<line> "upgrade dependency"` | Check/upgrade deps    |
| `gopls codelens -exec <file>:<line> "generate"`           | Run `go generate`     |

## Execute commands

`gopls execute <name> [json-args]`

Key commands: `gopls.add_import`, `gopls.add_test`, `gopls.change_signature`,
`gopls.add_dependency`, `gopls.upgrade_dependency`, `gopls.remove_dependency`,
`gopls.run_tests`, `gopls.tidy`, `gopls.vendor`, `gopls.diagnose_files`,
`gopls.vulncheck`, `gopls.package_symbols`, `gopls.list_known_packages`,
`gopls.list_imports`

## Workflow rules

### Before reading any file

1. `gopls workspace_symbol <query>` — find the symbol
2. `gopls symbols <file>` — get file outline without reading
3. `gopls definition <file>:#offset` — jump to precise location

### Before editing

4. `gopls check <file>` — find existing errors
5. `gopls references <pos>` — understand impact
6. `gopls call_hierarchy <pos>` — trace callers/callees

### When editing

7. `gopls rename -w <pos> <NewName>` — never hand-edit symbol names
8. `gopls codeaction -exec -kind <kind> <file>:#range` — use extract/inline/fill
9. `gopls imports -w <file>` — always clean imports after edits
10. `gopls format -w <file>` — always format after edits

### After editing

11. `gopls check <file>` — verify no new errors

## Templ integration

After editing `.templ` files:

1. `templ generate` — generate `*_templ.go` files
2. `gopls check *_templ.go` — validate generated code
3. `gopls references / symbols` on generated Go — explore via gopls
4. `templ fmt .` — format `.templ` files
5. `go build ./...` — compile check

## Limitations

* References don't cross build constraints (`foo_linux.go` won't find refs in `foo_windows.go`)
* Extracted/inlined code can lose comments — verify output
* `gopls` shells out to `go` — `go` must be on `PATH`
* Generated files with `DO NOT EDIT` header get no code actions
