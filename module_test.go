package popcorn_test

import (
	"context"
	"errors"
	"testing"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

// honest is a module implementation bypassing recipes, for cases where the
// contract under test is the kernel's handling of arbitrary Module values.
type honest struct {
	id   string
	deps []string
}

func (m honest) ID() string                                        { return m.id }
func (m honest) Dependencies() []string                            { return m.deps }
func (m honest) Start(_ context.Context) (popcorn.StopFunc, error) { return noStop, nil }

func Test_Module(t *testing.T) {
	t.Parallel()

	t.Run("empty id rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		m, err := popcorn.NewModule(popcorn.ModRecipe{Start: noopStart})
		is.True(errors.Is(err, popcorn.ErrModuleIDNotSet)) // NewModule must reject an empty id
		is.True(m == nil)
	})

	t.Run("reserved id rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		m, err := popcorn.NewModule(*recipe("kernel", noopStart))
		is.True(errors.Is(err, popcorn.ErrModuleIDReserved)) // "kernel" is reserved by the kernel
		is.True(m == nil)
	})

	t.Run("nil start rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		m, err := popcorn.NewModule(popcorn.ModRecipe{ID: "a"})
		is.True(errors.Is(err, popcorn.ErrModuleStartNotSet)) // a recipe without Start is rejected
		is.True(m == nil)
	})

	t.Run("Done non-nil satisfies TaskModule", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		done := make(chan struct{})
		m, err := popcorn.NewModule(popcorn.ModRecipe{ID: taskID, Done: done, Start: noopStart})
		is.NoErr(err)
		is.Equal(m.ID(), taskID)
		is.Equal(len(m.Dependencies()), 0)

		tm, isTask := m.(popcorn.TaskModule)
		is.Equal(isTask, true) // module with Done must satisfy TaskModule
		is.Equal(tm.Done(), (<-chan struct{})(done))
	})

	t.Run("Done nil does not satisfy TaskModule", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		plain, err := popcorn.NewModule(*recipe("plain", noopStart))
		is.NoErr(err)
		_, isTask := plain.(popcorn.TaskModule)
		is.Equal(isTask, false) // module without Done must not satisfy TaskModule
	})

	t.Run("dependencies are copied defensively", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		deps := []string{"a"}
		m, err := popcorn.NewModule(popcorn.ModRecipe{ID: "copy", Dependencies: deps, Start: noopStart})
		is.NoErr(err)

		deps[0] = "mutated" // mutating the caller's slice after NewModule

		is.Equal(m.Dependencies(), []string{"a"}) // the module must keep a defensive copy
	})

	t.Run("nil closer yields nil stop", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		is.True(popcorn.StopFuncFromCloser(nil) == nil) // nil io.Closer => nil StopFunc
	})

	t.Run("closer adapts to stop", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		closed := false
		sf := popcorn.StopFuncFromCloser(&closerSpy{onClose: func() { closed = true }})
		is.True(sf != nil)
		is.NoErr(sf(context.Background()))
		is.Equal(closed, true) // StopFunc must invoke Close
	})
}

type closerSpy struct{ onClose func() }

func (c *closerSpy) Close() error { c.onClose(); return nil }
