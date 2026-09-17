package popcorn_test

import (
	"context"
	"errors"
	"testing"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

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
		is.True(errors.Is(err, popcorn.ErrModuleIDNotSet))
		is.True(m == nil)
	})

	t.Run("reserved id rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		m, err := popcorn.NewModule(*recipe("kernel", noopStart))
		is.True(errors.Is(err, popcorn.ErrModuleIDReserved))
		is.True(m == nil)
	})

	t.Run("nil start rejected", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		m, err := popcorn.NewModule(popcorn.ModRecipe{ID: "a"})
		is.True(errors.Is(err, popcorn.ErrModuleStartNotSet))
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
		is.Equal(isTask, true)
		is.Equal(tm.Done(), (<-chan struct{})(done))
	})

	t.Run("Done nil does not satisfy TaskModule", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		plain, err := popcorn.NewModule(*recipe("plain", noopStart))
		is.NoErr(err)
		_, isTask := plain.(popcorn.TaskModule)
		is.Equal(isTask, false)
	})

	t.Run("dependencies are copied defensively", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		deps := []string{"a"}
		m, err := popcorn.NewModule(popcorn.ModRecipe{ID: "copy", Dependencies: deps, Start: noopStart})
		is.NoErr(err)

		deps[0] = "mutated"

		is.Equal(m.Dependencies(), []string{"a"})
	})

	t.Run("nil closer yields nil stop", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		is.True(popcorn.StopCloser(nil) == nil)
	})

	t.Run("closer adapts to stop", func(t *testing.T) {
		t.Parallel()
		is := is.New(t)

		closed := false
		sf := popcorn.StopCloser(&closerSpy{onClose: func() { closed = true }})
		is.True(sf != nil)
		is.NoErr(sf(context.Background()))
		is.Equal(closed, true)
	})
}

type closerSpy struct{ onClose func() }

func (c *closerSpy) Close() error { c.onClose(); return nil }
