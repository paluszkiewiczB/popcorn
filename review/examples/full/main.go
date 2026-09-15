// Command full wires every use case together: a database, a one-shot migration, an
// HTTP server, Kubernetes probes, a scheduled job, and a recipe-built module. It
// shows how the pieces compose with one bus and one kernel.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	popcorn "github.com/paluszkiewiczB/popcorn/review"
)

// ---------------------------------------------------------------------------
// Database module
// ---------------------------------------------------------------------------

type DB struct {
	id  string
	dsn string
	pub popcorn.Publisher
	db  *sql.DB
}

var _ popcorn.Module = (*DB)(nil)

func NewDB(id, dsn string, pub popcorn.Publisher) *DB {
	return &DB{id: id, dsn: dsn, pub: pub}
}

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

func (m *DB) Pool() *sql.DB { return m.db }

// ---------------------------------------------------------------------------
// One-shot migration (TaskModule)
// ---------------------------------------------------------------------------

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
		_, _ = m.db.Pool().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS users (id bigserial PRIMARY KEY)`)
	}()

	return nil, nil
}

// ---------------------------------------------------------------------------
// HTTP module
// ---------------------------------------------------------------------------

type HTTP struct {
	id          string
	addr        string
	db          *DB
	migrationID string
	bus         *popcorn.Bus
	pub         popcorn.Publisher
	log         *slog.Logger
	server      *http.Server
}

var _ popcorn.Module = (*HTTP)(nil)

func NewHTTP(id, addr string, db *DB, migrationID string, bus *popcorn.Bus, log *slog.Logger) *HTTP {
	return &HTTP{
		id:          id,
		addr:        addr,
		db:          db,
		migrationID: migrationID,
		bus:         bus,
		pub:         bus.Publisher(id),
		log:         log,
	}
}

func (m *HTTP) ID() string { return m.id }

// Depend on the migration, not the DB: the server must start only once the schema
// exists. A TaskModule dependency is ready when Done closes.
func (m *HTTP) Dependencies() []string { return []string{m.migrationID} }

func (m *HTTP) Start(ctx context.Context) (popcorn.StopFunc, error) {
	events, err := m.bus.Subscribe(m.id, popcorn.WithBacklog(32))
	if err != nil {
		return nil, err
	}

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", m.addr)
	if err != nil {
		m.bus.Unsubscribe(m.id)
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		row := m.db.Pool().QueryRowContext(r.Context(), `SELECT count(*) FROM users`)
		var n int
		if err := row.Scan(&n); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"users": n})
	})

	m.server = &http.Server{Addr: ln.Addr().String(), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = m.server.Serve(ln) }()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-events:
				if !ok {
					return
				}
				m.log.DebugContext(ctx, "http event", "kind", e.Kind, "source", e.Source())
			}
		}
	}()

	return func(stopCtx context.Context) error {
		defer m.bus.Unsubscribe(m.id)
		return m.server.Shutdown(stopCtx)
	}, nil
}

// ---------------------------------------------------------------------------
// Probes module — k8s liveness/readiness/startup
// ---------------------------------------------------------------------------

type Probes struct {
	id      string
	addr    string
	bus     *popcorn.Bus
	server  *http.Server
	mu      sync.RWMutex
	kernel  popcorn.KernelState
	modules map[string]popcorn.ModuleState
}

var _ popcorn.Module = (*Probes)(nil)

func NewProbes(id, addr string, bus *popcorn.Bus) *Probes {
	return &Probes{id: id, addr: addr, bus: bus, kernel: popcorn.KernelStateUnknown, modules: map[string]popcorn.ModuleState{}}
}

func (m *Probes) ID() string             { return m.id }
func (m *Probes) Dependencies() []string { return nil }

func (m *Probes) Start(ctx context.Context) (popcorn.StopFunc, error) {
	events, err := m.bus.Subscribe(
		m.id,
		popcorn.WithBacklog(128),
		popcorn.WithFilter(func(e popcorn.Event) bool {
			switch e.Payload.(type) {
			case popcorn.KernelStateChanged, popcorn.ModuleStateChanged:
				return true
			default:
				return false
			}
		}),
	)
	if err != nil {
		return nil, err
	}

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", m.addr)
	if err != nil {
		m.bus.Unsubscribe(m.id)
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.RLock()
		ok := m.kernel == popcorn.KernelStateRunning
		m.mu.RUnlock()
		writeJSON(w, ok)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.RLock()
		ok := m.kernel == popcorn.KernelStateRunning
		for _, s := range m.modules {
			ok = ok && s != popcorn.ModuleStateNOK
		}
		m.mu.RUnlock()
		writeJSON(w, ok)
	})
	mux.HandleFunc("/startupz", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.RLock()
		ok := m.kernel == popcorn.KernelStateRunning
		m.mu.RUnlock()
		writeJSON(w, ok)
	})

	m.server = &http.Server{Addr: ln.Addr().String(), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = m.server.Serve(ln) }()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-events:
				if !ok {
					return
				}
				m.apply(e)
			}
		}
	}()

	return func(stopCtx context.Context) error {
		defer m.bus.Unsubscribe(m.id)
		return m.server.Shutdown(stopCtx)
	}, nil
}

func (m *Probes) apply(e popcorn.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch v := e.Payload.(type) {
	case popcorn.KernelStateChanged:
		m.kernel = v.To
	case popcorn.ModuleStateChanged:
		m.modules[e.Source()] = v.To
	}
}

func writeJSON(w http.ResponseWriter, ok bool) {
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": ok})
}

// ---------------------------------------------------------------------------
// Scheduled job
// ---------------------------------------------------------------------------

type Scheduled struct {
	id       string
	interval time.Duration
	log      *slog.Logger
	cancel   context.CancelFunc
	stopped  chan struct{}
}

var _ popcorn.Module = (*Scheduled)(nil)

func NewScheduled(id string, interval time.Duration, log *slog.Logger) *Scheduled {
	return &Scheduled{id: id, interval: interval, log: log}
}

func (m *Scheduled) ID() string             { return m.id }
func (m *Scheduled) Dependencies() []string { return nil }

func (m *Scheduled) Start(ctx context.Context) (popcorn.StopFunc, error) {
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.stopped = make(chan struct{})

	go func() {
		defer close(m.stopped)
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				m.log.InfoContext(runCtx, "scheduled tick")
			}
		}
	}()

	return func(stopCtx context.Context) error {
		m.cancel()
		select {
		case <-m.stopped:
			return nil
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}, nil
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	log := slog.Default()

	bus, err := popcorn.NewBus(
		popcorn.WithBusLogger(log),
		popcorn.WithReplayBuffer(256),
	)
	if err != nil {
		panic(err)
	}

	db := NewDB("example/db", "postgres://user:pass@localhost:5432/app", bus.Publisher("example/db"))
	migrate := NewMigration("example/migrate", db)
	httpSrv := NewHTTP("example/http", ":8080", db, migrate.ID(), bus, log)
	probes := NewProbes("example/probes", ":9090", bus)
	cleanup := NewScheduled("example/cleanup", 10*time.Minute, log)

	// A module built from a recipe: no struct needed for trivial behavior.
	metrics, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "example/metrics",
		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
			log.InfoContext(ctx, "metrics module started")
			return func(context.Context) error {
				log.Info("metrics module stopped")
				return nil
			}, nil
		},
	})
	if err != nil {
		panic(err)
	}

	kernel, err := popcorn.NewKernel(
		popcorn.WithBus(bus),
		popcorn.WithLogger(log),
		popcorn.WithModules(db, migrate, httpSrv, probes, cleanup, metrics),
		popcorn.WithHealthTick(time.Second),
		popcorn.WithStopTimeout(15*time.Second),
		popcorn.WithParallelism(0), // start/stop independent modules concurrently
	)
	if err != nil {
		panic(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err = kernel.Start(ctx)
	switch {
	case err == nil || errors.Is(err, popcorn.ErrKernelStopped):
		log.Info("kernel stopped gracefully")
	default:
		var unhealthy popcorn.KernelUnhealthyError
		if errors.As(err, &unhealthy) {
			log.Error("kernel stopped because a module is unhealthy", "module", unhealthy.ModuleID, "err", unhealthy.Cause)
		} else {
			log.Error("kernel stopped", "err", err)
		}
		panic(err)
	}
}
