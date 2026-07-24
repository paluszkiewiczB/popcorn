package main

// CR: what the fuck are those examples supposed to show?

import (
	"testing"

	"github.com/matryer/is"
)

func TestHTTPModule(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	mod := NewHTTPModule(HTTPConfig{
		Handler: echoHandler(),
	})
	is.True(mod != nil)

	recipe := mod.ModRecipe()
	is.Equal(recipe.ID, "github.com/paluszkiewiczB/popcorn/examples/HTTPModule")
	is.True(recipe.Start != nil)
}

func TestPingerModule(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	mod := NewPingerModule(nil, 3, "dep")
	is.True(mod != nil)

	recipe := mod.ModRecipe()
	is.Equal(recipe.ID, "github.com/paluszkiewiczB/popcorn/examples/PingerModule")
	is.Equal(recipe.Dependencies, []string{"dep"})
	is.True(recipe.Start != nil)
	is.True(mod.Done() != nil)
}
