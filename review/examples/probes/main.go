// Command probes shows how Kubernetes liveness/readiness/startup probes are just a
// bus consumer. The module subscribes from inside Start and tracks kernel and module
// state from events. The core framework contains no probe code.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	popcorn "github.com/paluszkiewiczB/popcorn/review"
)

// Probes exposes /livez, /readyz and /startupz derived from bus state events.
type Probes struct {
	id   string
	addr string
	bus  *popcorn.Bus
	log  *slog.Logger
	srv  *http.Server

	mu      sync.RWMutex
	kernel  popcorn.KernelState
	modules map[string]popcorn.ModuleState
}

var _ popcorn.Module = (*Probes)(nil)

func NewProbes(id, addr string, bus *popcorn.Bus, log *slog.Logger) *Probes {
	return &Probes{
		id:      id,
		addr:    addr,
		bus:     bus,
		log:     log,
		kernel:  popcorn.KernelStateUnknown,
		modules: make(map[string]popcorn.ModuleState),
	}
}

func (m *Probes) ID() string             { return m.id }
func (m *Probes) Dependencies() []string { return nil }

func (m *Probes) Start(ctx context.Context) (popcorn.StopFunc, error) {
	// Subscribe only to state events. The optional filter is a subscription option,
	// not an interface method.
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
	mux.HandleFunc("/livez", m.handleLive)
	mux.HandleFunc("/readyz", m.handleReady)
	mux.HandleFunc("/startupz", m.handleStartup)

	m.srv = &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() { _ = m.srv.Serve(ln) }()
	go m.consume(ctx, events)

	return func(stopCtx context.Context) error {
		defer m.bus.Unsubscribe(m.id)
		return m.srv.Shutdown(stopCtx)
	}, nil
}

func (m *Probes) consume(ctx context.Context, events <-chan popcorn.Event) {
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

func (m *Probes) handleLive(w http.ResponseWriter, _ *http.Request) {
	m.mu.RLock()
	running := m.kernel == popcorn.KernelStateRunning
	m.mu.RUnlock()

	writeProbe(w, running, "kernel not running")
}

func (m *Probes) handleReady(w http.ResponseWriter, _ *http.Request) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ready := m.kernel == popcorn.KernelStateRunning
	for _, s := range m.modules {
		if s == popcorn.ModuleStateNOK {
			ready = false
		}
	}

	writeProbe(w, ready, "not ready")
}

func (m *Probes) handleStartup(w http.ResponseWriter, _ *http.Request) {
	m.mu.RLock()
	started := m.kernel != popcorn.KernelStateUnknown && m.kernel != popcorn.KernelStateStarting
	m.mu.RUnlock()

	writeProbe(w, started, "still starting")
}

func writeProbe(w http.ResponseWriter, ok bool, reason string) {
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "reason": reason})
}

func main() {
	log := slog.Default()
	bus, err := popcorn.NewBus(
		popcorn.WithBusLogger(log),
		popcorn.WithReplayBuffer(128),
	)
	if err != nil {
		panic(err)
	}

	probes := NewProbes("example/probes", ":9090", bus, log)

	kernel, err := popcorn.NewKernel(
		popcorn.WithBus(bus),
		popcorn.WithLogger(log),
		popcorn.WithModules(probes),
	)
	if err != nil {
		panic(err)
	}

	if err := kernel.Start(context.Background()); err != nil && !errors.Is(err, popcorn.ErrKernelStopped) {
		panic(err)
	}
}
