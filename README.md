# popcorn

Popcorn is a microframework for building modular Go applications following the
microkernel architecture. The core knows nothing about what your application
does: it handles bootstrap, dependency-ordered startup, health watching, and
graceful shutdown, while modules hold the functionality and communicate over an
in-process event bus.

## Install

```sh
go get github.com/paluszkiewiczB/popcorn
```

## Documentation

- API reference and runnable examples:
  [pkg.go.dev/github.com/paluszkiewiczB/popcorn](https://pkg.go.dev/github.com/paluszkiewiczB/popcorn)
- Architecture and contribution notes: [CONTRIBUTING.md](./CONTRIBUTING.md)

Popcorn has no runtime dependencies beyond the standard library.
