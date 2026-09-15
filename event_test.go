package popcorn_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
	popcorn "github.com/paluszkiewiczB/popcorn"
)

// named is a payload type whose kind should be derived as "named".
type named struct{}

func Test_Event(test *testing.T) {
	test.Run("kind", func(t *testing.T) {
		t.Run("named payload derives the type name", func(t *testing.T) {
			is := is.New(t)

			e := popcorn.NewEvent(named{})
			is.Equal(e.Kind, "named") // Kind is the type name of the payload
		})

		t.Run("opaque payloads get a safe kind", func(t *testing.T) {
			is := is.New(t)

			// B14: deriving must not panic for any, pointers and unnamed types.
			probe := func(name string, e popcorn.Event) {
				is.True(e.Kind != "") // kind must not collapse to empty for "+name"
			}

			probe("any", popcorn.NewEvent[any]("x"))
			probe("map", popcorn.NewEvent(map[string]int{}))
			probe("pointer", popcorn.NewEvent(&named{}))
		})

		t.Run("explicit kind via NewEventOf", func(t *testing.T) {
			is := is.New(t)

			e := popcorn.NewEventOf("custom.kind", named{})
			is.Equal(e.Kind, "custom.kind")
			is.Equal(e.Payload, named{})
		})

		t.Run("kinds are distinct per payload type and stable", func(t *testing.T) {
			is := is.New(t)

			first := popcorn.NewEvent(named{})
			pointer := popcorn.NewEvent(&named{})
			mapped := popcorn.NewEvent(map[string]int{})
			slice := popcorn.NewEvent([]named{})

			is.True(first.Kind != pointer.Kind)                  // pointer payloads must not collapse to the base kind
			is.True(first.Kind != mapped.Kind)                   // unnamed payloads must not collide with named kinds
			is.True(mapped.Kind != slice.Kind)                   // distinct unnamed types yield distinct kinds
			is.Equal(popcorn.NewEvent(named{}).Kind, first.Kind) // derivation must be stable
		})
	})

	test.Run("timestamp", func(t *testing.T) {
		t.Run("NewEvent stamps At", func(t *testing.T) {
			is := is.New(t)

			before := time.Now()
			e := popcorn.NewEvent(named{})
			is.True(!e.At.Before(before)) // At is stamped by NewEvent
		})

		t.Run("NewEventOf stamps At", func(t *testing.T) {
			is := is.New(t)

			before := time.Now()
			e := popcorn.NewEventOf("custom.kind", named{})
			is.True(!e.At.Before(before)) // NewEventOf stamps At as well
		})
	})

	test.Run("source is not caller owned", func(t *testing.T) {
		is := is.New(t)

		// B19: the caller can never set the source field; it stays empty until
		// a bound Publisher sets it in Send. No constructor accepts a source.
		e := popcorn.NewEvent(named{})
		is.Equal(e.Source(), "") //source must be empty before any Send
	})

	test.Run("module state rendering", func(t *testing.T) {
		is := is.New(t)

		states := []popcorn.ModuleState{
			popcorn.ModuleStateUnknown,
			popcorn.ModuleStateOK,
			popcorn.ModuleStateTempNOK,
			popcorn.ModuleStateNOK,
		}

		seen := map[string]bool{}
		for _, s := range states {
			is.True(s.String() != "")  // String must render a non-empty name
			is.True(!seen[s.String()]) // rendering must be distinct per state
			seen[s.String()] = true
		}

		is.True(!popcorn.ModuleStateUnknown.IsHealthy()) // unknown is not healthy
		is.True(popcorn.ModuleStateOK.IsHealthy())       // only OK is healthy
		is.True(!popcorn.ModuleStateTempNOK.IsHealthy()) // temporary failure is not healthy
		is.True(!popcorn.ModuleStateNOK.IsHealthy())     // failure is not healthy
	})

	test.Run("kernel state rendering", func(t *testing.T) {
		is := is.New(t)

		states := []popcorn.KernelState{
			popcorn.KernelStateUnknown,
			popcorn.KernelStateStarting,
			popcorn.KernelStateRunning,
			popcorn.KernelStateStopping,
			popcorn.KernelStateStopped,
		}

		seen := map[string]bool{}
		for _, s := range states {
			is.True(s.String() != "")  // String must render a non-empty name
			is.True(!seen[s.String()]) // rendering must be distinct per state
			seen[s.String()] = true
		}
	})

	test.Run("unhealthy error shape", func(t *testing.T) {
		is := is.New(t)

		cause := errors.New("boom")
		err := popcorn.KernelUnhealthyError{ModuleID: "a", Cause: cause}

		is.True(err.Error() != "")
		is.True(errors.Is(err, cause))              // Unwrap must expose the cause
		is.True(strings.Contains(err.Error(), "a")) // the reported module must be named
	})
}
