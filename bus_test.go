package popcorn_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/matryer/is"
	popcorn "github.com/paluszkiewiczB/popcorn"
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

func Test_Bus(t *testing.T) {
	t.Parallel()
	synctest.Test(t, testBus)
}

func testBus(test *testing.T) {
	testBusOptions(test)
	testBusPublishing(test)
	testBusFilter(test)
	testBusReplay(test)
	testBusMisc(test)
}

func testBusOptions(t *testing.T) {
	t.Helper()
	step(t, "options", func(t *testing.T) {
		t.Helper()
		step(t, "negative replay buffer rejected", func(t *testing.T) {
			t.Helper()
			is := is.New(t)
			_, err := popcorn.NewBus(popcorn.WithReplayBuffer(-1))
			is.True(err != nil) // invalid options must be rejected at construction
		})

		step(t, "negative backlog rejected", func(t *testing.T) {
			t.Helper()
			is := is.New(t)

			b := newBus(t)
			_, err := b.Subscribe("neg", popcorn.WithBacklog(-1))
			is.True(err != nil) // negative ring sizes must be rejected
		})
	})

	step(t, "subscribe validation", func(t *testing.T) {
		t.Helper()
		b := newBus(t)

		step(t, "empty id rejected", func(t *testing.T) {
			t.Helper()
			is := is.New(t)

			_, err := b.Subscribe("")
			is.True(err != nil) // Subscribe must reject an empty id
		})

		step(t, "duplicate id rejected", func(t *testing.T) {
			t.Helper()
			is := is.New(t)

			ch, err := b.Subscribe("dup")
			is.NoErr(err)
			is.True(ch != nil)

			_, err = b.Subscribe("dup")
			is.True(err != nil) // Subscribe must reject an already-registered id
		})
	})

	step(t, "owns and closes channels", func(t *testing.T) {
		t.Helper()
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
}

func testBusPublishing(t *testing.T) {
	t.Helper()
	step(t, "publisher stamps the bound source", func(t *testing.T) {
		t.Helper()
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

	step(t, "send skips own subscription", func(t *testing.T) {
		t.Helper()
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

	step(t, "backlog ring", func(t *testing.T) {
		t.Helper()
		step(t, "slow subscriber overflows only its own ring", func(t *testing.T) {
			t.Helper()
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

		step(t, "default subscription is unbuffered", func(t *testing.T) {
			t.Helper()
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
			// the drop rule; up to one may be in the delivery goroutine's hand.
			nSlow := len(drained(is, slow))
			is.True(nSlow <= 1) // rendezvous must not queue events for unread subscribers

			nOther := len(drained(is, other))
			is.True(nOther >= 1 && nOther <= 50) // other subs still work
		})
	})
}

func testBusFilter(t *testing.T) {
	t.Helper()
	step(t, "filter", func(t *testing.T) {
		t.Helper()
		step(t, "keeps matching and drops the rest", func(t *testing.T) {
			t.Helper()
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
			is.True(len(drained(is, filtered)) == 0)  // ...and drop non-matching ones
			is.True(len(drained(is, strangers)) == 2) // unfiltered subscription sees both
		})

		step(t, "nil filter allows all", func(t *testing.T) {
			t.Helper()
			is := is.New(t)

			b := newBus(t)
			permissive, err := b.Subscribe("permissive", popcorn.WithFilter(nil))
			is.NoErr(err)

			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEventOf("unrelated", 99)))
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			is.True(len(drained(is, permissive)) == 2) // a nil filter accepts everything
		})

		step(t, "panicking filter is recovered", func(t *testing.T) {
			t.Helper()
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
}

func testBusReplay(t *testing.T) {
	t.Helper()
	step(t, "replay", func(t *testing.T) {
		t.Helper()
		step(t, "subscribing after history picks it up", func(t *testing.T) {
			t.Helper()
			is := is.New(t)

			b := newBus(t, popcorn.WithReplayBuffer(8))

			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 2})))

			late, err := b.Subscribe("late", popcorn.WithBacklog(8))
			is.NoErr(err)

			is.True(len(drained(is, late)) == 2) // the replay buffer must be seeded
		})

		step(t, "replay honors filter and backlog", func(t *testing.T) {
			t.Helper()
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

		step(t, "history thinner than backlog replays in full", func(t *testing.T) {
			t.Helper()
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

		step(t, "no gap, no duplicate at the seam", func(t *testing.T) {
			t.Helper()
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
				tk, ok := e.Payload.(tick)
				is.True(ok) // every seen event must carry a tick payload
				uniq[tk.N]++
				is.True(uniq[tk.N] == 1) // no duplicate delivery (seam)
			}

			got := make([]int, 0, len(seen))
			for _, e := range seen {
				tk, ok := e.Payload.(tick)
				is.True(ok)
				got = append(got, tk.N)
			}
			for i, n := range got {
				is.Equal(n, i+1) // order must be preserved across the seam
			}
		})

		step(t, "rendezvous subscriber is not seeded", func(t *testing.T) {
			t.Helper()
			is := is.New(t)

			b := newBus(t)
			pub := b.Publisher("p")
			is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: 1})))

			late, err := b.Subscribe("late")
			is.NoErr(err)

			is.True(len(drained(is, late)) == 0) // a rendezvous subscription is never seeded
		})
	})
}

func testBusMisc(t *testing.T) {
	t.Helper()
	step(t, "deliver", func(t *testing.T) {
		t.Helper()
		step(t, "Send never stalls on a dead subscriber", func(t *testing.T) {
			t.Helper()
			is := is.New(t)

			b := newBus(t)
			_, err := b.Subscribe("never-reads") // a dead subscriber nobody reads
			is.NoErr(err)

			pub := b.Publisher("p")
			for i := range 100 { // Send must return without delivery having happened
				is.NoErr(pub.Send(context.Background(), popcorn.NewEvent(tick{N: i})))
			}
		})

		step(t, "canceled context surfaces as error", func(t *testing.T) {
			t.Helper()
			is := is.New(t)

			b := newBus(t)
			pub := b.Publisher("p")

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			is.True(pub.Send(ctx, popcorn.NewEvent(tick{})) != nil) // a canceled ctx must not be swallowed
		})

		step(t, "unsubscribe and resubscribe during a filter must not panic", func(t *testing.T) {
			t.Helper()
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

			select {
			case err := <-sendErr:
				is.NoErr(err) // Send must survive the id being recycled
			case <-time.After(never):
				is.Fail() // Send never completed
			}
		})
	})

	step(t, "nil bus safety", func(t *testing.T) {
		t.Helper()
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
