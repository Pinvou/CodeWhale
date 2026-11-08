package account

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestApprovalAuthorizationCommitRolledBack(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "commit rollback", err: pgx.ErrTxCommitRollback, want: true},
		{name: "wrapped commit rollback", err: errors.Join(errors.New("commit"), pgx.ErrTxCommitRollback), want: true},
		{name: "serialization", err: &pgconn.PgError{Code: "40001"}, want: true},
		{name: "deadlock", err: &pgconn.PgError{Code: "40P01"}, want: true},
		{name: "constraint", err: &pgconn.PgError{Code: "23514"}, want: false},
		{name: "transport unknown", err: errors.New("connection reset during commit"), want: false},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := approvalAuthorizationCommitRolledBack(test.err); got != test.want {
				t.Fatalf("approvalAuthorizationCommitRolledBack() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestApprovalAuthorizationPublicError(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		pgx.ErrTxCommitRollback,
		ErrApprovalAuthorizationControlChanged,
		ErrApprovalAuthorizationInvalid,
		&pgconn.PgError{Code: "42501", Message: "private object detail"},
	} {
		if got := approvalAuthorizationPublicError(context.Background(), err); !errors.Is(got, ErrApprovalWriterUnavailable) {
			t.Fatalf("approvalAuthorizationPublicError(%T) = %v", err, got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := approvalAuthorizationPublicError(ctx, ErrApprovalAuthorizationInvalid); !errors.Is(got, context.Canceled) {
		t.Fatalf("canceled context must win: %v", got)
	}
}
