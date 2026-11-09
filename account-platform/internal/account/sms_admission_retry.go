package account

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// A provider fence can abort a transaction whose SERIALIZABLE snapshot predates
// a committed send. Retry only when PostgreSQL confirms the whole transaction
// aborted. The caller has not invoked the SMS provider at this point.
func retrySMSAdmission[T any](ctx context.Context, run func() (T, error)) (T, error) {
	const maximumAttempts = 3
	for attempt := 1; ; attempt++ {
		result, err := run()
		var postgresError *pgconn.PgError
		if attempt == maximumAttempts || ctx.Err() != nil || !errors.As(err, &postgresError) || postgresError.Code != "40001" {
			return result, err
		}
	}
}
