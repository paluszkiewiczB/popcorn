package popcorn_test

import (
	"context"
	"testing"
	"time"

	"github.com/matryer/is"
	popcorn "github.com/paluszkiewiczB/popcorn"
)

// tick is the reference payload used across bus tests. Its kind derives from
// the type name: "tick".
type tick struct{ N int }

func newBus(t *testing.T, opts ...popcorn.BusOption) *popcorn.Bus {
	is := is.New(t)
	b, err := popcorn.NewBus(opts...)
	is.NoErr(err) // zero-config bus must be constructible
	return b
}

func Test_Bus(test *testing.T) {
	test.Run("options", func(t *testing.T) {
		t.Run("negative replay buffer rejected", func(t *testing.T) {
			is := is.New(t)
			_, err := popcorn.NewBus(popcorn.WithReplayBuffer(-1))
			is.True(err != nil) // invalid options must be rejected at construction
		})

		t.Run("negative backlog rejected", func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			_, err := b.Subscribe("neg", popcorn.WithBacklog(-1))
			is.True(err != nil) // negative ring sizes must be rejected
		})
	})

	test.Run("subscribe validation", func(t *testing.T) {
		b := newBus(t)

		t.Run("empty id rejected", func(t *testing.T) {
			is := is.New(t)

			_, err := b.Subscribe("")
			is.True(err != nil) // Subscribe must reject an empty id
		})

		t.Run("duplicate id rejected", func(t *testing.T) {
			is := is.New(t)

			ch, err := b.Subscribe("dup")
			is.NoErr(err)
			is.True(ch != nil)

			_, err = b.Subscribe("dup")
			is.True(err != nil) // Subscribe must reject an already-registered id
		})
	})

	test.Run("owns and closes channels", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t)
		ch, err := b.Subscribe("owned")
		is.NoErr(err)
		is.True(ch != nil)

		// The bus passing a receive-only channel proves modules can never write
		// or close; that discipline is enforced at compile time by the signature.
		b.Unsubscribe("owned")

		done := make(chan struct{})
		go func() { // the channel must be closed by Unsubscribe
			<-ch
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(never):
			is.Fail() // Unsubscribe must close the subscription channel
		}

		b.Unsubscribe("owned") // second call must not panic
	})

	test.Run("publisher stamps the bound source", func(t *testing.T) {
		is := is.New(t)

		b := newBus(t)
		other, err := b.Subscribe("other")
		is.NoErr(err)

		pub := b.Publisher("owner")
		e := popcorn.NewEvent(tick{N: 1})

		ctx := context.Background()
		is.NoErr(pub.Send(ctx, e)) // Send must deliver

		got := mustRecv(is, other)
		is.Equal(got.Source(), "owner") // Send stamps the bound id
		is.Equal(got.Kind, "tick")
		is.Equal(got.Payload, tick{N: 1})
	})

	test.Run("send skips own subscription", func(t *testing.T) {
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

		// The publisher's own subscription must never receive its own send.
		// The ring contract makes absence observable: drain until quiescent.
		select {
		case <-self:
			is.Fail() // sender must not receive its own event
		case <-time.After(300 * time.Millisecond):
		}
	})

	test.Run("backlog ring", func(t *testing.T) {
		t.Run("slow subscriber overflows only its own ring", func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			slow, err := b.Subscribe("slow", popcorn.WithBacklog(2))
			is.NoErr(err)

			pub := b.Publisher("p")
			for i := 1; i <= 5; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			// Drop-oldest: exactly the 2 most recent events survive. Send must
			// not have blocked on the unread subscription, and overflow must
			// not surface as a Send error.
			fast, err := b.Subscribe("fast", popcorn.WithBacklog(5))
			is.NoErr(err)
			is.True(len(drained(is, fast)) == 5) // a healthy subscriber loses nothing

			got := drained(is, slow)
			is.True(len(got) == 2) // ring size bounds delivery
			is.Equal(got[0].Payload, tick{N: 4})
			is.Equal(got[1].Payload, tick{N: 5})
		})

		t.Run("default subscription is unbuffered", func(t *testing.T) {
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
			for i := 0; i < 50; i++ {
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}

			// Nobody read during the burst: with no backlog, every event faces
			// the drop rule; up to one may be in the delivery goroutine's hand.
			is.True(len(drained(is, slow)) <= 1)                                   // rendezvous must not queue events for unread subscribers
			is.True(len(drained(is, other)) >= 1 && len(drained(is, other)) <= 50) // other subs still work
		})
	})

	test.Run("filter", func(t *testing.T) {
		t.Run("keeps matching and drops the rest", func(t *testing.T) {
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
			is.Equal(got.Payload, tick{N: 1})         // filter must keep matching events
			is.True(len(drained(is, filtered)) == 1)  // ...and drop non-matching ones
			is.True(len(drained(is, strangers)) == 2) // unfiltered subscription sees both
		})

		t.Run("nil filter allows all", func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			permissive, err := b.Subscribe("permissive", popcorn.WithFilter(nil))
			is.NoErr(err)

			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEventOf("unrelated", 99)))
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			is.True(len(drained(is, permissive)) == 2) // a nil filter accepts everything
		})

		t.Run("panicking filter is recovered", func(t *testing.T) {
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
			is.True(len(drained(is, sane)) == 1)
			is.True(len(drained(is, guarded)) == 0) // panicking filter drops the event
		})
	})

	test.Run("replay", func(t *testing.T) {
		t.Run("subscribing after history picks it up", func(t *testing.T) {
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 2})))

			late, err := b.Subscribe("late", popcorn.WithBacklog(8))
			is.NoErr(err)

			is.True(len(drained(is, late)) == 2) // the replay buffer must be seeded
		})

		t.Run("replay honors filter and backlog", func(t *testing.T) {
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
			is.True(len(got) == 2) // backlog caps replay
			is.Equal(got[0].Payload, tick{N: 3})
			is.Equal(got[1].Payload, tick{N: 4})
		})

		t.Run("history thinner than backlog replays in full", func(t *testing.T) {
			is := is.New(t)

			// history of 2, subscription ring of 8: the seam must replay at
			// most min(history, backlog) — here the history bounds it.
			b := newBus(t, popcorn.WithReplayBuffer(2))
			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 2})))

			late, err := b.Subscribe("late", popcorn.WithBacklog(8))
			is.NoErr(err)

			is.True(len(drained(is, late)) == 2) // exactly the whole thin history
		})

		t.Run("no gap, no duplicate at the seam", func(t *testing.T) {
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

			var seen []popcorn.Event
			quiesce := never
			for {
				select {
				case e := <-late:
					if e.Kind == "sentinel" {
						goto after
					}
					seen = append(seen, e)
				case <-time.After(quiesce):
					goto after
				}
			}
		after:
			<-doneLive

			is.True(len(seen) == 6) // replay + live must be complete: 6 unique events

			uniq := map[int]int{}
			for _, e := range seen {
				n := e.Payload.(tick).N
				uniq[n]++
				is.True(uniq[n] == 1) // no duplicate delivery (seam)
			}

			got := make([]int, 0, len(seen))
			for _, e := range seen {
				got = append(got, e.Payload.(tick).N)
			}
			for i, n := range got {
				is.Equal(n, i+1) // order must be preserved across the seam
			}
		})

		t.Run("replay off by default", func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			late, err := b.Subscribe("late")
			is.NoErr(err)

			is.True(len(drained(is, late)) == 0) // without history, past events are not replayed
		})
	})

	test.Run("deliver", func(t *testing.T) {
		t.Run("Send never stalls on a dead subscriber", func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			_, err := b.Subscribe("never-reads") // a dead subscriber nobody reads
			is.NoErr(err)

			pub := b.Publisher("p")
			for i := 0; i < 100; i++ { // Send must return without delivery having happened
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}
		})

		t.Run("canceled context surfaces as error", func(t *testing.T) {
			is := is.New(t)

			b := newBus(t)
			pub := b.Publisher("p")

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			is.True(pub.Send(ctx, popcorn.NewEvent(tick{})) != nil) // a canceled ctx must not be swallowed
		})
	})

	test.Run("nil bus safety", func(t *testing.T) {
		// B10: zero-value safety — every bus method on a nil target must be safe.
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
