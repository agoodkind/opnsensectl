// Package cli prints command help, starts goroutines, and reconnects
// RPC clients.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

var errRecoveredPanic = errors.New("panic")

// Spawn starts a function in a goroutine and logs recovered panics.
//
// The caller supplies the context and logger for the panic record.
//
// For example:
//
//	Spawn(ctx, logger, "worker", work)
func Spawn(ctx context.Context, log *slog.Logger, where string, work func()) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(
					ctx,
					"opnsense drain: goroutine panic",
					"where",
					where,
					"panic",
					recovered,
					"err",
					fmt.Errorf("%w: %v", errRecoveredPanic, recovered),
				)
			}
		}()
		work()
	}()
}
