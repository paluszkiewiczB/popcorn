// Command http shows an HTTP server module that subscribes to bus events itself,
// from inside Start, once it is ready to process them.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	popcorn "github.com/paluszkiewiczB/popcorn/review"
)

// ServerListens is the module's own event: it tells subscribers where the server bound.
type ServerListens struct {
	URL string
}

// HTTP is a long-running module that owns an http.Server.
type HTTP struct {
	id   string
	addr string
	bus  *popcorn.Bus
	pub  popcorn.Publisher
	log  *slog.Logger
	srv  *http.Server
}

var _ popcorn.Module = (*HTTP)(nil)

// NewHTTP wires the module with the bus (for subscribing) and its bound publisher.
func NewHTTP(id, addr string, bus *popcorn.Bus, log *slog.Logger) *HTTP {
	return &HTTP{id: id, addr: addr, bus: bus, pub: bus.Publisher(id), log: log}
}

func (m *HTTP) ID() string             { return m.id }
func (m *HTTP) Dependencies() []string { return nil }

func (m *HTTP) Start(ctx context.Context) (popcorn.StopFunc, error) {
	// Subscribe explicitly, when ready. A bounded backlog plus the bus replay
	// history means this module can still see recent events it needs, without the
	// kernel knowing anything about its subscriptions.
	events, err := m.bus.Subscribe(m.id, popcorn.WithBacklog(64))
	if err != nil {
		return nil, err
	}

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", m.addr)
	if err != nil {
		m.bus.Unsubscribe(m.id)
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "hello from %s", m.id)
	})

	m.srv = &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- m.srv.Serve(ln) }()

	// Announce the address so subscribers can reach us.
	if err := m.pub.Send(ctx, popcorn.NewEvent(ServerListens{URL: "http://" + ln.Addr().String()})); err != nil {
		return nil, err
	}

	// Consume events from the bus-owned channel. select keeps normal Go concurrency.
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

		shutdownErr := m.srv.Shutdown(stopCtx)

		serveDone := <-serveErr
		if errors.Is(serveDone, http.ErrServerClosed) {
			serveDone = nil
		}

		return errors.Join(shutdownErr, serveDone)
	}, nil
}

func main() {
	log := slog.Default()
	bus, err := popcorn.NewBus(
		popcorn.WithBusLogger(log),
		popcorn.WithReplayBuffer(128), // enable a bounded replay history
	)
	if err != nil {
		panic(err)
	}

	srv := NewHTTP("example/http", ":8080", bus, log)

	kernel, err := popcorn.NewKernel(
		popcorn.WithBus(bus),
		popcorn.WithLogger(log),
		popcorn.WithModules(srv),
	)
	if err != nil {
		panic(err)
	}

	if err := kernel.Start(context.Background()); err != nil && !errors.Is(err, popcorn.ErrKernelStopped) {
		panic(err)
	}
}
