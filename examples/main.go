package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/paluszkiewiczB/popcorn"
)

// HTTPModule is a reusable HTTP server module.
type HTTPModule struct {
	cfg HTTPConfig
}

// HTTPConfig is the configuration for HTTPModule.
type HTTPConfig struct {
	ModID     string
	Handler   http.Handler
	ServerOpt func(*http.Server)
}

// NewHTTPModule creates a new HTTPModule.
func NewHTTPModule(cfg HTTPConfig) *HTTPModule {
	return &HTTPModule{cfg: cfg}
}

// ModRecipe returns a recipe for registering this module with the kernel.
func (m *HTTPModule) ModRecipe() popcorn.ModRecipe {
	id := "github.com/paluszkiewiczB/popcorn/examples/HTTPModule"
	if m.cfg.ModID != "" {
		id = m.cfg.ModID
	}

	return popcorn.ModRecipe{
		ID: id,
		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
			listener, err := new(net.ListenConfig).Listen(ctx, "tcp4", ":0")
			if err != nil {
				return nil, fmt.Errorf("listening: %w", err)
			}

			srv := &http.Server{
				Addr:         listener.Addr().String(),
				Handler:      m.cfg.Handler,
				ReadTimeout:  5 * time.Second,
				WriteTimeout: 5 * time.Second,
				IdleTimeout:  60 * time.Second,
			}
			if m.cfg.ServerOpt != nil {
				m.cfg.ServerOpt(srv)
			}

			done := make(chan error, 1)
			go func() {
				slog.InfoContext(ctx, "http server starting", slog.String("addr", listener.Addr().String()))
				done <- srv.Serve(listener)
			}()

			return func(ctx context.Context) error {
				slog.InfoContext(ctx, "http server stopping", slog.String("addr", listener.Addr().String()))
				shutdownErr := srv.Shutdown(ctx)
				serveErr := <-done
				if errors.Is(serveErr, http.ErrServerClosed) {
					serveErr = nil
				}
				return errors.Join(shutdownErr, serveErr)
			}, nil
		},
	}
}

// HTTPServerListens is emitted when the HTTP server starts listening.
type HTTPServerListens struct {
	ID  string
	URL string
}

// PingerModule pings the HTTP servers it depends on and then finishes.
// It implements popcorn.TaskModule to demonstrate finite (CLI-style) work.
type PingerModule struct {
	id       string
	deps     []string
	bus      *popcorn.Bus
	client   *http.Client
	maxPings int
	done     chan struct{}
}

// NewPingerModule creates a new PingerModule.
func NewPingerModule(bus *popcorn.Bus, maxPings int, deps ...string) *PingerModule {
	return &PingerModule{
		id:       "github.com/paluszkiewiczB/popcorn/examples/PingerModule",
		bus:      bus,
		deps:     deps,
		maxPings: maxPings,
		done:     make(chan struct{}),
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// ModRecipe returns a recipe for registering this module with the kernel.
func (m *PingerModule) ModRecipe() popcorn.ModRecipe {
	evts := make(chan popcorn.Event, 1)

	return popcorn.ModRecipe{
		ID:           m.id,
		Dependencies: m.deps,
		EventsChan:   evts,
		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
			retainedCtx, cancel := popcorn.RetainContext(ctx)

			go func() {
				defer close(m.done)
				addrs, err := m.waitForAddresses(retainedCtx, evts)
				if err != nil {
					slog.ErrorContext(retainedCtx, "failed to wait for addresses", slog.String("err", err.Error()))
					return
				}
				m.pingAll(retainedCtx, addrs)
			}()

			return func(ctx context.Context) error {
				cancel()
				return nil
			}, nil
		},
	}
}

// Done implements popcorn.TaskModule.
func (m *PingerModule) Done() <-chan struct{} {
	return m.done
}

// waitForAddresses blocks until the module has received an HTTPServerListens event
// for each dependency and the ModuleStarted event for itself.
func (m *PingerModule) waitForAddresses(ctx context.Context, evts <-chan popcorn.Event) ([]string, error) {
	seen := make(map[string]struct{}, len(m.deps))
	var addrs []string

	for len(seen) < len(m.deps) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case evt := <-evts:
			switch p := evt.Payload.(type) {
			case HTTPServerListens:
				if _, ok := seen[p.ID]; !ok {
					seen[p.ID] = struct{}{}
					addrs = append(addrs, p.URL)
				}
			case popcorn.ModuleStarted:
				if p.ID == m.id {
					return addrs, nil
				}
			}
		}
	}

	return addrs, nil
}

func (m *PingerModule) pingAll(ctx context.Context, addrs []string) {
	for i := 0; i < m.maxPings; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		for _, addr := range addrs {
			if err := m.ping(ctx, addr); err != nil {
				slog.ErrorContext(ctx, "ping failed", slog.String("addr", addr), slog.String("err", err.Error()))
			}
		}
		time.Sleep(time.Second)
	}
}

func (m *PingerModule) ping(ctx context.Context, addr string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading body: %w", err)
	}

	slog.InfoContext(ctx, "pinged", slog.String("addr", addr), slog.String("status", resp.Status), slog.String("body", string(body)))
	return nil
}

func main() {
	bus, err := popcorn.NewBus()
	if err != nil {
		panic(err)
	}

	httpMod := NewHTTPModule(HTTPConfig{
		Handler: echoHandler(),
	})

	httpRecipe := httpMod.ModRecipe()
	httpModule, err := popcorn.NewModule(httpRecipe)
	if err != nil {
		panic(err)
	}

	pinger := NewPingerModule(bus, 5, httpRecipe.ID)
	pingerRecipe := pinger.ModRecipe()
	pingerModule, err := popcorn.NewModule(pingerRecipe)
	if err != nil {
		panic(err)
	}

	kernel, err := popcorn.NewKernel(
		popcorn.WithBus(bus),
		popcorn.WithLogger(slog.Default()),
		popcorn.WithModules(httpModule, pingerModule),
	)
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := kernel.Start(ctx); err != nil {
		panic(err)
	}
}

func echoHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Host))
	}
}

// must panics if err is non-nil.
func must(err error) {
	if err != nil {
		panic(err)
	}
}

// must2 panics if err is non-nil, otherwise returns val.
func must2[T any](val T, err error) T {
	must(err)
	return val
}
