package attr

import (
	"fmt"
	"log/slog"
	"math/rand"
	"reflect"
)

func Err(err error) slog.Attr {
	return slog.String("err", err.Error())
}

func NamedErr(key string, err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}

	return slog.String(key, err.Error())
}

func Strings(key string, strings []string) slog.Attr {
	return slog.String(key, fmt.Sprintf("%v", strings))
}

func ModId(id string) slog.Attr {
	return slog.String("mod", id)
}

func RandomID(key string) slog.Attr {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var id [10]uint8
	for i := 0; i < 10; i++ {
		id[i] = charset[rand.Intn(len(charset))]
	}

	return slog.String(key, string(id[:]))
}

func TypeOf[T any](key string, val T) slog.Attr {
	return slog.String(key, typeName(val))
}

func typeName[T any](val T) string {
	t := reflect.TypeOf(val)
	if t == nil {
		return "nil"
	}

	return t.Name()
}

func Chan[T any](c chan T) slog.Attr {
	return NamedChan("chan", c)
}

func NamedChan[T any](key string, c chan T) slog.Attr {
	return slog.String(key, fmt.Sprintf("%#v", c))
}
