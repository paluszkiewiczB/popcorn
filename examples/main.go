package main

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"github.com/paluszkiewiczB/popcorn"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// HTTPModule is a facade of reusable HTTP module.
type HTTPModule struct {
	cfg HttpCfg

	State *popcorn.ModuleStateStore
	// URL is set when the module is started - State stores [popcorn.ModuleStateOK].
	URL string
}

// HttpCfg is a configuration for HTTPModule.
type HttpCfg struct {
	ModID      string
	Host       string
	Handler    http.Handler
	ServerOpt  func(*http.Server)
	StartDelay time.Duration
	Bus        *popcorn.Bus
}

// NewHTTPModule creates a new HTTPModule.
func NewHTTPModule(cfg HttpCfg) *HTTPModule {
	return &HTTPModule{
		cfg:   cfg,
		State: &popcorn.ModuleStateStore{},
	}
}

func (m *HTTPModule) ModRecipe() popcorn.ModRecipe {
	id := "github.com/paluszkiewiczB/popcorn/examples/HttpModule"
	if len(m.cfg.ModID) != 0 {
		id = m.cfg.ModID
	}

	return popcorn.ModRecipe{
		ID: id,
		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
			time.Sleep(m.cfg.StartDelay)
			listener, err := new(net.ListenConfig).Listen(ctx, "tcp4", m.cfg.Host)
			if err != nil {
				return popcorn.NoStop(fmt.Errorf("listening on tcp: %s, %w", m.cfg.Host, err))
			}
			srv := &http.Server{
				Addr:    m.cfg.Host,
				Handler: m.cfg.Handler,
			}

			if m.cfg.ServerOpt != nil {
				m.cfg.ServerOpt(srv)
			}

			done := make(chan error, 1)
			go func() {
				log.Printf("starting http server on: %q", m.cfg.Host)
				done <- srv.Serve(listener)
			}()

			evt := popcorn.NewEvent[HTTPServerListens](id, HTTPServerListens{URL: "http://" + listener.Addr().String()})
			err = m.cfg.Bus.Send(ctx, evt)
			if err != nil {
				return popcorn.NoStop(fmt.Errorf("sending event: %v, %w", evt, err))
			}

			return func(ctx context.Context) error {
				log.Printf("stopping http server on: %q", m.cfg.Host)
				err := srv.Shutdown(ctx)
				servErr := <-done
				if errors.Is(servErr, http.ErrServerClosed) {
					servErr = nil
				}

				return errors.Join(err, servErr)
			}, nil
		},
	}
}

// HTTPServerListens is an event sent by the HTTPModule when HTTP server starts listening.
type HTTPServerListens struct {
	URL string
}

func main() {
	bus := must2(popcorn.NewBus(nil))

	s1 := NewHTTPModule(HttpCfg{
		Host:       ":3000",
		Handler:    dummyHandler(),
		StartDelay: 2 * time.Second,
		Bus:        bus,
	})

	s2 := NewHTTPModule(HttpCfg{
		ModID:      "github.com/paluszkiewiczB/popcorn/examples/HttpModule@3001",
		Host:       ":3001",
		Handler:    dummyHandler(),
		StartDelay: 5 * time.Second,
		Bus:        bus,
	})

	pinger := &PingerModule{
		Tick: time.Second,
	}

	s1r := s1.ModRecipe()
	s1m := must2(popcorn.NewModule(s1r))

	s2r := s2.ModRecipe()
	s2m := must2(popcorn.NewModule(s2r))

	pm := must2(popcorn.NewModule(pinger.ModRecipe(s1r.ID, s2r.ID)))

	kernel := must2(popcorn.NewKernel(bus, slog.Default(), s1m, s2m, pm))

	ctx, cf := context.WithTimeout(context.Background(), 15*time.Second)
	defer cf()

	must(kernel.Start(ctx))
}

type PingerModule struct {
	id   string
	Tick time.Duration
	Bus  *popcorn.Bus
}

func (m PingerModule) ModRecipe(deps ...string) popcorn.ModRecipe {
	var d driver.Driver
	d.Open()

	evts := make(chan popcorn.Event, 10)
	m.id = "github.com/paluszkiewiczB/popcorn/examples/PingerModule"

	return popcorn.ModRecipe{
		ID:           m.id,
		Dependencies: deps,
		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
			p := &Pinger{
				c:    &http.Client{},
				tick: m.Tick,
			}

			retainedCtx, cf := popcorn.RetainContextCause(ctx)

			go func() {
				var addrs []string

				addrs, err := m.waitForAddressesOfHttpServers(retainedCtx, evts)
				if err != nil {
					err = m.Bus.Send(retainedCtx, popcorn.NewEvent[popcorn.ModuleStatusChanged](m.id, popcorn.ModuleStatusChanged{
						ID:    m.id,
						From:  popcorn.ModuleStateUnknown,
						To:    popcorn.ModuleStateNOK,
						Cause: "waiting for addressed of http servers failed: " + err.Error(),
					}))

					if err != nil {
						log.Printf("sending event failed: %s", err)
					}
				}

				p.addrs = addrs
				p.PeriodicallyPing(retainedCtx)
			}()

			return func(ctx context.Context) (err error) {
				cf(fmt.Errorf("stop func called, %w", ctx.Err()))
				p.Close()
				return nil
			}, nil
		},
		EventsChan: evts,
	}
}

func (m PingerModule) waitForAddressesOfHttpServers(ctx context.Context, evts chan popcorn.Event) ([]string, error) {
	var addrs []string
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for addresses of http servers canceled, %w", ctx.Err())
		case evt := <-evts:
			switch p := evt.Payload.(type) {
			case HTTPServerListens:
				addrs = append(addrs, p.URL)
			case popcorn.ModuleStarted:
				if p.ID == m.id {
					return addrs, nil
				}
			}
		}
	}
}

type Pinger struct {
	c     *http.Client
	addrs []string

	tick   time.Duration
	ticker *time.Ticker
}

func (m *Pinger) PeriodicallyPing(ctx context.Context) {
	log.Printf("starting pinger with tick: %s", m.tick)
	ticker := time.NewTicker(m.tick)
	m.ticker = ticker
	for {
		select {
		case <-ctx.Done():
			log.Printf("stopping pinger")
			return
		case t, ok := <-ticker.C:
			if !ok {
				log.Printf("ticker channel closed")
				return
			}

			log.Printf("tick: %s", t)
			for _, addr := range m.addrs {
				if err := m.doPing(ctx, addr); err != nil {
					log.Printf("failed to ping %q, %s", addr, err)
				}
			}
		}
	}
}

func (m *Pinger) doPing(ctx context.Context, addr string) error {
	log.Printf("pinging %q", addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return fmt.Errorf("building new ping request for addr: %q, %s", addr, err)
	}

	resp, err := m.c.Do(req)
	if err != nil {
		return fmt.Errorf("pinging %q, %s", addr, err)
	}

	defer resp.Body.Close()
	all, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body, %s", err)
	}

	log.Printf("pinged %q, status: %q, body: %q", addr, resp.Status, string(all))
	return nil
}

func (m *Pinger) Close() error {
	m.ticker.Stop()
	return nil
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func must2[T any](val T, err error) T {
	must(err)
	return val
}

func dummyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("received request on: %v", r.Host)
		_, _ = w.Write([]byte(r.Host))
	}
}
