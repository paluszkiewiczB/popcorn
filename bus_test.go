package popcorn_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

func TestBus_Send_DeliversToAllListeners(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	is := is.New(t)
	bus, err := popcorn.NewBus()
	is.NoErr(err)

	ch := make(chan popcorn.Event, 1)
	is.NoErr(bus.Subscribe("a", ch))
	is.True(bus.Subscribe("a", ch) != nil)
}

func TestBus_Subscribe_EmptyID(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	bus, err := popcorn.NewBus()
	is.NoErr(err)

	ch := make(chan popcorn.Event, 1)
	is.True(bus.Subscribe("", ch) != nil)
}

func TestBus_NilBus(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	var bus *popcorn.Bus
	is.NoErr(bus.Send(context.Background(), popcorn.NewEvent[string]("test", "hello")))
	bus.Unsubscribe("a")
}

func TestBus_Buffer(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx := context.Background()

	bus, err := popcorn.NewBus()
	is.NoErr(err)

	bus.StartBuffering()

	evt := popcorn.NewEvent[string]("test", "hello")
	is.NoErr(bus.Send(ctx, evt))

	buf := bus.Buffer()
	is.Equal(len(buf), 1)
	is.Equal(buf[0], evt)

	bus.FinishBuffering()
	is.Equal(len(bus.Buffer()), 0)
}

func TestBus_WithBusLogger(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	bus, err := popcorn.NewBus(popcorn.WithBusLogger(slog.Default()))
	is.NoErr(err)
	is.True(bus != nil)
}

func TestBus_WithSendTimeout(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	bus, err := popcorn.NewBus(popcorn.WithSendTimeout(2 * time.Second))
	is.NoErr(err)
	is.True(bus != nil)
}

func TestBus_Buffer_ReplayOnSubscribe(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx := context.Background()

	bus, err := popcorn.NewBus()
	is.NoErr(err)
	bus.StartBuffering()

	e1 := popcorn.NewEvent[string]("src1", "hello")
	e2 := popcorn.NewEvent[string]("src2", "world")

	is.NoErr(bus.Send(ctx, e1))
	is.NoErr(bus.Send(ctx, e2))

	ch := make(chan popcorn.Event, 2)
	is.NoErr(bus.Subscribe("a", ch))

	select {
	case received := <-ch:
		is.Equal(received, e1)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for buffered event replay")
	}

	select {
	case received := <-ch:
		is.Equal(received, e2)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second buffered event replay")
	}
}

func TestBus_Buffer_ReplayOnlySinceSubscription(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx := context.Background()

	bus, err := popcorn.NewBus()
	is.NoErr(err)

	ch1 := make(chan popcorn.Event, 10)
	is.NoErr(bus.Subscribe("a", ch1))

	e1 := popcorn.NewEvent[string]("src", "before")
	is.NoErr(bus.Send(ctx, e1))
	is.Equal(<-ch1, e1)

	bus.StartBuffering()

	e2 := popcorn.NewEvent[string]("src", "buffered")
	is.NoErr(bus.Send(ctx, e2))

	ch2 := make(chan popcorn.Event, 10)
	is.NoErr(bus.Subscribe("b", ch2))

	select {
	case received := <-ch2:
		is.Equal(received, e2)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for buffered event replay")
	}

	select {
	case <-ch2:
		t.Fatal("subscriber b should not receive pre-buffering events")
	default:
	}
}

func TestBus_Buffer_DeliversAndBuffersSimultaneously(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx := context.Background()

	bus, err := popcorn.NewBus()
	is.NoErr(err)

	bus.StartBuffering()

	ch := make(chan popcorn.Event, 1)
	is.NoErr(bus.Subscribe("a", ch))

	e := popcorn.NewEvent[string]("src", "live")
	is.NoErr(bus.Send(ctx, e))

	is.Equal(<-ch, e)

	buf := bus.Buffer()
	is.Equal(len(buf), 1)
	is.Equal(buf[0], e)
}

func TestBus_Buffer_TOCTOURace(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx := context.Background()

	bus, err := popcorn.NewBus()
	is.NoErr(err)

	bus.StartBuffering()

	ch := make(chan popcorn.Event, 10)
	is.NoErr(bus.Subscribe("a", ch))

	var wg sync.WaitGroup

	for range 20 {
		wg.Go(func() {
			e := popcorn.NewEvent[string]("src", "racing")
			_ = bus.Send(ctx, e)
		})
	}

	wg.Go(func() {
		bus.FinishBuffering()
	})

	wg.Wait()

	// All 20 events must have been delivered.
	// At the time of Send, there were 0–20 events buffered.
	// The subscriber was registered before Send, so every event should be received.
	count := 0
	for range 20 {
		select {
		case <-ch:
			count++
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event delivery")
		}
	}
	is.Equal(count, 20)
}

func TestBus_Buffer_ConcurrentSendDuringStartup(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx := context.Background()

	bus, err := popcorn.NewBus()
	is.NoErr(err)
	bus.StartBuffering()

	ch1 := make(chan popcorn.Event, 10)
	is.NoErr(bus.Subscribe("a", ch1))

	for i := range 5 {
		e := popcorn.NewEvent[string]("early", fmt.Sprintf("msg-%d", i))
		is.NoErr(bus.Send(ctx, e))
	}

	// Late subscriber joins — should receive all 5 buffered events
	ch2 := make(chan popcorn.Event, 10)
	is.NoErr(bus.Subscribe("b", ch2))

	received := 0
	for range 5 {
		select {
		case <-ch2:
			received++
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for buffered event replay to late subscriber")
		}
	}
	is.Equal(received, 5)
}

func TestBus_Buffer_ConcurrentSendDuringStartup_NoSubscriberYet(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ctx := context.Background()

	bus, err := popcorn.NewBus()
	is.NoErr(err)
	bus.StartBuffering()

	for i := range 5 {
		e := popcorn.NewEvent[string]("early", fmt.Sprintf("msg-%d", i))
		is.NoErr(bus.Send(ctx, e))
	}

	// Late subscriber joins after events were sent
	ch := make(chan popcorn.Event, 10)
	is.NoErr(bus.Subscribe("a", ch))

	received := 0
	for range 5 {
		select {
		case <-ch:
			received++
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for buffered events")
		}
	}
	is.Equal(received, 5)
}
