package opnsense

import (
	"context"
	"fmt"
	"log/slog"
)

// logWrap emits an ErrorContext event and returns the wrapped error.
// All RPC and Transfer client wrappers funnel error returns through
// this helper so the staticcheck-extra slogged-wrap rule is satisfied
// and every external error reaches the structured log.
func logWrap(ctx context.Context, log *slog.Logger, op string, err error) error {
	if log == nil {
		log = slog.Default()
	}
	log.ErrorContext(ctx, "opnsense: "+op, "err", err)
	return fmt.Errorf("opnsense: %s: %w", op, err)
}
