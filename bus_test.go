package popcorn_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

// tick is the reference payload used across bus tests. Its kind derives from
// the type name: "tick".
type tick struct{ N int }

func newBus(t *testing.T, opts ...popcorn.BusOption) *popcorn.Bus {
	t.Helper()

	is := is.New(t)
	b, err := popcorn.NewBus(opts...)
	is.NoErr(err) // zero-config bus must be constructible
	t.Cleanup(b.Close)
	return b
}

func TestBusOptions(t *testing.T) {
	t.Parallel()
	t.Run("negative replay buffer rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		_, err := popcorn.NewBus(popcorn.WithReplayBuffer(-1))
		is.True(err != nil) // invalid options must be rejected at construction
	})

	t.Run("negative backlog rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		_, err := b.Subscribe("neg", popcorn.WithBacklog(-1))
		is.True(err != nil) // negative ring sizes must be rejected
	})

	t.Run("empty id rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		_, err := b.Subscribe("")
		is.True(err != nil) // Subscribe must reject an empty id
	})

	t.Run("duplicate id rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		ch, err := b.Subscribe("dup")
		is.NoErr(err)
		is.True(ch != nil)

		_, err = b.Subscribe("dup")
		is.True(err != nil) // Subscribe must reject an already-registered id
	})

	t.Run("owns and closes channels", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		ch, err := b.Subscribe("owned")
		is.NoErr(err)
		is.True(ch != nil)

		// The bus passing a receive-only channel proves modules can never write
		// or close; that discipline is enforced at compile time by the signature.
		b.Unsubscribe("owned")

		_, open := <-ch
		is.True(!open)         // Unsubscribe must close the subscription channel
		b.Unsubscribe("owned") // second call must not panic
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
			is.NoErr(pub.Send(context.Background(), sent)) // Send must deliver

			got := mustRecv(is, other)
			is.Equal(got.Source(), "owner") // Send stamps the bound id
			is.Equal(got.Kind, "tick")
			is.Equal(got.Payload, tick{N: 1})
			is.Equal(got.At, sent.At) // Send must preserve the event's timestamp
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

			// The publisher's own subscription is never a target, so it must
			// already be empty when the peer has received the event.
			select {
			case <-self:
				is.Fail() // sender must not receive its own event
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

			// A later buffered subscriber replays the whole history.
			fast, err := b.Subscribe("fast", popcorn.WithBacklog(5))
			is.NoErr(err)
			fastGot := drained(is, fast)
			is.Equal(len(fastGot), 5) // a healthy subscriber loses nothing
			for i, e := range fastGot {
				is.Equal(e.Payload, tick{N: i + 1})
			}

			// Drop-oldest: exactly the 2 most recent events survive. Send must
			// not have blocked on the unread subscription, and overflow must
			// not surface as a Send error.
			got := drained(is, slow)
			is.Equal(len(got), 2) // ring size bounds delivery
			is.Equal(got[0].Payload, tick{N: 4})
			is.Equal(got[1].Payload, tick{N: 5})
		})
	})

	t.Run("default subscription is unbuffered", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			slow, err := b.Subscribe("slow") // WithBacklog(0) => rendezvous
			is.NoErr(err)

			// Contract: a rendezvous subscription never buffers - Send enqueues
			// without blocking, and events nobody is ready to read are simply
			// not sitting in an ever-growing queue. There must never be
			// unbounded accumulation behind a reader that was not ready.
			other, err := b.Subscribe("other", popcorn.WithBacklog(1))
			is.NoErr(err)

			pub := b.Publisher("p")
			for i := range 50 {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			// Nobody read during the burst: with no backlog, every event faces
			// the drop rule; at most one may be in the delivery goroutine's hand.
			nSlow := len(drained(is, slow))
			is.True(nSlow <= 1) // rendezvous must not queue events for unread subscribers

			// A backlog-1 ring drops all but the newest event.
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
			strangers, err := b.Subscribe("stranger")
			is.NoErr(err)

			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEventOf("unrelated", 99)))
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			got := mustRecv(is, filtered)
			is.Equal(got.Payload, tick{N: 1})       // filter must keep matching events
			is.Equal(len(drained(is, filtered)), 0) // ...and drop non-matching ones

			strangerGot := drained(is, strangers)
			is.Equal(len(strangerGot), 2) // unfiltered subscription sees both
			is.Equal(strangerGot[0].Payload, 99)
			is.Equal(strangerGot[1].Payload, tick{N: 1})
		})
	})

	t.Run("nil filter allows all", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			permissive, err := b.Subscribe("permissive", popcorn.WithFilter(nil))
			is.NoErr(err)

			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEventOf("unrelated", 99)))
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			got := drained(is, permissive)
			is.Equal(len(got), 2) // a nil filter accepts everything
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

			// A panicking filter must never take down Send or another
			// subscriber: the event is simply dropped for the guarded
			// subscription.
			saneGot := drained(is, sane)
			is.Equal(len(saneGot), 1)
			is.Equal(saneGot[0].Payload, tick{N: 1})
			is.Equal(len(drained(is, guarded)), 0) // panicking filter drops the event
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
			is.Equal(len(got), 3) // the replay buffer must be seeded
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
			is.Equal(len(got), 2) // backlog caps replay
			is.Equal(got[0].Payload, tick{N: 3})
			is.Equal(got[1].Payload, tick{N: 4})
		})
	})

	t.Run("replay buffer bounds retained history", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			// history capped at 2, subscription ring of 8: the newest 2 events
			// are replayed, older ones have been evicted from history.
			b := newBus(t, popcorn.WithReplayBuffer(2))
			pub := b.Publisher("p")
			for i := 1; i <= 5; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			late, err := b.Subscribe("late", popcorn.WithBacklog(8))
			is.NoErr(err)

			got := drained(is, late)
			is.Equal(len(got), 2) // history is bounded by the replay buffer
			is.Equal(got[0].Payload, tick{N: 4})
			is.Equal(got[1].Payload, tick{N: 5})
		})
	})

	t.Run("default replay buffer keeps the last 64 events", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t) // no option: the 64-event default applies
			pub := b.Publisher("p")
			for i := 1; i <= 100; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			late, err := b.Subscribe("late", popcorn.WithBacklog(64))
			is.NoErr(err)

			got := drained(is, late)
			is.Equal(len(got), 64) // the default bound is 64
			for i, e := range got {
				is.Equal(e.Payload, tick{N: i + 37}) // the newest 64, oldest first
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

			is.Equal(len(drained(is, late)), 0) // WithReplayBuffer(0) must retain nothing
		})
	})

	t.Run("ring wraps around across multiple cycles", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			// Cap 3 with 10 sends crosses the ring boundary more than twice.
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
				is.True(ok) // every replayed event must carry a tick payload
				replayed = append(replayed, tk.N)
			}
			is.Equal(replayed, []int{8, 9, 10}) // only the newest cap survive, in order
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

			// Live traffic flows right across the subscribe seam; a sentinel
			// event marks the end of live delivery so the drain below is
			// exact, not a quiescence guess.
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
				is.True(ok) // every replayed/live event must carry a tick payload
				got = append(got, tk.N)
			}
			<-doneLive

			is.Equal(got, []int{1, 2, 3, 4, 5, 6}) // replay + live complete and ordered, no duplicates
		})
	})

	t.Run("rendezvous subscriber is not seeded", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			late, err := b.Subscribe("late")
			is.NoErr(err)

			is.Equal(len(drained(is, late)), 0) // a rendezvous subscription is never seeded
		})
	})
}

func TestBusMisc(t *testing.T) {
	t.Parallel()
	t.Run("Send never stalls on a dead subscriber", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		b := newBus(t)
		_, err := b.Subscribe("never-reads") // a dead subscriber nobody reads
		is.NoErr(err)

		pub := b.Publisher("p")
		for i := range 100 { // Send must return without delivery having happened
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
		is.True(errors.Is(err, context.Canceled)) // a canceled ctx must not be swallowed
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

			// Tear the filtered subscription down and replace it with the same id
			// while Send is parked inside the filter.
			b.Unsubscribe("racy")
			_, err = b.Subscribe("racy", popcorn.WithBacklog(4))
			is.NoErr(err)
			close(release)

			is.NoErr(<-sendErr) // Send must survive the id being recycled
		})
	})

	t.Run("nil bus safety", func(t *testing.T) {
		t.Parallel()
		// Zero-value safety — every bus method on a nil target must be safe.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("nil target must be safe, panicked with: %v", r)
			}
		}()

		var b *popcorn.Bus
		is := is.New(t)

		ch, err := b.Subscribe("x")
		is.True(err != nil) // nil bus must surface an error
		is.True(ch == nil)  // and never hand out a channel

		pu := b.Publisher("p")
		if pu != nil {
			_ = pu.Send(context.Background(), popcorn.NewEvent(tick{}))
		}
		b.Unsubscribe("x")
	})
}
