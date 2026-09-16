package popcorn_test

import (
	"errors"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

// named is a payload type whose kind should be derived as "named".
type named struct{}

func Test_Event(t *testing.T) {
	t.Parallel()

	t.Run("named payload derives the type name", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		is.Equal(popcorn.NewEvent(named{}).Kind, "named") // Kind is the type name of the payload
	})

	t.Run("opaque payloads derive their exact kinds", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		is.Equal(popcorn.NewEvent[any]("x").Kind, "string")
		is.Equal(popcorn.NewEvent(map[string]int{}).Kind, "map[string]int")
		is.Equal(popcorn.NewEvent(&named{}).Kind, "*popcorn_test.named")
		is.Equal(popcorn.NewEvent([]named{}).Kind, "[]popcorn_test.named")
	})

	t.Run("explicit kind via NewEventOf", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		e := popcorn.NewEventOf("custom.kind", named{})
		is.Equal(e.Kind, "custom.kind")
		is.Equal(e.Payload, named{})
	})

	t.Run("kinds are distinct per payload type and stable", func(t *testing.T) {
		t.Parallel()
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

	t.Run("constructors stamp At", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		before := time.Now()
		is.True(!popcorn.NewEvent(named{}).At.Before(before))                  // At is stamped by NewEvent
		is.True(!popcorn.NewEventOf("custom.kind", named{}).At.Before(before)) // NewEventOf stamps At as well
	})

	t.Run("At is caller controlled", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		// At is an exported field, so a listener can overwrite the stamp to test
		// staleness without controlling a clock.
		fixed := time.Unix(0, 0)
		e := popcorn.NewEvent(named{})
		e.At = fixed
		is.Equal(e.At, fixed)
	})

	t.Run("source is not caller owned", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		is.Equal(popcorn.NewEvent(named{}).Source(), "") // source must be empty before any Send
	})

	t.Run("module state rendering", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		is.Equal(popcorn.ModuleStateUnknown.String(), "unknown")
		is.Equal(popcorn.ModuleStateOK.String(), "ok")
		is.Equal(popcorn.ModuleStateTempNOK.String(), "temp-nok")
		is.Equal(popcorn.ModuleStateNOK.String(), "nok")

		is.Equal(popcorn.ModuleStateUnknown.IsHealthy(), false) // unknown is not healthy
		is.Equal(popcorn.ModuleStateOK.IsHealthy(), true)       // only OK is healthy
		is.Equal(popcorn.ModuleStateTempNOK.IsHealthy(), false) // temporary failure is not healthy
		is.Equal(popcorn.ModuleStateNOK.IsHealthy(), false)     // failure is not healthy
	})

	t.Run("kernel state rendering", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		is.Equal(popcorn.KernelStateUnknown.String(), "unknown")
		is.Equal(popcorn.KernelStateStarting.String(), "starting")
		is.Equal(popcorn.KernelStateRunning.String(), "running")
		is.Equal(popcorn.KernelStateStopping.String(), "stopping")
		is.Equal(popcorn.KernelStateStopped.String(), "stopped")
	})

	t.Run("unhealthy error shape", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		err := popcorn.KernelUnhealthyError{ModuleID: "a", Cause: errBoom}

		is.Equal(err.Error(), `module "a" unhealthy: boom`)
		is.True(errors.Is(err, errBoom)) // Unwrap must expose the cause
	})
}
