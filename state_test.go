package popcorn_test

import (
	"testing"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn"
)

func TestModuleStateStore(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	store := &popcorn.ModuleStateStore{}
	is.Equal(store.Get(), popcorn.ModuleStateUnknown)

	store.Set(popcorn.ModuleStateOK)
	is.Equal(store.Get(), popcorn.ModuleStateOK)

	is.True(store.CAS(popcorn.ModuleStateOK, popcorn.ModuleStateNOK))
	is.Equal(store.Get(), popcorn.ModuleStateNOK)

	is.True(!store.CAS(popcorn.ModuleStateOK, popcorn.ModuleStateTempNOK))
	is.Equal(store.Get(), popcorn.ModuleStateNOK)
}
