package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ApprovalAuthorizationBackfillReadiness struct {
	AuthorizationCount  int64
	MissingEpochCount   int64
	MissingSidecarCount int64
	InvalidSidecarCount int64
	HighWaterOccurredAt *time.Time
	HighWaterEventID    *uuid.UUID
}

type approvalAuthorizationBackfillCandidate struct {
	EventID                            uuid.UUID
	EventType                          string
	AggregateID                        uuid.UUID
	AggregateVersion                   int64
	Payload                            json.RawMessage
	TraceID                            string
	OccurredAt                         time.Time
	PublishedAt                        *time.Time
	ApprovalControlEpoch               *int64
	SidecarSerializationVersion        *string
	SidecarProvenance                  *string
	SidecarDeliveryBytes               []byte
	SidecarEventDigest                 *string
	SidecarReconstructedSemanticDigest *string
}

func ReadApprovalAuthorizationBackfillReadiness(ctx context.Context, pool *pgxpool.Pool) (ApprovalAuthorizationBackfillReadiness, error) {
	if err := ApprovalBackfillReady(ctx, pool); err != nil {
		return ApprovalAuthorizationBackfillReadiness{}, err
	}
	var readiness ApprovalAuthorizationBackfillReadiness
	if err := pool.QueryRow(ctx, `SELECT * FROM account_approval_authorization.authorization_backfill_readiness()`).Scan(
		&readiness.AuthorizationCount, &readiness.MissingEpochCount, &readiness.MissingSidecarCount,
		&readiness.InvalidSidecarCount, &readiness.HighWaterOccurredAt, &readiness.HighWaterEventID,
	); err != nil {
		return ApprovalAuthorizationBackfillReadiness{}, ErrApprovalBackfillUnavailable
	}
	if (readiness.HighWaterOccurredAt == nil) != (readiness.HighWaterEventID == nil) {
		return ApprovalAuthorizationBackfillReadiness{}, ErrApprovalBackfillUnavailable
	}
	return readiness, nil
}

// BackfillApprovalAuthorizationEvents processes only the high-water snapshot
// observed at entry. It never chases newly appended events indefinitely.
func BackfillApprovalAuthorizationEvents(ctx context.Context, pool *pgxpool.Pool, batchLimit int) (ApprovalAuthorizationBackfillReadiness, error) {
	if batchLimit < 1 || batchLimit > 200 {
		return ApprovalAuthorizationBackfillReadiness{}, ErrApprovalBackfillUnavailable
	}
	initial, err := ReadApprovalAuthorizationBackfillReadiness(ctx, pool)
	if err != nil || initial.HighWaterOccurredAt == nil {
		return initial, err
	}
	var afterOccurredAt *time.Time
	var afterEventID *uuid.UUID
	for {
		candidates, err := loadApprovalAuthorizationBackfillCandidates(
			ctx, pool, afterOccurredAt, afterEventID, *initial.HighWaterOccurredAt, *initial.HighWaterEventID, batchLimit,
		)
		if err != nil {
			return ApprovalAuthorizationBackfillReadiness{}, err
		}
		if len(candidates) == 0 {
			break
		}
		for _, candidate := range candidates {
			if err := backfillApprovalAuthorizationEvent(ctx, pool, candidate); err != nil {
				return ApprovalAuthorizationBackfillReadiness{}, err
			}
		}
		last := candidates[len(candidates)-1]
		afterOccurredAt, afterEventID = &last.OccurredAt, &last.EventID
	}
	return ReadApprovalAuthorizationBackfillReadiness(ctx, pool)
}

// ValidateApprovalAuthorizationForQuiescing acquires the control FOR UPDATE
// lock in caller-owned tx, then reconstructs and dual-decodes every row through
// a migrator-only read seam in that same Serializable snapshot. The later
// cutover owner retains that lock for its CAS and owns commit/rollback.
func ValidateApprovalAuthorizationForQuiescing(
	ctx context.Context,
	tx pgx.Tx,
	expectedControlVersion int64,
	batchLimit int,
) (ApprovalAuthorizationBackfillReadiness, error) {
	if tx == nil || batchLimit < 1 || batchLimit > 200 || expectedControlVersion < 1 {
		return ApprovalAuthorizationBackfillReadiness{}, ErrApprovalBackfillUnavailable
	}
	var checked ApprovalAuthorizationBackfillReadiness
	if err := tx.QueryRow(ctx, `SELECT * FROM account_approval_authorization.validate_authorization_outbox_for_quiescing($1)`, expectedControlVersion).Scan(
		&checked.AuthorizationCount, &checked.MissingEpochCount, &checked.MissingSidecarCount, &checked.InvalidSidecarCount,
	); err != nil {
		return checked, approvalBackfillPublicError(ctx)
	}
	var readiness ApprovalAuthorizationBackfillReadiness
	var afterOccurredAt *time.Time
	var afterEventID *uuid.UUID
	for {
		candidates, loadErr := loadApprovalAuthorizationValidationCandidates(ctx, tx, afterOccurredAt, afterEventID, batchLimit)
		if loadErr != nil {
			return readiness, loadErr
		}
		if len(candidates) == 0 {
			break
		}
		for _, candidate := range candidates {
			if validateErr := validateApprovalAuthorizationCandidate(candidate); validateErr != nil {
				return readiness, validateErr
			}
			readiness.AuthorizationCount++
		}
		last := candidates[len(candidates)-1]
		afterOccurredAt, afterEventID = &last.OccurredAt, &last.EventID
	}
	if checked.AuthorizationCount != readiness.AuthorizationCount || checked.MissingEpochCount != 0 || checked.MissingSidecarCount != 0 || checked.InvalidSidecarCount != 0 {
		return checked, ErrApprovalBackfillUnavailable
	}
	checked.HighWaterOccurredAt, checked.HighWaterEventID = afterOccurredAt, afterEventID
	return checked, nil
}

func loadApprovalAuthorizationValidationCandidates(ctx context.Context, tx pgx.Tx, afterOccurredAt *time.Time, afterEventID *uuid.UUID, limit int) ([]approvalAuthorizationBackfillCandidate, error) {
	rows, err := tx.Query(ctx, `SELECT * FROM account_approval_authorization.authorization_validation_candidates($1,$2,$3)`, afterOccurredAt, afterEventID, limit)
	if err != nil {
		return nil, approvalBackfillPublicError(ctx)
	}
	defer rows.Close()
	items := make([]approvalAuthorizationBackfillCandidate, 0, limit)
	for rows.Next() {
		var item approvalAuthorizationBackfillCandidate
		if err := rows.Scan(&item.EventID, &item.EventType, &item.AggregateID, &item.AggregateVersion,
			&item.Payload, &item.TraceID, &item.OccurredAt, &item.PublishedAt, &item.ApprovalControlEpoch,
			&item.SidecarSerializationVersion, &item.SidecarProvenance, &item.SidecarDeliveryBytes,
			&item.SidecarEventDigest, &item.SidecarReconstructedSemanticDigest); err != nil {
			return nil, approvalBackfillPublicError(ctx)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, approvalBackfillPublicError(ctx)
	}
	return items, nil
}

func loadApprovalAuthorizationBackfillCandidates(
	ctx context.Context,
	pool *pgxpool.Pool,
	afterOccurredAt *time.Time,
	afterEventID *uuid.UUID,
	highWaterOccurredAt time.Time,
	highWaterEventID uuid.UUID,
	limit int,
) ([]approvalAuthorizationBackfillCandidate, error) {
	rows, err := pool.Query(ctx, `SELECT * FROM account_approval_authorization.authorization_backfill_candidates($1,$2,$3,$4,$5)`,
		afterOccurredAt, afterEventID, highWaterOccurredAt, highWaterEventID, limit)
	if err != nil {
		return nil, ErrApprovalBackfillUnavailable
	}
	defer rows.Close()
	items := make([]approvalAuthorizationBackfillCandidate, 0, limit)
	for rows.Next() {
		var item approvalAuthorizationBackfillCandidate
		if err := rows.Scan(&item.EventID, &item.EventType, &item.AggregateID, &item.AggregateVersion,
			&item.Payload, &item.TraceID, &item.OccurredAt, &item.PublishedAt, &item.ApprovalControlEpoch,
			&item.SidecarSerializationVersion, &item.SidecarProvenance, &item.SidecarDeliveryBytes,
			&item.SidecarEventDigest, &item.SidecarReconstructedSemanticDigest); err != nil {
			return nil, ErrApprovalBackfillUnavailable
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrApprovalBackfillUnavailable
	}
	return items, nil
}

func backfillApprovalAuthorizationEvent(ctx context.Context, pool *pgxpool.Pool, candidate approvalAuthorizationBackfillCandidate) error {
	deliveryBytes, digestText, err := reconstructLegacyApprovalAuthorizationCandidate(candidate)
	if err != nil {
		return err
	}
	if candidate.SidecarSerializationVersion != nil {
		return validateApprovalAuthorizationSidecar(candidate, deliveryBytes, digestText)
	}
	storedBytes := deliveryBytes
	var semanticDigest *string
	if candidate.PublishedAt != nil {
		semanticDigest = &digestText
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return approvalAuthorizationBackfillRowError(ctx, candidate, err, false)
	}
	defer func() {
		rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackContext)
	}()
	if _, err := tx.Exec(ctx, `SELECT account_approval_authorization.backfill_authorization_sidecar($1,$2,$3)`,
		candidate.EventID, storedBytes, semanticDigest); err != nil {
		return approvalAuthorizationBackfillRowError(ctx, candidate, err, false)
	}
	if err := tx.Commit(ctx); err != nil {
		var postgresErr *pgconn.PgError
		if errors.As(err, &postgresErr) || errors.Is(err, pgx.ErrTxCommitRollback) {
			return newApprovalAuthorizationBackfillDiagnostic(candidate, "ROW_WRITE_REJECTED")
		}
		provenance := "RECONSTRUCTED_FOR_DELIVERY"
		if candidate.PublishedAt != nil {
			provenance = "RECONSTRUCTED_AFTER_PUBLISH"
		}
		var committed bool
		verificationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		verificationErr := pool.QueryRow(verificationContext,
			`SELECT account_approval_authorization.authorization_backfill_outcome($1,$2,$3)`,
			candidate.EventID, provenance, digestText).Scan(&committed)
		if verificationErr == nil {
			if committed {
				return nil
			}
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return newApprovalAuthorizationBackfillDiagnostic(candidate, "ROW_WRITE_OUTCOME_UNKNOWN")
	}
	return nil
}

func validateApprovalAuthorizationCandidate(candidate approvalAuthorizationBackfillCandidate) error {
	deliveryBytes, digestText, err := reconstructLegacyApprovalAuthorizationCandidate(candidate)
	if err != nil {
		return err
	}
	if candidate.ApprovalControlEpoch == nil || *candidate.ApprovalControlEpoch != 1 || candidate.SidecarSerializationVersion == nil {
		return approvalAuthorizationBlocker(candidate, "MISSING_IMMUTABLE_EVIDENCE", digestText)
	}
	return validateApprovalAuthorizationSidecar(candidate, deliveryBytes, digestText)
}

func reconstructLegacyApprovalAuthorizationCandidate(candidate approvalAuthorizationBackfillCandidate) ([]byte, string, error) {
	if candidate.EventType != approvalAuthorizationEventType || candidate.EventID == uuid.Nil || candidate.AggregateID == uuid.Nil || candidate.AggregateVersion < 1 {
		return nil, "", approvalAuthorizationBlocker(candidate, "INVALID_EVENT_IDENTITY", "")
	}
	deliveryBytes, err := approvalencoding.EncodeLegacyAuthorizationEvent(events.Event{
		ID: candidate.EventID, Type: candidate.EventType, AggregateID: candidate.AggregateID,
		AggregateVersion: candidate.AggregateVersion, Payload: candidate.Payload,
		TraceID: candidate.TraceID, OccurredAt: candidate.OccurredAt,
	})
	if err != nil {
		return nil, "", approvalAuthorizationBlocker(candidate, "LEGACY_RECONSTRUCTION_FAILED", "")
	}
	if _, err := approvalencoding.DecodeAuthorizationEvent(deliveryBytes, approvalencoding.ExpectedAuthorizationBinding{
		ApprovedRequestVersion: candidate.AggregateVersion,
		ApprovalEpoch:          1,
	}); err != nil {
		return nil, "", approvalAuthorizationBlocker(candidate, "LEGACY_CLOSED_DECODE_FAILED", "")
	}
	digest := sha256.Sum256(deliveryBytes)
	digestText := hex.EncodeToString(digest[:])
	return deliveryBytes, digestText, nil
}

func validateApprovalAuthorizationSidecar(candidate approvalAuthorizationBackfillCandidate, deliveryBytes []byte, digestText string) error {
	if candidate.ApprovalControlEpoch == nil || *candidate.ApprovalControlEpoch != 1 || candidate.SidecarSerializationVersion == nil || *candidate.SidecarSerializationVersion != approvalencoding.LegacySerializationVersion || candidate.SidecarProvenance == nil {
		return approvalAuthorizationBlocker(candidate, "SIDECAR_BINDING_MISMATCH", digestText)
	}
	switch *candidate.SidecarProvenance {
	case "FROZEN_AT_APPROVAL", "RECONSTRUCTED_FOR_DELIVERY":
		if !bytes.Equal(candidate.SidecarDeliveryBytes, deliveryBytes) || candidate.SidecarEventDigest == nil || *candidate.SidecarEventDigest != digestText || candidate.SidecarReconstructedSemanticDigest != nil {
			return approvalAuthorizationBlocker(candidate, "DELIVERY_EVIDENCE_MISMATCH", digestText)
		}
	case "RECONSTRUCTED_AFTER_PUBLISH":
		if candidate.PublishedAt == nil || len(candidate.SidecarDeliveryBytes) != 0 || candidate.SidecarEventDigest != nil || candidate.SidecarReconstructedSemanticDigest == nil || *candidate.SidecarReconstructedSemanticDigest != digestText {
			return approvalAuthorizationBlocker(candidate, "SEMANTIC_EVIDENCE_MISMATCH", digestText)
		}
	default:
		return approvalAuthorizationBlocker(candidate, "UNKNOWN_PROVENANCE", digestText)
	}
	return nil
}

func approvalAuthorizationBlocker(candidate approvalAuthorizationBackfillCandidate, reason, digest string) error {
	return newApprovalAuthorizationBackfillDiagnostic(candidate, reason)
}

func approvalAuthorizationBackfillRowError(ctx context.Context, candidate approvalAuthorizationBackfillCandidate, err error, duringCommit bool) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if duringCommit {
		var postgresErr *pgconn.PgError
		if !errors.As(err, &postgresErr) && !errors.Is(err, pgx.ErrTxCommitRollback) {
			return newApprovalAuthorizationBackfillDiagnostic(candidate, "ROW_WRITE_OUTCOME_UNKNOWN")
		}
	}
	return newApprovalAuthorizationBackfillDiagnostic(candidate, "ROW_WRITE_REJECTED")
}

func approvalBackfillPublicError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrApprovalBackfillUnavailable
}

func approvalAuthorizationBackfillUnavailable(err error) bool {
	return err != nil && (errors.Is(err, ErrApprovalBackfillUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
