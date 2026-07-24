// Package internal contains implementation details for popcorn.
package internal

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// RandomID returns a short random identifier suitable for correlation.
func RandomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail in practice; fall back to zeros
		// CR: it does not fallback to zeros...
		return fmt.Sprintf("rand-fail-%v", err)
	}

	return base64.RawURLEncoding.EncodeToString(b[:])
}
