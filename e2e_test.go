package popcorn_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
	"github.com/paluszkiewiczB/popcorn/plog/attr"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

const (
	idA = "a"
	idB = "b"
	idC = "c"
)

func Test_DependentModules(t *testing.T) {
	t.Run("module b should be started before module a, because it has a dependency", func(t *testing.T) {
		is := newIs(t)
		ctx := context.Background()
		ctx, cf := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
		defer cf()

		bus, err := popcorn.NewBus(nil)
		is.NoErr(err)

		var stopModuleA eventCb = func(event popcorn.Event) {
			err := bus.Send(ctx, popcorn.NewEvent[popcorn.ModuleStatusChanged](idA, popcorn.ModuleStatusChanged{
				ID:    idA,
				From:  popcorn.ModuleStateOK,
				To:    popcorn.ModuleStateNOK,
				Cause: "the other module has started",
			}))

			// FIXME: should I get the context.Canceled error here?
			is.NoCtxErr(err)
		}

		// even though module A starts as the second one
		// and will not be notified about module B being started,
		// missing event should be buffered
		// and send to module A too

		aStore, aChan := newEventStore(t,
			stopModuleA.when(payloadMatches(func(started popcorn.ModuleStarted) bool { return started.ID == idA })),
		)
		defer aStore.startStoring(aChan)()

		modA, err := popcorn.NewModule(popcorn.ModRecipe{
			ID:           idA,
			Dependencies: []string{idB},
			EventsChan:   aChan,
			Start: func(ctx context.Context) (popcorn.StopFunc, error) {
				slog.InfoContext(ctx, "module starting", attr.ModId(idA))
				return func(ctx context.Context) error {
					slog.InfoContext(ctx, "module stopping", attr.ModId(idA))
					return nil
				}, nil
			},
		})
		is.NoErr(err)

		modB, err := popcorn.NewModule(popcorn.ModRecipe{
			ID: idB,
			Start: func(ctx context.Context) (popcorn.StopFunc, error) {
				log.Printf("module b starting")
				return nil, nil
			},
		})
		is.NoErr(err)

		kernel, err := popcorn.NewKernel(
			bus,
			slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
			modA, modB,
		)
		is.NoErr(err)

		err = kernel.Start(ctx)

		// kernel should stop due to module A being unhealthy
		unhealthy := popcorn.ErrKernelUnhealthy{}
		if !errors.As(err, &unhealthy) {
			is.NoErr(err)
		}

		is.True(strings.Contains(unhealthy.Error(), idA))

		is.Equal(len(aStore.evts), 2)
		is.True(payloadMatches[popcorn.ModuleStarted](
			func(started popcorn.ModuleStarted) bool {
				return started.ID == idB
			},
		)(aStore.evts[0]))
		is.True(payloadMatches[popcorn.ModuleStarted](
			func(started popcorn.ModuleStarted) bool {
				return started.ID == idA
			},
		)(aStore.evts[1]))
	})
}

func newEventStore(t *testing.T, cbs ...eventCb) (eventStore, chan popcorn.Event) {
	store := eventStore{
		t:         t,
		callBacks: cbs,
	}
	return store, make(chan popcorn.Event)
}

type eventFilter func(popcorn.Event) bool

func (f eventFilter) and(next eventFilter) eventFilter {
	return func(event popcorn.Event) bool {
		return f(event) && next(event)
	}
}

type eventCb func(popcorn.Event)

func (cb eventCb) when(f eventFilter) eventCb {
	return func(event popcorn.Event) {
		if f(event) {
			cb(event)
		}
	}
}

func payloadIs[T any]() eventFilter {
	return func(evt popcorn.Event) bool {
		var t T
		return reflect.TypeOf(evt.Payload) == reflect.TypeOf(t)
	}
}

func payloadMatches[T any](f func(T) bool) eventFilter {
	return payloadIs[T]().and(func(event popcorn.Event) bool {
		p := event.Payload.(T)
		return f(p)
	})
}

type eventStore struct {
	t         *testing.T
	evts      []popcorn.Event
	callBacks []eventCb
}

func (s *eventStore) startStoring(c <-chan popcorn.Event) func() {
	ctx := context.Background()
	ctx, cf := context.WithCancel(ctx)

	fmt.Printf("listening for events: %p\n", c)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-c:
				s.evts = append(s.evts, event)
				for _, cb := range s.callBacks {
					cb(event)
				}
			}
		}
	}()

	return cf
}

func newIs(t *testing.T) *popIs {
	return &popIs{I: is.New(t)}
}

type popIs struct {
	*is.I
}

func (is *popIs) NoCtxErr(err error) {
	is.I.Helper()
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, context.DeadlineExceeded):
		return
	default:
		is.NoErr(err)
	}
}
