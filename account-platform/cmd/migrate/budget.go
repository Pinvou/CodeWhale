package main

import (
	"context"
	"errors"
	"time"
)

const defaultMigrationTimeout = "2m"
const maximumMigrationTimeout = 24 * time.Hour

// The flag parser chooses the flag over the environment before validation, so
// an explicit valid flag can override a malformed environment value.
func migrationTimeout(raw string) (time.Duration, error) {
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 || duration > maximumMigrationTimeout {
		return 0, errors.New("migration timeout must be greater than zero and at most 24h")
	}
	return duration, nil
}

// Do not log driver errors: they can contain database URLs or SQL values. A
// deadline means the result must be inspected, not that every version rolled back.
func migrationFailureCode(ctx context.Context, err error, fallback string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "MIGRATION_DEADLINE_EXCEEDED"
	}
	return fallback
}
