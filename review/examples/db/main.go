// Command db shows a database module: it owns a connection pool, reports health via
// the bus, and is a dependency of other modules.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	popcorn "github.com/paluszkiewiczB/popcorn/review"
)

// DB is a long-running module that owns a database/sql pool.
type DB struct {
	id  string
	dsn string
	pub popcorn.Publisher
	log *slog.Logger
	db  *sql.DB
}

var _ popcorn.Module = (*DB)(nil)

// NewDB wires the module with its bound publisher.
func NewDB(id, dsn string, pub popcorn.Publisher, log *slog.Logger) *DB {
	return &DB{id: id, dsn: dsn, pub: pub, log: log}
}

func (m *DB) ID() string             { return m.id }
func (m *DB) Dependencies() []string { return nil }

// Start opens and pings the pool, then announces healthy.
func (m *DB) Start(ctx context.Context) (popcorn.StopFunc, error) {
	db, err := sql.Open("pgx", m.dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	m.db = db

	// Health is a normal event. The caller's context (trace, baggage) is preserved.
	// The module reports only the new state; the kernel derived the previous one.
	if err := m.pub.Send(ctx, popcorn.NewEvent(popcorn.ModuleStateChanged{
		To: popcorn.ModuleStateOK,
	})); err != nil {
		return nil, err
	}

	// io.Closer -> StopFunc without boilerplate.
	return popcorn.StopFuncFromCloser(m.db), nil
}

// Pool is exposed to dependents through ordinary dependency injection, not the bus.
func (m *DB) Pool() *sql.DB { return m.db }

func main() {
	log := slog.Default()

	bus, err := popcorn.NewBus(popcorn.WithBusLogger(log))
	if err != nil {
		panic(err)
	}

	db := NewDB(
		"example/db",
		"postgres://user:pass@localhost:5432/app",
		bus.Publisher("example/db"),
		log,
	)

	kernel, err := popcorn.NewKernel(
		popcorn.WithBus(bus),
		popcorn.WithLogger(log),
		popcorn.WithModules(db),
		popcorn.WithHealthTick(time.Second),
		popcorn.WithStopTimeout(10*time.Second),
		popcorn.WithParallelism(0), // GOMAXPROCS
	)
	if err != nil {
		panic(err)
	}

	if err := kernel.Start(context.Background()); err != nil && !errors.Is(err, popcorn.ErrKernelStopped) {
		panic(err)
	}
}
