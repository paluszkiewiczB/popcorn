// Command migration shows a one-shot TaskModule. The kernel runs finite tasks and,
// with WithExitWhenIdle, shuts down once they finish — a CLI-style program.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	popcorn "github.com/paluszkiewiczB/popcorn/review"
)

// DB is a minimal database module.
type DB struct {
	id  string
	dsn string
	db  *sql.DB
}

var _ popcorn.Module = (*DB)(nil)

func NewDB(id, dsn string) *DB { return &DB{id: id, dsn: dsn} }

func (m *DB) ID() string             { return m.id }
func (m *DB) Dependencies() []string { return nil }

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

	return popcorn.StopFuncFromCloser(m.db), nil
}

// Pool returns the pool to dependents via ordinary DI.
func (m *DB) Pool() *sql.DB { return m.db }

// Migration is a TaskModule: it depends on the DB, runs once, and closes Done.
type Migration struct {
	id   string
	db   *DB
	done chan struct{}
}

var _ popcorn.TaskModule = (*Migration)(nil)

func NewMigration(id string, db *DB) *Migration {
	return &Migration{id: id, db: db, done: make(chan struct{})}
}

func (m *Migration) ID() string             { return m.id }
func (m *Migration) Dependencies() []string { return []string{m.db.ID()} }
func (m *Migration) Done() <-chan struct{}  { return m.done }

func (m *Migration) Start(ctx context.Context) (popcorn.StopFunc, error) {
	go func() {
		defer close(m.done)

		const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
			version bigint PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`

		if _, err := m.db.Pool().ExecContext(ctx, ddl); err != nil {
			slog.ErrorContext(ctx, "migration failed", slog.String("err", err.Error()))
		}
	}()

	// Nothing to release: the DB module owns the pool.
	return nil, nil
}

func main() {
	log := slog.Default()
	bus, err := popcorn.NewBus(popcorn.WithBusLogger(log))
	if err != nil {
		panic(err)
	}

	db := NewDB("example/db", "postgres://user:pass@localhost:5432/app")
	migrate := NewMigration("example/migrate", db)

	kernel, err := popcorn.NewKernel(
		popcorn.WithBus(bus),
		popcorn.WithLogger(log),
		popcorn.WithModules(db, migrate),
		popcorn.WithParallelism(1),     // CLI: keep it serial
		popcorn.WithExitWhenIdle(true), // stop once the migration task is done
	)
	if err != nil {
		panic(err)
	}

	if err := kernel.Start(context.Background()); err != nil && !errors.Is(err, popcorn.ErrKernelStopped) {
		panic(err)
	}
}
