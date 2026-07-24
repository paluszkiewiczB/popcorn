// Package attr exposes common slog attributes used by popcorn.
package attr

import (
	"log/slog"
	"reflect"
	"strings"
)

// Err returns a slog attribute that preserves the full error chain.
func Err(err error) slog.Attr {
	return slog.Any("err", err)
}

// NamedErr returns a slog attribute with the error under the given key.
func NamedErr(key string, err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}

	return slog.Any(key, err)
}

// Strings returns a slog attribute with the string slice under the given key.
func Strings(key string, values []string) slog.Attr {
	return slog.String(key, strings.Join(values, ", "))
}

// ModID returns a slog attribute with the module id under the key "mod".
func ModID(id string) slog.Attr {
	return slog.String("mod", id)
}

// TypeOf returns a slog attribute with the fully qualified type name of the value.
func TypeOf[T any](key string, val T) slog.Attr {
	return slog.String(key, typeName(val))
}

func typeName[T any](val T) string {
	v := reflect.ValueOf(val)
	if !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return "nil"
	}

	t := v.Type()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	name := t.Name()
	if pkg := t.PkgPath(); pkg != "" {
		name = pkg + "." + name
	}

	return name
}
