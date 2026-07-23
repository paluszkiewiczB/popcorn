package popcorn

import (
	"testing"

	"github.com/matryer/is"
)

func TestModuleStateStore(t *testing.T) {
	is := is.New(t)

	store := &moduleStateStore{}
	is.Equal(store.Get(), ModuleStateUnknown)

	store.Set(ModuleStateOK)
	is.Equal(store.Get(), ModuleStateOK)

	is.True(store.CAS(ModuleStateOK, ModuleStateNOK))
	is.Equal(store.Get(), ModuleStateNOK)

	is.True(!store.CAS(ModuleStateOK, ModuleStateTempNOK))
	is.Equal(store.Get(), ModuleStateNOK)
}
