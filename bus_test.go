package popcorn_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

type tick struct{ N int }

func newBus(t *testing.T, opts ...popcorn.BusOption) *popcorn.Bus {
	t.Helper()

	is := is.New(t)
	b, err := popcorn.NewBus(opts...)
	is.NoErr(err)
	t.Cleanup(b.Close)
	return b
}

func TestBusOptions(t *testing.T) {
	t.Parallel()
	t.Run("negative replay buffer rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		_, err := popcorn.NewBus(popcorn.WithReplayBuffer(-1))
		is.True(err != nil)
	})

	t.Run("negative backlog rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		_, err := b.Subscribe("neg", popcorn.WithBacklog(-1))
		is.True(err != nil)
	})

	t.Run("empty id rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		_, err := b.Subscribe("")
		is.True(err != nil)
	})

	t.Run("nil bus logger falls back to discard", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b, err := popcorn.NewBus(popcorn.WithBusLogger(nil))
		is.NoErr(err)
		b.Close()
	})

	t.Run("duplicate id rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		ch, err := b.Subscribe("dup")
		is.NoErr(err)
		is.True(ch != nil)

		_, err = b.Subscribe("dup")
		is.True(err != nil)
	})

	t.Run("owns and closes channels", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		ch, err := b.Subscribe("owned")
		is.NoErr(err)
		is.True(ch != nil)

		b.Unsubscribe("owned")

		_, open := <-ch
		is.True(!open)
		b.Unsubscribe("owned")
	})
}

func TestBusPublishing(t *testing.T) {
	t.Parallel()
	t.Run("publisher stamps the bound source", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			other, err := b.Subscribe("other")
			is.NoErr(err)

			pub := b.Publisher("owner")
			sent := popcorn.NewEvent(tick{N: 1})
			is.NoErr(pub.Send(context.Background(), sent))

			got := mustRecv(is, other)
			is.Equal(got.Source(), "owner")
			is.Equal(got.Kind, "tick")
			is.Equal(got.Payload, tick{N: 1})
			is.Equal(got.At, sent.At)
		})
	})

	t.Run("send skips own subscription", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			self, err := b.Subscribe("a")
			is.NoErr(err)
			other, err := b.Subscribe("b")
			is.NoErr(err)

			pub := b.Publisher("a")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			got := mustRecv(is, other)
			is.Equal(got.Source(), "a")

			select {
			case <-self:
				is.Fail()
			default:
			}
		})
	})

	t.Run("slow subscriber overflows only its own ring", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			slow, err := b.Subscribe("slow", popcorn.WithBacklog(2))
			is.NoErr(err)

			pub := b.Publisher("p")
			for i := 1; i <= 5; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			fast, err := b.Subscribe("fast", popcorn.WithBacklog(5))
			is.NoErr(err)
			fastGot := drained(is, fast)
			is.Equal(len(fastGot), 5)
			for i, e := range fastGot {
				is.Equal(e.Payload, tick{N: i + 1})
			}

			got := drained(is, slow)
			is.Equal(len(got), 2)
			is.Equal(got[0].Payload, tick{N: 4})
			is.Equal(got[1].Payload, tick{N: 5})
		})
	})

	t.Run("default subscription buffers one event", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			slow, err := b.Subscribe("slow")
			is.NoErr(err)

			other, err := b.Subscribe("other", popcorn.WithBacklog(1))
			is.NoErr(err)

			pub := b.Publisher("p")
			for i := range 50 {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			is.Equal(len(drained(is, slow)), 1)
			otherGot := drained(is, other)
			is.Equal(len(otherGot), 1)
			is.Equal(otherGot[0].Payload, tick{N: 49})
		})
	})
}

func TestBusFilter(t *testing.T) {
	t.Parallel()
	t.Run("keeps matching and drops the rest", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			filtered, err := b.Subscribe("filtered",
				popcorn.WithBacklog(8),
				popcorn.WithFilter(func(e popcorn.Event) bool { return e.Kind == "tick" }))
			is.NoErr(err)
			strangers, err := b.Subscribe("stranger", popcorn.WithBacklog(8))
			is.NoErr(err)

			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEventOf("unrelated", 99)))
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			got := mustRecv(is, filtered)
			is.Equal(got.Payload, tick{N: 1})
			is.Equal(len(drained(is, filtered)), 0)

			strangerGot := drained(is, strangers)
			is.Equal(len(strangerGot), 2)
			is.Equal(strangerGot[0].Payload, 99)
			is.Equal(strangerGot[1].Payload, tick{N: 1})
		})
	})

	t.Run("nil filter allows all", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			permissive, err := b.Subscribe("permissive",
				popcorn.WithBacklog(8), popcorn.WithFilter(nil))
			is.NoErr(err)

			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEventOf("unrelated", 99)))
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			got := drained(is, permissive)
			is.Equal(len(got), 2)
			is.Equal(got[0].Payload, 99)
			is.Equal(got[1].Payload, tick{N: 1})
		})
	})

	t.Run("panicking filter is recovered", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			guarded, err := b.Subscribe("guarded",
				popcorn.WithBacklog(4),
				popcorn.WithFilter(func(popcorn.Event) bool { panic("bad filter") }))
			is.NoErr(err)
			sane, err := b.Subscribe("sane")
			is.NoErr(err)

			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			saneGot := drained(is, sane)
			is.Equal(len(saneGot), 1)
			is.Equal(saneGot[0].Payload, tick{N: 1})
			is.Equal(len(drained(is, guarded)), 0)
		})
	})
}

func TestBusReplay(t *testing.T) {
	t.Parallel()
	t.Run("subscribing after history picks it up", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			pub := b.Publisher("p")
			for i := 1; i <= 3; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			late, err := b.Subscribe("late", popcorn.WithBacklog(8))
			is.NoErr(err)

			got := drained(is, late)
			is.Equal(len(got), 3)
			is.Equal(got[0].Payload, tick{N: 1})
			is.Equal(got[1].Payload, tick{N: 2})
			is.Equal(got[2].Payload, tick{N: 3})
		})
	})

	t.Run("replay honors filter and backlog", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEventOf("noise", 0)))
			for i := 1; i <= 4; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			late, err := b.Subscribe("late",
				popcorn.WithBacklog(2),
				popcorn.WithFilter(func(e popcorn.Event) bool { return e.Kind == "tick" }))
			is.NoErr(err)

			got := drained(is, late)
			is.Equal(len(got), 2)
			is.Equal(got[0].Payload, tick{N: 3})
			is.Equal(got[1].Payload, tick{N: 4})
		})
	})

	t.Run("replay buffer bounds retained history", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(2))
			pub := b.Publisher("p")
			for i := 1; i <= 5; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			late, err := b.Subscribe("late", popcorn.WithBacklog(8))
			is.NoErr(err)

			got := drained(is, late)
			is.Equal(len(got), 2)
			is.Equal(got[0].Payload, tick{N: 4})
			is.Equal(got[1].Payload, tick{N: 5})
		})
	})

	t.Run("default replay buffer keeps the last 64 events", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			pub := b.Publisher("p")
			for i := 1; i <= 100; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			late, err := b.Subscribe("late", popcorn.WithBacklog(64))
			is.NoErr(err)

			got := drained(is, late)
			is.Equal(len(got), 64)
			for i, e := range got {
				is.Equal(e.Payload, tick{N: i + 37})
			}
		})
	})

	t.Run("zero replay buffer disables history", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(0))
			pub := b.Publisher("p")
			for i := 1; i <= 5; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			late, err := b.Subscribe("late", popcorn.WithBacklog(8))
			is.NoErr(err)

			is.Equal(len(drained(is, late)), 0)
		})
	})

	t.Run("ring wraps around across multiple cycles", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(3))
			pub := b.Publisher("p")
			for i := 1; i <= 10; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			late, err := b.Subscribe("late", popcorn.WithBacklog(8))
			is.NoErr(err)

			replayed := make([]int, 0, 8)
			for _, e := range drained(is, late) {
				tk, ok := e.Payload.(tick)
				is.True(ok)
				replayed = append(replayed, tk.N)
			}
			is.Equal(replayed, []int{8, 9, 10})
		})
	})

	t.Run("no gap, no duplicate at the seam", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))
			pub := b.Publisher("p")

			for i := 1; i <= 3; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			late, err := b.Subscribe("late", popcorn.WithBacklog(8))
			is.NoErr(err)

			doneLive := make(chan struct{})
			go func() {
				defer close(doneLive)
				for i := 4; i <= 6; i++ {
					_ = pub.Send(context.Background(), popcorn.NewEvent(tick{N: i}))
				}
				_ = pub.Send(context.Background(), popcorn.NewEventOf("sentinel", true))
			}()

			var got []int
			for {
				e := <-late
				if e.Kind == "sentinel" {
					break
				}
				tk, ok := e.Payload.(tick)
				is.True(ok)
				got = append(got, tk.N)
			}
			<-doneLive

			is.Equal(got, []int{1, 2, 3, 4, 5, 6})
		})
	})

	t.Run("default subscription is seeded with one event", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			late, err := b.Subscribe("late")
			is.NoErr(err)

			got := drained(is, late)
			is.Equal(len(got), 1)
			is.Equal(got[0].Payload, tick{N: 1})
		})
	})
}

func TestBusMisc(t *testing.T) {
	t.Parallel()
	t.Run("Send never stalls on a dead subscriber", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		_, err := b.Subscribe("never-reads")
		is.NoErr(err)

		pub := b.Publisher("p")
		for i := range 100 {
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
		}
	})

	t.Run("canceled context surfaces as error", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		pub := b.Publisher("p")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := pub.Send(ctx, popcorn.NewEvent(tick{}))
		is.True(errors.Is(err, context.Canceled))
	})

	t.Run("unsubscribe and resubscribe during a filter must not panic", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			_, err := b.Subscribe("racy",
				popcorn.WithBacklog(4),
				popcorn.WithFilter(func(popcorn.Event) bool {
					select {
					case entered <- struct{}{}:
					default:
					}
					<-release
					return true
				}))
			is.NoErr(err)

			pub := b.Publisher("p")
			sendErr := make(chan error, 1)
			go func() { sendErr <- pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})) }()
			<-entered

			b.Unsubscribe("racy")
			_, err = b.Subscribe("racy", popcorn.WithBacklog(4))
			is.NoErr(err)
			close(release)

			is.NoErr(<-sendErr)
		})
	})

	t.Run("nil bus safety", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("nil target must be safe, panicked with: %v", r)
			}
		}()

		var b *popcorn.Bus
		is := is.New(t)

		ch, err := b.Subscribe("x")
		is.True(err != nil)
		is.True(ch == nil)

		pu := b.Publisher("p")
		if pu != nil {
			_ = pu.Send(context.Background(), popcorn.NewEvent(tick{}))
		}
		b.Unsubscribe("x")
		b.Close()

		var p *popcorn.Publisher
		err = p.Send(context.Background(), popcorn.NewEvent(tick{}))
		is.True(err != nil)
	})
}

func TestBusDefaultBuffer(t *testing.T) {
	t.Parallel()
	t.Run("default subscription keeps the newest event", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			ch, err := b.Subscribe("default")
			is.NoErr(err)

			pub := b.Publisher("p")
			for i := range 3 {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			got := make(chan popcorn.Event, 1)
			go func() { got <- <-ch }()
			synctest.Wait()

			select {
			case e := <-got:
				is.Equal(e.Payload, tick{N: 2})
			case <-time.After(never):
				is.Fail()
			}
		})
	})
}
