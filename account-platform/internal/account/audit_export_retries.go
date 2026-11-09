package account

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Preserve the database origin of transport errors without exposing driver
// messages. Local encryption, CSV and quota errors keep their original class.
type auditExportDatabaseError struct{ cause error }

func (err *auditExportDatabaseError) Error() string { return "audit export database operation failed" }
func (err *auditExportDatabaseError) Unwrap() error { return err.cause }

func wrapAuditExportDatabaseError(err error) error {
	if err == nil {
		return nil
	}
	return &auditExportDatabaseError{cause: err}
}

func transientAuditExportDatabaseError(err error) bool {
	var database *pgconn.PgError
	if errors.As(err, &database) {
		switch database.Code {
		case "40001", "40P01", "53300", "55P03", "57P01", "57P02", "57P03":
			return true
		default:
			return strings.HasPrefix(database.Code, "08")
		}
	}
	var operation *auditExportDatabaseError
	if !errors.As(err, &operation) {
		return false
	}
	var transport net.Error
	return errors.Is(operation.cause, io.EOF) || pgconn.Timeout(operation.cause) || errors.As(operation.cause, &transport)
}

func (s *AuditExportService) retryGeneration(ctx context.Context, job claimedAuditExport) error {
	if job.Attempt >= auditExportMaximumAttempts {
		return s.fail(ctx, job, "RETRY_EXHAUSTED")
	}
	tx, err := s.repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now, err := requireAuditExportLease(ctx, tx, job)
	if err != nil {
		return err
	}
	// PENDING has no execution owner. Its existing claim deadline delays the
	// next attempt while retaining the original file key and quota identity.
	next := now.Add(auditExportRetryDelay)
	if _, err := tx.Exec(ctx, `UPDATE audit_export_jobs SET status='PENDING',lease_until=$2,error_code='GENERATION_RETRY_PENDING',updated_at=$3 WHERE id=$1`, job.ID, next, now); err != nil {
		return err
	}
	if err := appendAuditExportAudit(ctx, tx, job.ActorUserID, "admin.audit.export.retry.scheduled", job.ID, "SUCCEEDED", map[string]any{"attempt": job.Attempt, "maximum_attempts": auditExportMaximumAttempts, "retry_not_before": next, "error_code": "TRANSIENT_DATABASE_ERROR"}, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Crash recovery consumes the same persisted attempt budget. A live generator
// can finish, while an expired third attempt is terminalized under its row lock.
func (s *AuditExportService) exhaustAttempts(ctx context.Context) error {
	for index := 0; index < 4; index++ {
		tx, err := s.repository.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		var job claimedAuditExport
		var status string
		var until *time.Time
		err = tx.QueryRow(ctx, `SELECT id,actor_user_id,attempts,status,lease_until FROM audit_export_jobs
WHERE attempts >= $1 AND (status='PENDING' OR (status='GENERATING' AND (lease_until IS NULL OR lease_until<=clock_timestamp())))
ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, auditExportMaximumAttempts).Scan(&job.ID, &job.ActorUserID, &job.Attempt, &status, &until)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if status == "GENERATING" && until != nil && until.After(now) {
			if err := tx.Rollback(ctx); err != nil {
				return err
			}
			continue
		}
		if err := finishAuditExportFailure(ctx, tx, job, "RETRY_EXHAUSTED", now); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func finishAuditExportFailure(ctx context.Context, tx pgx.Tx, job claimedAuditExport, code string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE audit_export_jobs SET status='FAILED',wrapped_file_key_nonce=NULL,wrapped_file_key=NULL,file_payload=NULL,lease_until=NULL,error_code=$2,completed_at=$3,updated_at=$3 WHERE id=$1`, job.ID, code, now); err != nil {
		return err
	}
	details := map[string]any{"error_code": code}
	if code == "RETRY_EXHAUSTED" {
		details["attempts"] = job.Attempt
		details["maximum_attempts"] = auditExportMaximumAttempts
	}
	if err := appendAuditExportEvent(ctx, tx, job.ID, "FAILED", nil, details, now); err != nil {
		return err
	}
	return appendAuditExportAudit(ctx, tx, job.ActorUserID, "admin.audit.export.failed", job.ID, "FAILED", details, now)
}
