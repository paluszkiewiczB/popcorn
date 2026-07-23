package popcorn_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

func TestBus_Send_DeliversToAllListeners(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()

	bus, err := popcorn.NewBus(popcorn.WithSendTimeout(time.Second))
	is.NoErr(err)

	ch1 := make(chan popcorn.Event, 1)
	ch2 := make(chan popcorn.Event, 1)

	is.NoErr(bus.Subscribe("a", ch1))
	is.NoErr(bus.Subscribe("b", ch2))

	evt := popcorn.NewEvent[string]("test", "hello")
	is.NoErr(bus.Send(ctx, evt))

	is.Equal(<-ch1, evt)
	is.Equal(<-ch2, evt)
}

func TestBus_Send_Timeout(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()

	bus, err := popcorn.NewBus(popcorn.WithSendTimeout(time.Millisecond))
	is.NoErr(err)

	ch := make(chan popcorn.Event) // unbuffered, no reader
	is.NoErr(bus.Subscribe("a", ch))

	err = bus.Send(ctx, popcorn.NewEvent[string]("test", "hello"))
	is.True(err != nil)
}

func TestBus_Subscribe_DuplicateID(t *testing.T) {
	is := is.New(t)
	bus, err := popcorn.NewBus()
	is.NoErr(err)

	ch := make(chan popcorn.Event, 1)
	is.NoErr(bus.Subscribe("a", ch))
	is.True(bus.Subscribe("a", ch) != nil)
}

func TestBus_Subscribe_EmptyID(t *testing.T) {
	is := is.New(t)
	bus, err := popcorn.NewBus()
	is.NoErr(err)

	ch := make(chan popcorn.Event, 1)
	is.True(bus.Subscribe("", ch) != nil)
}

func TestBus_NilBus(t *testing.T) {
	is := is.New(t)
	var bus *popcorn.Bus
	is.NoErr(bus.Send(context.Background(), popcorn.NewEvent[string]("test", "hello")))
	bus.Unsubscribe("a")
}

func TestBus_Buffer(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()

	bus, err := popcorn.NewBus()
	is.NoErr(err)

	bus.SetBuffering(true)
	evt := popcorn.NewEvent[string]("test", "hello")
	is.NoErr(bus.Send(ctx, evt))

	buf := bus.Buffer()
	is.Equal(len(buf), 1)
	is.Equal(buf[0], evt)

	bus.ClearBuffer()
	is.Equal(len(bus.Buffer()), 0)
}

func TestBus_WithBusLogger(t *testing.T) {
	is := is.New(t)

	bus, err := popcorn.NewBus(popcorn.WithBusLogger(slog.Default()))
	is.NoErr(err)
	is.True(bus != nil)
}

func TestBus_WithSendTimeout(t *testing.T) {
	is := is.New(t)

	bus, err := popcorn.NewBus(popcorn.WithSendTimeout(2 * time.Second))
	is.NoErr(err)
	is.True(bus != nil)
}
