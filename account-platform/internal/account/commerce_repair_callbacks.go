package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A stale callback can supply evidence, never completion authority. Original
// attempts (including earlier attempts of this same job) remain attributable.
// No mobile, raw supplier identifier, error text or credentials are retained.
func retainCommerceRepairCallback(ctx context.Context, tx pgx.Tx, job CommerceCustomerSyncJob, kind, reference string) (bool, error) {
	j, status, until, subjectStatus, lifecycle, err := lockCommerceRepairJob(ctx, tx, job)
	if err != nil {
		return false, err
	}
	if job.Attempts < 1 || job.Attempts > j.Attempts {
		return false, ErrConflict
	}
	c, err := scanCommerceRepairCase(tx.QueryRow(ctx, `SELECT `+commerceRepairCaseColumns+` FROM commerce_customer_repair_cases WHERE job_id=$1 FOR UPDATE`, job.ID))
	if errors.Is(err, ErrNotFound) {
		now, e := approvalDatabaseTime(ctx, tx)
		if e != nil {
			return false, e
		}
		discovery := commerceRepairManual
		if subjectStatus != "ACTIVE" && subjectStatus != "DISABLED" || job.SubjectType == "USER" && lifecycle != job.SubjectLifecycleVersion {
			discovery = commerceRepairRetiredCallback
		}
		c, _, err = ensureCommerceRepairCaseTx(ctx, tx, j, status, until, discovery, now)
		if errors.Is(err, ErrConflict) {
			return false, nil
		}
	}
	if err != nil {
		return false, err
	}
	if !commerceRepairBindingMatches(c, j) || job.Attempts < 1 || job.Attempts > c.Attempts {
		return false, ErrConflict
	}
	if kind != "SUCCEEDED" && kind != "PROVIDER_ERROR" {
		return false, ErrConflict
	}
	if !commerceRedactedReference.MatchString(reference) || subjectStatus != "ACTIVE" && subjectStatus != "DISABLED" || lifecycle != job.SubjectLifecycleVersion {
		reference = ""
	}
	if kind == "PROVIDER_ERROR" {
		reference = ""
	}
	digest := sha256.Sum256([]byte(kind + "\x00" + reference))
	now, err := approvalDatabaseTime(ctx, tx)
	if err != nil {
		return false, err
	}
	id := uuid.Must(uuid.NewV7())
	var inserted uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO commerce_customer_repair_callbacks(id,case_id,attempts,kind,external_reference_redacted,callback_digest,after_completion,recorded_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(case_id,attempts,callback_digest) DO NOTHING RETURNING id`, id, c.ID, job.Attempts, kind, reference, hex.EncodeToString(digest[:]), c.Status == "RESOLVED", now).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if c.Status == "OPEN" {
		// New evidence invalidates any earlier human proposal. A reviewer must
		// inspect the new observation and obtain a fresh proposer attestation.
		if _, err = tx.Exec(ctx, `UPDATE commerce_customer_repair_cases SET version=version+1 WHERE id=$1`, c.ID); err != nil {
			return false, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at)
VALUES($1::uuid,$2,'COMMERCE_CALLBACK_EVIDENCE_RETAINED',jsonb_build_object('callback_id',($1::uuid)::text,'attempts',$3::integer,'kind',$4::text,'after_completion',$5::boolean),$6)`, id, c.ID, job.Attempts, kind, c.Status == "RESOLVED", now); err != nil {
		return false, err
	}
	return true, nil
}

func finishCommerceRepairCallback(ctx context.Context, tx pgx.Tx, job CommerceCustomerSyncJob, kind, reference string) (bool, error) {
	handled, err := retainCommerceRepairCallback(ctx, tx, job, kind, reference)
	if err != nil || !handled {
		return handled, err
	}
	if err = tx.Commit(ctx); err != nil {
		return true, err
	}
	// The original callback did not gain permission to complete or redispatch.
	return true, ErrConflict
}
