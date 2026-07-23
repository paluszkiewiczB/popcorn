package main

import (
	"testing"

	"github.com/matryer/is"
)

//CR: what the fuck are those examples supposed to show?

func TestHTTPModule(t *testing.T) {
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
	is := is.New(t)
	mod := NewPingerModule(nil, 3, "dep")
	is.True(mod != nil)

	recipe := mod.ModRecipe()
	is.Equal(recipe.ID, "github.com/paluszkiewiczB/popcorn/examples/PingerModule")
	is.Equal(recipe.Dependencies, []string{"dep"})
	is.True(recipe.Start != nil)
	is.True(mod.Done() != nil)
}
