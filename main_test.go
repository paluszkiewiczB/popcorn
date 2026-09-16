package popcorn_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the suite and fails it if any goroutine leaked.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
