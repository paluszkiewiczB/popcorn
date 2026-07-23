package attr_test

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn/plog/attr"
)

func TestErr(t *testing.T) {
	is := is.New(t)
	a := attr.Err(errors.New("boom"))
	is.Equal(a.Key, "err")
}

func TestNamedErr(t *testing.T) {
	is := is.New(t)
	a := attr.NamedErr("cause", errors.New("boom"))
	is.Equal(a.Key, "cause")
}

func TestNamedErr_Nil(t *testing.T) {
	is := is.New(t)
	a := attr.NamedErr("cause", nil)
	is.Equal(a, slog.Attr{})
}

func TestModID(t *testing.T) {
	is := is.New(t)
	a := attr.ModID("foo")
	is.Equal(a.Key, "mod")
	is.Equal(a.Value.String(), "foo")
}

func TestStrings(t *testing.T) {
	is := is.New(t)
	a := attr.Strings("items", []string{"a", "b", "c"})
	is.Equal(a.Key, "items")
	is.Equal(a.Value.String(), "a, b, c")
}

func TestTypeOf(t *testing.T) {
	is := is.New(t)
	a := attr.TypeOf("type", errors.New("boom"))
	is.Equal(a.Key, "type")
	is.True(a.Value.String() != "")
}

func TestTypeOf_Nil(t *testing.T) {
	is := is.New(t)
	var p *int
	a := attr.TypeOf("type", p)
	is.Equal(a.Value.String(), "nil")
}

func TestErr_Nil(t *testing.T) {
	is := is.New(t)
	a := attr.Err(nil)
	is.Equal(a.Key, "err")
}
