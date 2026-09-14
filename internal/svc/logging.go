package svc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

func loggerOrDefault(log *slog.Logger) *slog.Logger {
	if log != nil {
		return log
	}
	return slog.Default()
}

func logWrappedErrorContext(
	ctx context.Context,
	log *slog.Logger,
	logMessage string,
	wrapMessage string,
	err error,
	attrs ...slog.Attr,
) error {
	logger := loggerOrDefault(log)
	for _, attr := range attrs {
		logger = logger.With(attr)
	}
	logger.ErrorContext(ctx, logMessage, "err", err)
	return fmt.Errorf("%s: %w", wrapMessage, err)
}

func logCloseErrorContext(
	ctx context.Context,
	log *slog.Logger,
	closer io.Closer,
	message string,
	attrs ...slog.Attr,
) {
	if err := closer.Close(); err != nil {
		loggerOrDefault(log).LogAttrs(
			ctx,
			slog.LevelWarn,
			message,
			append(attrs, slog.Any("err", err))...,
		)
	}
}
