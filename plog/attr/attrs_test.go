package attr_test

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/matryer/is"
	"github.com/paluszkiewiczB/popcorn/plog/attr"
)

var errBoom = errors.New("boom")

func TestErr(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	a := attr.Err(errBoom)
	is.Equal(a.Key, "err")
}

func TestNamedErr(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	a := attr.NamedErr("cause", errBoom)
	is.Equal(a.Key, "cause")
}

func TestNamedErr_Nil(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	a := attr.NamedErr("cause", nil)
	is.Equal(a, slog.Attr{})
}

func TestModID(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	a := attr.ModID("foo")
	is.Equal(a.Key, "mod")
	is.Equal(a.Value.String(), "foo")
}

func TestStrings(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	a := attr.Strings("items", []string{"a", "b", "c"})
	is.Equal(a.Key, "items")
	is.Equal(a.Value.String(), "a, b, c")
}

func TestTypeOf(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	a := attr.TypeOf("type", errBoom)
	is.Equal(a.Key, "type")
	is.True(a.Value.String() != "")
}

func TestTypeOf_Nil(t *testing.T) {
	t.Parallel()
	is := is.New(t)

	var p *int

	a := attr.TypeOf("type", p)
	is.Equal(a.Value.String(), "nil")
}

func TestErr_Nil(t *testing.T) {
	t.Parallel()
	is := is.New(t)
	a := attr.Err(nil)
	is.Equal(a.Key, "err")
}
