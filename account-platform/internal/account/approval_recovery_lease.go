package account

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrApprovalRecoveryLeaseUnavailable = errors.New("approval recovery query lease unavailable")

// RecoveryQueryLease permits one bounded read-only investigation of the
// original execution. It never grants another domain attempt or a terminal
// Account transition. Token and version must remain private to the worker.
type RecoveryQueryLease struct {
	RequestID  uuid.UUID
	Token      uuid.UUID
	Until      time.Time
	QueryCount int
	Version    int64
}

type recoveryLeaseQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// RecoveryQueryLeaseStore must be constructed with a connection authenticated
// as account_approval_recovery. The database functions independently verify
// session_user and the original execution binding on every claim.
type RecoveryQueryLeaseStore struct {
	db recoveryLeaseQuerier
}

func NewRecoveryQueryLeaseStore(db recoveryLeaseQuerier) (*RecoveryQueryLeaseStore, error) {
	if db == nil {
		return nil, ErrApprovalRecoveryPlanInvalid
	}
	return &RecoveryQueryLeaseStore{db: db}, nil
}

func (s *RecoveryQueryLeaseStore) Claim(ctx context.Context, candidate ApprovalRecoveryCandidate) (RecoveryQueryLease, error) {
	if s == nil || s.db == nil || candidate.RequestID == uuid.Nil || candidate.ExecutionID == uuid.Nil ||
		candidate.OriginalEventID == uuid.Nil || candidate.Durable == "" || candidate.ApprovalEpoch < 1 || candidate.RequestVersion < 1 {
		return RecoveryQueryLease{}, ErrApprovalRecoveryPlanInvalid
	}
	lease := RecoveryQueryLease{RequestID: candidate.RequestID}
	err := s.db.QueryRow(ctx, `SELECT lease_token,lease_until,query_count,version FROM public.claim_approval_recovery_query($1,$2,$3,$4,$5,$6)`,
		candidate.RequestID, candidate.ExecutionID, candidate.OriginalEventID, candidate.Durable,
		candidate.ApprovalEpoch, candidate.RequestVersion).Scan(&lease.Token, &lease.Until, &lease.QueryCount, &lease.Version)
	if err != nil {
		return RecoveryQueryLease{}, classifyRecoveryLeaseError(err)
	}
	if lease.Token == uuid.Nil || !lease.Until.After(time.Now()) || lease.QueryCount < 1 || lease.Version < 1 {
		return RecoveryQueryLease{}, ErrApprovalRecoveryPlanInvalid
	}
	lease.Until = lease.Until.UTC()
	return lease, nil
}

func (s *RecoveryQueryLeaseStore) Release(ctx context.Context, lease RecoveryQueryLease) (time.Time, error) {
	if s == nil || s.db == nil || lease.RequestID == uuid.Nil || lease.Token == uuid.Nil || lease.Version < 1 {
		return time.Time{}, ErrApprovalRecoveryPlanInvalid
	}
	var next time.Time
	var version int64
	err := s.db.QueryRow(ctx, `SELECT next_query_at,version FROM public.release_approval_recovery_query($1,$2,$3)`,
		lease.RequestID, lease.Token, lease.Version).Scan(&next, &version)
	if err != nil {
		return time.Time{}, classifyRecoveryLeaseError(err)
	}
	if version != lease.Version+1 || next.IsZero() {
		return time.Time{}, ErrApprovalRecoveryPlanInvalid
	}
	return next.UTC(), nil
}

func classifyRecoveryLeaseError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55000" {
		return ErrApprovalRecoveryLeaseUnavailable
	}
	return err
}
