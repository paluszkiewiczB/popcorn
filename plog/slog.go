// Package plog exposes Popcorn specific logger interface.
package plog

import (
	"context"
	"log/slog"
)

// ensure compatibility
var _ Slog = (*slog.Logger)(nil)

// Slog is a narrowed-down version of slog.Logger.
type Slog interface {
	LogAttrs(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr)
	With(attrs ...any) *slog.Logger
}
