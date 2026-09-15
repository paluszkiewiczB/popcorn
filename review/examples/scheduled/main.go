// Command scheduled shows a long-running module driven by a ticker. Stop cancels the
// work and waits for it to finish, bounded by the shutdown context.
package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	popcorn "github.com/paluszkiewiczB/popcorn/review"
)

// Scheduled runs a job on a fixed interval until stopped.
type Scheduled struct {
	id       string
	interval time.Duration
	pub      popcorn.Publisher
	log      *slog.Logger

	cancel  context.CancelFunc
	stopped chan struct{}
}

var _ popcorn.Module = (*Scheduled)(nil)

func NewScheduled(id string, interval time.Duration, pub popcorn.Publisher, log *slog.Logger) *Scheduled {
	return &Scheduled{id: id, interval: interval, pub: pub, log: log}
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
				if err := m.tick(runCtx); err != nil {
					// Degrade to temporary NOK; recover on the next successful tick.
					_ = m.pub.Send(runCtx, popcorn.NewEvent(popcorn.ModuleStateChanged{
						To:    popcorn.ModuleStateTempNOK,
						Cause: err,
					}))
				}
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

func (m *Scheduled) tick(ctx context.Context) error {
	m.log.InfoContext(ctx, "scheduled tick")
	return nil
}

func main() {
	log := slog.Default()
	bus, err := popcorn.NewBus(popcorn.WithBusLogger(log))
	if err != nil {
		panic(err)
	}

	job := NewScheduled("example/scheduled", 5*time.Second, bus.Publisher("example/scheduled"), log)

	kernel, err := popcorn.NewKernel(
		popcorn.WithBus(bus),
		popcorn.WithLogger(log),
		popcorn.WithModules(job),
	)
	if err != nil {
		panic(err)
	}

	if err := kernel.Start(context.Background()); err != nil && !errors.Is(err, popcorn.ErrKernelStopped) {
		panic(err)
	}
}
