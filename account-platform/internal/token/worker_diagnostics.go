package token

import (
	"context"
	"errors"
	"log/slog"
)

func reportTokenWorkerFailure(ctx context.Context, event, stage string, err error) {
	if ctx.Err() != nil {
		return
	}
	code := "WORKER_OPERATION_FAILED"
	switch {
	case err == nil:
		code = "WORKER_UNEXPECTED_RESULT"
	case errors.Is(err, context.DeadlineExceeded):
		code = "WORKER_TIMEOUT"
	case errors.Is(err, context.Canceled):
		code = "WORKER_CANCELED"
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrNotFound):
		code = "WORKER_STATE_REJECTED"
	}
	slog.WarnContext(ctx, event, "stage", stage, "error_code", code)
}
