// Package debuglog provides opt-in structured debug logging carried through
// context.Context. It is deliberately silent unless the CLI global verbose
// flag installs a logger.
package debuglog

import (
	"context"
	"io"
	"log/slog"
)

type contextKey struct{}

// With returns a context that writes debug records to w.
func With(ctx context.Context, w io.Writer) context.Context {
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return context.WithValue(ctx, contextKey{}, logger)
}

// Debug emits a debug record when verbose logging is enabled in ctx.
func Debug(ctx context.Context, message string, args ...any) {
	if logger, ok := ctx.Value(contextKey{}).(*slog.Logger); ok {
		logger.DebugContext(ctx, message, args...)
	}
}
