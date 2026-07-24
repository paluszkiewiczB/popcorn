package popcorn_test

import (
	"context"
	"testing"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

const testModuleID = "test"

func TestNewModule_Valid(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	m, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: testModuleID,
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)
	is.Equal(m.ID(), testModuleID)
	is.Equal(m.Dependencies(), []string(nil))
}

func TestNewModule_EmptyID(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	_, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "",
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.True(err != nil)
}

func TestNewModule_ReservedID(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	_, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: popcorn.EventSourceKernel,
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.True(err != nil)
}

func TestNewModule_NilStart(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	_, err := popcorn.NewModule(popcorn.ModRecipe{ID: testModuleID})
	is.True(err != nil)
}

func TestNewModule_WithDependencies(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	m, err := popcorn.NewModule(popcorn.ModRecipe{
		ID:           testModuleID,
		Dependencies: []string{"dep1", "dep2"},
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)
	is.Equal(m.Dependencies(), []string{"dep1", "dep2"})
}

func TestNewModule_EventReceiver(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	ch := make(chan popcorn.Event, 1)
	m, err := popcorn.NewModule(popcorn.ModRecipe{
		ID:         testModuleID,
		EventsChan: ch,
		Start: func(_ context.Context) (popcorn.StopFunc, error) {
			return func(_ context.Context) error { return nil }, nil
		},
	})
	is.NoErr(err)

	er, ok := m.(popcorn.EventReceiver)
	is.True(ok)
	is.Equal(er.Events(), ch)
}
