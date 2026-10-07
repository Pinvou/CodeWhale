package account

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	defaultCommerceProviderID       = "commerce-default"
	defaultCommerceAdapterVersion   = "v1"
	commerceCustomerNotConfigured   = "NOT_CONFIGURED"
	commerceCustomerMaximumAttempts = 12
)

type CommerceCustomerMapping struct {
	Status         string    `json:"status"`
	ProviderID     string    `json:"provider_id,omitempty"`
	AdapterVersion string    `json:"adapter_version,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

type CommerceCustomerSyncJob struct {
	ID                      uuid.UUID
	MappingID               uuid.UUID
	MappingVersion          int64
	SubjectType             string
	SubjectID               uuid.UUID
	SubjectLifecycleVersion int64
	ProviderID              string
	AdapterVersion          string
	TaskCategory            string
	ContactMobile           string
	Attempts                int
}

type CommerceCustomerSyncStore interface {
	ClaimCommerceCustomerSyncJob(context.Context, string, string, time.Duration) (*CommerceCustomerSyncJob, error)
	CompleteCommerceCustomerSyncJob(context.Context, CommerceCustomerSyncJob, commerce.CustomerProvisionResult) error
	RetryCommerceCustomerSyncJob(context.Context, CommerceCustomerSyncJob, string, time.Time) error
}

type CommerceCustomerSyncWorker struct {
	store          CommerceCustomerSyncStore
	adapter        commerce.CustomerAdapter
	providerID     string
	adapterVersion string
	interval       time.Duration
}

func NewCommerceCustomerSyncWorker(store CommerceCustomerSyncStore, adapter commerce.CustomerAdapter, providerID, adapterVersion string, interval time.Duration) *CommerceCustomerSyncWorker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &CommerceCustomerSyncWorker{
		store: store, adapter: adapter,
		providerID: strings.TrimSpace(providerID), adapterVersion: strings.TrimSpace(adapterVersion),
		interval: interval,
	}
}

func (w *CommerceCustomerSyncWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		w.runOne(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *CommerceCustomerSyncWorker) runOne(ctx context.Context) {
	job, err := w.store.ClaimCommerceCustomerSyncJob(ctx, w.providerID, w.adapterVersion, 30*time.Second)
	if err != nil || job == nil {
		return
	}
	result, err := w.adapter.EnsureCustomer(ctx, commerce.CustomerProvisionRequest{
		SubjectType: job.SubjectType, SubjectID: job.SubjectID.String(), ContactMobile: job.ContactMobile,
	})
	if err != nil {
		delay := time.Duration(1<<min(job.Attempts, 8)) * time.Minute
		_ = w.store.RetryCommerceCustomerSyncJob(context.WithoutCancel(ctx), *job, "COMMERCE_PROVIDER_UNAVAILABLE", time.Now().UTC().Add(delay))
		return
	}
	_ = w.store.CompleteCommerceCustomerSyncJob(context.WithoutCancel(ctx), *job, result)
}

func (r *PostgresRepository) ConfigureCommerceProvider(providerID, adapterVersion string) error {
	providerID, adapterVersion = strings.TrimSpace(providerID), strings.TrimSpace(adapterVersion)
	if providerID == "" || adapterVersion == "" || len(providerID) > 80 || len(adapterVersion) > 80 {
		return errors.New("commerce provider requires non-empty provider and adapter version identifiers")
	}
	r.commerceProviderID, r.commerceAdapterVersion = providerID, adapterVersion
	return nil
}

func (r *PostgresRepository) EnsureCommerceProviderMappings(ctx context.Context) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
SELECT u.id,u.mobile_e164 FROM users u
LEFT JOIN commerce_customer_mappings m
  ON m.subject_type='USER' AND m.subject_id=u.id AND m.provider_id=$1
WHERE u.mobile_verified_at IS NOT NULL AND u.mobile_e164 IS NOT NULL AND u.status='ACTIVE'
  AND (m.id IS NULL OR (m.status <> 'READY' AND m.adapter_version <> $2))
ORDER BY u.id FOR UPDATE OF u`, r.commerceProviderID, r.commerceAdapterVersion)
	if err != nil {
		return err
	}
	type candidate struct {
		userID uuid.UUID
		mobile string
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.userID, &item.mobile); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	now := r.now()
	for _, item := range candidates {
		if err = enqueueCommerceCustomerSync(ctx, tx, "USER", item.userID, item.mobile, r.commerceProviderID, r.commerceAdapterVersion, "CUSTOMER_PROVISIONING", now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func enqueueCommerceCustomerSync(ctx context.Context, tx pgx.Tx, subjectType string, subjectID uuid.UUID, mobile, providerID, adapterVersion, taskCategory string, now time.Time) error {
	mappingID := uuid.Must(uuid.NewV7())
	var mappingVersion int64
	err := tx.QueryRow(ctx, `
INSERT INTO commerce_customer_mappings(id,subject_type,subject_id,provider_id,adapter_version,status,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,'PENDING',$6,$6)
ON CONFLICT(subject_type,subject_id,provider_id) DO UPDATE SET
    adapter_version=EXCLUDED.adapter_version,status='PENDING',external_reference_redacted=NULL,
    last_error_code=NULL,provisioned_at=NULL,detached_at=NULL,version=commerce_customer_mappings.version+1,updated_at=EXCLUDED.updated_at
RETURNING id,version`, mappingID, subjectType, subjectID, providerID, adapterVersion, now).Scan(&mappingID, &mappingVersion)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE commerce_customer_sync_jobs SET status='COMPLETED',completed_at=$2,updated_at=$2,last_error_code='SUPERSEDED' WHERE mapping_id=$1 AND status IN ('PENDING','RETRY')`, mappingID, now); err != nil {
		return err
	}
	// Original 015 records a job lifecycle version. This personal/payment
	// delivery has no irreversible account closure or user lifecycle column;
	// version 1 is its actual invariant. Active user and mapping-version fences
	// remain authoritative here; closure delivery restores its lifecycle fence.
	subjectLifecycleVersion := int64(1)
	_, err = tx.Exec(ctx, `
INSERT INTO commerce_customer_sync_jobs(
    id,mapping_id,mapping_version,subject_type,subject_id,subject_lifecycle_version,provider_id,adapter_version,
    task_category,contact_mobile_e164,status,next_attempt_at,created_at,updated_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'PENDING',$11,$11,$11)`,
		uuid.Must(uuid.NewV7()), mappingID, mappingVersion, subjectType, subjectID, subjectLifecycleVersion, providerID, adapterVersion, taskCategory, mobile, now)
	return err
}

func (r *PostgresRepository) ClaimCommerceCustomerSyncJob(ctx context.Context, providerID, adapterVersion string, lease time.Duration) (*CommerceCustomerSyncJob, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	var job CommerceCustomerSyncJob
	err = tx.QueryRow(ctx, `
SELECT id,mapping_id,mapping_version,subject_type,subject_id,subject_lifecycle_version,provider_id,adapter_version,task_category,contact_mobile_e164,attempts
FROM commerce_customer_sync_jobs
WHERE provider_id=$2 AND adapter_version=$3
  AND (subject_type<>'USER' OR EXISTS(SELECT 1 FROM users WHERE id=subject_id AND status='ACTIVE'))
  AND ((status IN ('PENDING','RETRY') AND next_attempt_at<=$1) OR (status='LEASED' AND lease_until<$1))
ORDER BY next_attempt_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1`, now, providerID, adapterVersion).Scan(
		&job.ID, &job.MappingID, &job.MappingVersion, &job.SubjectType, &job.SubjectID, &job.SubjectLifecycleVersion,
		&job.ProviderID, &job.AdapterVersion, &job.TaskCategory, &job.ContactMobile, &job.Attempts,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	command, err := tx.Exec(ctx, `
UPDATE commerce_customer_sync_jobs SET status='LEASED',attempts=attempts+1,lease_until=$2,updated_at=$1
WHERE id=$3 AND status IN ('PENDING','RETRY','LEASED')`, now, now.Add(lease), job.ID)
	if err != nil {
		return nil, err
	}
	if command.RowsAffected() != 1 {
		return nil, errors.New("commerce customer job lease was lost")
	}
	if _, err = tx.Exec(ctx, `UPDATE commerce_customer_mappings SET status='IN_PROGRESS',updated_at=$2 WHERE id=$1 AND status <> 'DETACHED' AND version=$3`, job.MappingID, now, job.MappingVersion); err != nil {
		return nil, err
	}
	job.Attempts++
	return &job, tx.Commit(ctx)
}

func (r *PostgresRepository) CompleteCommerceCustomerSyncJob(ctx context.Context, job CommerceCustomerSyncJob, result commerce.CustomerProvisionResult) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	result.ExternalReferenceRedacted = safeExternalReference(result.ExternalReferenceRedacted)
	var currentProvider, currentAdapter, mappingStatus string
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT provider_id,adapter_version,status,version FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`, job.MappingID).Scan(&currentProvider, &currentAdapter, &mappingStatus, &currentVersion)
	if err != nil {
		return err
	}
	if err = lockCommerceCustomerSyncLease(ctx, tx, job); err != nil {
		return err
	}
	userActive, userDisabled := true, false
	if job.SubjectType == "USER" {
		var userStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1`, job.SubjectID).Scan(&userStatus); err != nil {
			return err
		}
		userActive = userStatus == "ACTIVE"
		userDisabled = userStatus == "DISABLED"
	}
	currentMapping := mappingStatus != "DETACHED" && currentProvider == job.ProviderID && currentAdapter == job.AdapterVersion && currentVersion == job.MappingVersion
	outcome := "SUCCEEDED"
	if !currentMapping || !userActive {
		outcome = "STALE"
	} else {
		if _, err = tx.Exec(ctx, `UPDATE commerce_customer_mappings SET status='READY',external_reference_redacted=NULLIF($2,''),last_error_code=NULL,provisioned_at=$3,version=version+1,updated_at=$3 WHERE id=$1`, job.MappingID, result.ExternalReferenceRedacted, now); err != nil {
			return err
		}
	}
	jobStatus := "COMPLETED"
	if currentMapping && userDisabled {
		// A temporary disable cannot accept this late result, but it must not
		// retire the current generation. Claim still requires ACTIVE and will
		// call the provider again after the existing enable operation.
		jobStatus = "RETRY"
	}
	if _, err = tx.Exec(ctx, `UPDATE commerce_customer_sync_jobs SET status=$3,lease_until=NULL,last_error_code=NULL,completed_at=CASE WHEN $3='RETRY' THEN NULL ELSE $2::timestamptz END,next_attempt_at=CASE WHEN $3='RETRY' THEN $2::timestamptz ELSE next_attempt_at END,updated_at=$2::timestamptz WHERE id=$1 AND status='LEASED'`, job.ID, now, jobStatus); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO commerce_customer_sync_evidence(id,job_id,mapping_id,provider_id,adapter_version,outcome,external_reference_redacted,recorded_at) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)`, uuid.Must(uuid.NewV7()), job.ID, job.MappingID, job.ProviderID, job.AdapterVersion, outcome, result.ExternalReferenceRedacted, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func safeExternalReference(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 80 || !strings.Contains(value, "*") {
		return ""
	}
	return value
}

// The caller locks the mapping first, matching enqueue's mapping-to-job order.
// Read the database clock only after both locks, so waiting for a row cannot
// extend the authority of an expired or subsequently reclaimed lease.
func lockCommerceCustomerSyncLease(ctx context.Context, tx pgx.Tx, job CommerceCustomerSyncJob) error {
	var attempts int
	var status string
	var until *time.Time
	err := tx.QueryRow(ctx, `SELECT attempts,status,lease_until FROM commerce_customer_sync_jobs WHERE id=$1 FOR UPDATE`, job.ID).Scan(&attempts, &status, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if status != "LEASED" || attempts != job.Attempts || until == nil || !until.After(now) {
		return ErrConflict
	}
	return nil
}

func (r *PostgresRepository) RetryCommerceCustomerSyncJob(ctx context.Context, job CommerceCustomerSyncJob, reason string, next time.Time) error {
	if len(reason) > 120 {
		reason = reason[:120]
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var mappingID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`, job.MappingID).Scan(&mappingID); err != nil {
		return err
	}
	if err = lockCommerceCustomerSyncLease(ctx, tx, job); err != nil {
		return err
	}
	now := r.now()
	jobStatus, mappingStatus := "RETRY", "RETRY"
	if job.Attempts >= commerceCustomerMaximumAttempts {
		jobStatus, mappingStatus = "REPAIR_REQUIRED", "REPAIR_REQUIRED"
	}
	if _, err = tx.Exec(ctx, `UPDATE commerce_customer_sync_jobs SET status=$2,lease_until=NULL,last_error_code=$3,next_attempt_at=$4,updated_at=$5 WHERE id=$1 AND status='LEASED'`, job.ID, jobStatus, reason, next, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE commerce_customer_mappings SET status=$2,last_error_code=$3,updated_at=$4 WHERE id=$1 AND status <> 'DETACHED' AND version=$5 AND ($6<>'USER' OR EXISTS(SELECT 1 FROM users WHERE id=$7 AND status='ACTIVE'))`, job.MappingID, mappingStatus, reason, now, job.MappingVersion, job.SubjectType, job.SubjectID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO commerce_customer_sync_evidence(id,job_id,mapping_id,provider_id,adapter_version,outcome,error_code,recorded_at) VALUES($1,$2,$3,$4,$5,'FAILED',$6,$7)`, uuid.Must(uuid.NewV7()), job.ID, job.MappingID, job.ProviderID, job.AdapterVersion, reason, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) GetCommerceCustomerMapping(ctx context.Context, principal identity.Principal) (CommerceCustomerMapping, error) {
	var mapping CommerceCustomerMapping
	err := r.pool.QueryRow(ctx, `
SELECT COALESCE(m.status,$3),COALESCE(m.provider_id,''),COALESCE(m.adapter_version,''),COALESCE(m.updated_at,'epoch'::timestamptz)
FROM identities i JOIN users u ON u.id=i.user_id
LEFT JOIN commerce_customer_mappings m ON m.subject_type='USER' AND m.subject_id=u.id AND m.provider_id=$4
WHERE i.issuer=$1 AND i.subject=$2`, principal.Issuer, principal.Subject, commerceCustomerNotConfigured, r.commerceProviderID).Scan(&mapping.Status, &mapping.ProviderID, &mapping.AdapterVersion, &mapping.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CommerceCustomerMapping{}, ErrNotFound
	}
	return mapping, err
}
