package token

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestF05CancellationKeyConflictExactConstraint(t *testing.T) {
	conflict := func(code, table, constraint string) error {
		return &pgconn.PgError{Code: code, TableName: table, ConstraintName: constraint}
	}
	const table = "usage_cancel_commands"
	const keyConstraint = "usage_cancel_commands_user_id_idempotency_key_digest_key"
	for _, sample := range []struct {
		name string
		err  error
		want bool
	}{
		{"actual-key-constraint", conflict("23505", table, keyConstraint), true},
		{"wrapped-actual-key-constraint", fmt.Errorf("controlled wrapper: %w", conflict("23505", table, keyConstraint)), true},
		{"other-unique-resource", conflict("23505", table, "usage_cancel_commands_usage_request_id_key"), false},
		{"other-unique-primary-key", conflict("23505", table, "usage_cancel_commands_pkey"), false},
		{"another-table", conflict("23505", "usage_requests", keyConstraint), false},
		{"foreign-key", conflict("23503", table, keyConstraint), false},
		{"missing-table", conflict("23505", "", keyConstraint), false},
		{"missing-constraint", conflict("23505", table, ""), false},
		{"infrastructure", errors.New("controlled infrastructure failure"), false},
		{"nil", nil, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			if got := isCancellationKeyConflict(sample.err); got != sample.want {
				t.Fatalf("exact cancellation constraint classification=%t want=%t", got, sample.want)
			}
		})
	}
}
