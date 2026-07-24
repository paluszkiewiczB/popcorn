package internal_test

import (
	"testing"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn/internal"
)

func TestRandomID(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	id1 := internal.RandomID()
	id2 := internal.RandomID()

	is.True(id1 != "")
	is.True(id1 != id2)
}

func TestRandomID_Length(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	id := internal.RandomID()
	is.True(id != "")
}
