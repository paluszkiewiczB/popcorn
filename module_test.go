package popcorn_test

import (
	"context"
	"testing"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

func TestNewModule_Valid(t *testing.T) {
	is := is.New(t)
	m, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "test",
		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
			return nil, nil
		},
	})
	is.NoErr(err)
	is.Equal(m.ID(), "test")
	is.Equal(m.Dependencies(), []string(nil))
}

func TestNewModule_EmptyID(t *testing.T) {
	is := is.New(t)
	_, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: "",
		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
			return nil, nil
		},
	})
	is.True(err != nil)
}

func TestNewModule_ReservedID(t *testing.T) {
	is := is.New(t)
	_, err := popcorn.NewModule(popcorn.ModRecipe{
		ID: popcorn.EventSourceKernel,
		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
			return nil, nil
		},
	})
	is.True(err != nil)
}

func TestNewModule_NilStart(t *testing.T) {
	is := is.New(t)
	_, err := popcorn.NewModule(popcorn.ModRecipe{ID: "test"})
	is.True(err != nil)
}

func TestNewModule_WithDependencies(t *testing.T) {
	is := is.New(t)
	m, err := popcorn.NewModule(popcorn.ModRecipe{
		ID:           "test",
		Dependencies: []string{"dep1", "dep2"},
		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
			return nil, nil
		},
	})
	is.NoErr(err)
	is.Equal(m.Dependencies(), []string{"dep1", "dep2"})
}

func TestNewModule_EventReceiver(t *testing.T) {
	is := is.New(t)
	ch := make(chan popcorn.Event, 1)
	m, err := popcorn.NewModule(popcorn.ModRecipe{
		ID:         "test",
		EventsChan: ch,
		Start: func(ctx context.Context) (popcorn.StopFunc, error) {
			return nil, nil
		},
	})
	is.NoErr(err)

	er, ok := m.(popcorn.EventReceiver)
	is.True(ok)
	is.Equal(er.Events(), ch)
}
