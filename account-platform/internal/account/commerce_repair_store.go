package account

import (
	"context"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const commerceRepairCaseColumns = `id,job_id,mapping_id,mapping_version,subject_type,subject_id,subject_lifecycle_version,
provider_id,adapter_version,task_category,attempts,version,status,resolution,approved_observation_id,reviewer_user_id,reviewer_binding_id,reviewer_authorization_version,created_at,resolved_at`
const commerceRepairObservationColumns = `id,case_id,case_version,outcome,evidence_reference,evidence_sha256,all_attempts_ended,
external_reference_redacted,proposer_user_id,proposer_binding_id,proposer_authorization_version,created_at`

type commerceRepairScanner interface{ Scan(...any) error }

type commerceRepairDiscovery uint8

const (
	commerceRepairAutomatic commerceRepairDiscovery = iota
	commerceRepairManual
	commerceRepairRetiredCallback
)

func scanCommerceRepairCase(row commerceRepairScanner) (c CommerceRepairCase, err error) {
	err = row.Scan(&c.ID, &c.JobID, &c.MappingID, &c.MappingVersion, &c.SubjectType, &c.SubjectID, &c.SubjectLifecycleVersion,
		&c.ProviderID, &c.AdapterVersion, &c.TaskCategory, &c.Attempts, &c.Version, &c.Status, &c.Resolution, &c.ApprovedObservationID, &c.ReviewerUserID, &c.ReviewerBindingID, &c.ReviewerAuthorizationVersion, &c.CreatedAt, &c.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

func scanCommerceRepairObservation(row commerceRepairScanner) (o CommerceRepairObservation, err error) {
	err = row.Scan(&o.ID, &o.CaseID, &o.CaseVersion, &o.Outcome, &o.EvidenceReference, &o.EvidenceSHA256, &o.AllAttemptsEnded,
		&o.ExternalReferenceRedacted, &o.ProposerUserID, &o.ProposerBindingID, &o.ProposerAuthorizationVersion, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

func commerceRepairActor(ctx context.Context, tx pgx.Tx, principal identity.Principal) (uuid.UUID, error) {
	if !humanApprovalPrincipal(principal) {
		return uuid.Nil, ErrForbidden
	}
	return activeUserID(ctx, tx, principal)
}

type commerceRepairAuthority struct {
	Locked    lockedPlatformAuthorization
	BindingID uuid.UUID
	Version   int64
}

// Writes to SLA, event and audit rows can wait after the initial admission
// check. Revalidate the frozen authorities using one fresh database clock
// after those writes, so an expired authority rolls back the whole operation.
func recheckCommerceRepairAuthorities(ctx context.Context, tx pgx.Tx, authorities ...commerceRepairAuthority) error {
	now, err := approvalDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	for _, authority := range authorities {
		binding, version, _, err := platformAuthorizationAt(ctx, tx, authority.Locked, now)
		if err != nil {
			return err
		}
		if authority.BindingID != uuid.Nil && (binding != authority.BindingID || version != authority.Version) {
			return ErrForbidden
		}
	}
	return nil
}

// All users (including the subject) precede any role/version, mapping, job or
// case lock. The final permission check uses the DB clock after row waits.
func lockCommerceRepairAuthorization(ctx context.Context, tx pgx.Tx, job CommerceCustomerSyncJob, requirements ...platformAuthorizationRequirement) ([]lockedPlatformAuthorization, error) {
	ids := make([]uuid.UUID, 0, len(requirements)+1)
	for _, r := range requirements {
		ids = append(ids, r.UserID)
	}
	if job.SubjectType == "USER" {
		ids = append(ids, job.SubjectID)
	}
	rows, err := tx.Query(ctx, `SELECT id FROM users WHERE id=ANY($1::uuid[]) ORDER BY id FOR SHARE`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return lockPlatformAuthorizationFacts(ctx, tx, requirements...)
}

func loadCommerceRepairJob(ctx context.Context, q queryable, id uuid.UUID, suffix string) (j CommerceCustomerSyncJob, status string, until *time.Time, err error) {
	err = q.QueryRow(ctx, `SELECT id,mapping_id,mapping_version,subject_type,subject_id,subject_lifecycle_version,provider_id,adapter_version,task_category,attempts,status,lease_until FROM commerce_customer_sync_jobs WHERE id=$1 `+suffix, id).Scan(
		&j.ID, &j.MappingID, &j.MappingVersion, &j.SubjectType, &j.SubjectID, &j.SubjectLifecycleVersion, &j.ProviderID, &j.AdapterVersion, &j.TaskCategory, &j.Attempts, &status, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// No contact details are read or copied into repair evidence. Missing/retired
// subjects remain evidence-only; they can never acquire a current mapping.
func lockCommerceRepairJob(ctx context.Context, tx pgx.Tx, seed CommerceCustomerSyncJob) (CommerceCustomerSyncJob, string, *time.Time, string, int64, error) {
	return lockCommerceRepairJobMode(ctx, tx, seed, false)
}

func lockCommerceRepairJobMode(ctx context.Context, tx pgx.Tx, seed CommerceCustomerSyncJob, nonblocking bool) (CommerceCustomerSyncJob, string, *time.Time, string, int64, error) {
	state, lifecycle := "MISSING", int64(0)
	lockSuffix := ""
	if nonblocking {
		lockSuffix = " NOWAIT"
	}
	var err error
	if seed.SubjectType == "USER" {
		err = tx.QueryRow(ctx, `SELECT status,subject_lifecycle_version FROM users WHERE id=$1 FOR SHARE`+lockSuffix, seed.SubjectID).Scan(&state, &lifecycle)
	} else {
		err = tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR SHARE`+lockSuffix, seed.SubjectID).Scan(&state)
		lifecycle = 1
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return seed, "", nil, "", 0, err
	}
	var id uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`+lockSuffix, seed.MappingID).Scan(&id); err != nil {
		return seed, "", nil, "", 0, err
	}
	j, status, until, err := loadCommerceRepairJob(ctx, tx, seed.ID, "FOR UPDATE"+lockSuffix)
	if err == nil && (j.MappingID != seed.MappingID || j.SubjectType != seed.SubjectType || j.SubjectID != seed.SubjectID || j.MappingVersion != seed.MappingVersion || j.SubjectLifecycleVersion != seed.SubjectLifecycleVersion || j.ProviderID != seed.ProviderID || j.AdapterVersion != seed.AdapterVersion || j.TaskCategory != seed.TaskCategory) {
		err = ErrConflict
	}
	return j, status, until, state, lifecycle, err
}

func commerceRepairBindingMatches(c CommerceRepairCase, j CommerceCustomerSyncJob) bool {
	return c.JobID == j.ID && c.MappingID == j.MappingID && c.MappingVersion == j.MappingVersion && c.SubjectType == j.SubjectType &&
		c.SubjectID == j.SubjectID && c.SubjectLifecycleVersion == j.SubjectLifecycleVersion && c.ProviderID == j.ProviderID && c.AdapterVersion == j.AdapterVersion && c.TaskCategory == j.TaskCategory && c.Attempts == j.Attempts
}

func (r *PostgresRepository) CommerceRepairCases(ctx context.Context, principal identity.Principal, after uuid.UUID, limit int) ([]CommerceRepairCase, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidCommerceRepair
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	actor, err := commerceRepairActor(ctx, tx, principal)
	if err != nil {
		return nil, err
	}
	if _, _, _, err = strongPlatformAuthorization(ctx, tx, actor, PermissionCommerceRepairsRead); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+commerceRepairCaseColumns+` FROM commerce_customer_repair_cases WHERE id>$1 ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	items := []CommerceRepairCase{}
	for rows.Next() {
		item, e := scanCommerceRepairCase(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		items = append(items, item)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}

func (r *PostgresRepository) CommerceRepairDetail(ctx context.Context, principal identity.Principal, id uuid.UUID, afterVersion, afterCallback int64) (out CommerceRepairDetail, err error) {
	if id == uuid.Nil || afterVersion < 0 || afterCallback < 0 {
		return out, ErrInvalidCommerceRepair
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	actor, err := commerceRepairActor(ctx, tx, principal)
	if err != nil {
		return out, err
	}
	if _, _, _, err = strongPlatformAuthorization(ctx, tx, actor, PermissionCommerceRepairsRead); err != nil {
		return out, err
	}
	out.Case, err = scanCommerceRepairCase(tx.QueryRow(ctx, `SELECT `+commerceRepairCaseColumns+` FROM commerce_customer_repair_cases WHERE id=$1 FOR SHARE`, id))
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT `+commerceRepairObservationColumns+` FROM commerce_customer_repair_observations WHERE case_id=$1 AND case_version>$2 ORDER BY case_version LIMIT 101`, id, afterVersion)
	if err != nil {
		return out, err
	}
	out.Observations = []CommerceRepairObservation{}
	for rows.Next() {
		o, e := scanCommerceRepairObservation(rows)
		if e != nil {
			rows.Close()
			return out, e
		}
		out.Observations = append(out.Observations, o)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Observations) > 100 {
		out.MoreObservations = true
		out.Observations = out.Observations[:100]
	}
	rows, err = tx.Query(ctx, `SELECT id,observation_sequence,case_id,attempts,kind,external_reference_redacted,after_completion,recorded_at
FROM commerce_customer_repair_callbacks WHERE case_id=$1 AND observation_sequence>$2 ORDER BY observation_sequence LIMIT 101`, id, afterCallback)
	if err != nil {
		return out, err
	}
	out.Callbacks = []CommerceRepairCallback{}
	for rows.Next() {
		var callback CommerceRepairCallback
		if err = rows.Scan(&callback.ID, &callback.Sequence, &callback.CaseID, &callback.Attempts, &callback.Kind, &callback.ExternalReferenceRedacted, &callback.AfterCompletion, &callback.RecordedAt); err != nil {
			rows.Close()
			return out, err
		}
		out.Callbacks = append(out.Callbacks, callback)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Callbacks) > 100 {
		out.MoreCallbacks = true
		out.Callbacks = out.Callbacks[:100]
	}
	return out, tx.Commit(ctx)
}

// The caller holds subject, mapping and job locks. Retry/expired-lease
// quarantine does not assert failure or no external effect.
func ensureCommerceRepairCaseTx(ctx context.Context, tx pgx.Tx, j CommerceCustomerSyncJob, status string, until *time.Time, discovery commerceRepairDiscovery, now time.Time) (CommerceRepairCase, bool, error) {
	existing, err := scanCommerceRepairCase(tx.QueryRow(ctx, `SELECT `+commerceRepairCaseColumns+` FROM commerce_customer_repair_cases WHERE job_id=$1 FOR UPDATE`, j.ID))
	if err == nil {
		if !commerceRepairBindingMatches(existing, j) {
			return existing, false, ErrConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return existing, false, err
	}
	eligible := status == "REPAIR_REQUIRED" || status == "LEASED" && until != nil && !until.After(now) ||
		discovery != commerceRepairAutomatic && status == "CANCELLED_ACCOUNT_CLOSURE" && j.Attempts > 0 ||
		discovery != commerceRepairAutomatic && status == "RETRY" && j.Attempts > 0 ||
		discovery == commerceRepairRetiredCallback && status == "LEASED" && j.Attempts > 0
	if !eligible || j.TaskCategory == "FINANCIAL" {
		return existing, false, ErrConflict
	}
	id := uuid.Must(uuid.NewV7())
	deadlines, err := manualreview.DeadlinesFor(now, "P2")
	if err != nil {
		return existing, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at)
VALUES($1,'ACCOUNT','COMMERCE_CUSTOMER_SYNC_REPAIR','COMMERCE_CUSTOMER_SYNC_JOB',$2,'P2','OPEN',$3,$4,$5,$6,$7,$3)`, id, j.ID, now, deadlines.Claim, deadlines.FirstResponse, deadlines.Mitigation, deadlines.Resolution); err != nil {
		return existing, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO commerce_customer_repair_cases(id,job_id,mapping_id,mapping_version,subject_type,subject_id,subject_lifecycle_version,provider_id,adapter_version,task_category,attempts,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, j.ID, j.MappingID, j.MappingVersion, j.SubjectType, j.SubjectID, j.SubjectLifecycleVersion, j.ProviderID, j.AdapterVersion, j.TaskCategory, j.Attempts, now)
	if err != nil {
		return existing, false, err
	}
	evidenceOnly := status == "CANCELLED_ACCOUNT_CLOSURE" || discovery == commerceRepairRetiredCallback
	if evidenceOnly {
		_, err = tx.Exec(ctx, `UPDATE commerce_customer_sync_jobs SET commerce_repair_case_id=$2 WHERE id=$1`, j.ID, id)
	} else {
		_, err = tx.Exec(ctx, `UPDATE commerce_customer_sync_jobs SET status='REPAIR_REQUIRED',lease_until=NULL,last_error_code=COALESCE(last_error_code,'COMMERCE_RESULT_UNCONFIRMED'),commerce_repair_case_id=$3,updated_at=$2 WHERE id=$1`, j.ID, now, id)
	}
	if err != nil {
		return existing, false, err
	}
	if !evidenceOnly {
		if _, err = tx.Exec(ctx, `UPDATE commerce_customer_mappings SET status='REPAIR_REQUIRED',last_error_code=COALESCE(last_error_code,'COMMERCE_RESULT_UNCONFIRMED'),updated_at=$2
WHERE id=$1 AND version=$3 AND provider_id=$4 AND adapter_version=$5 AND status<>'DETACHED' AND subject_type=$6 AND subject_id=$7
AND (($6='USER' AND EXISTS(SELECT 1 FROM users WHERE id=$7 AND status='ACTIVE' AND subject_lifecycle_version=$8))
 OR ($6='ORGANIZATION' AND EXISTS(SELECT 1 FROM organizations WHERE id=$7 AND status='ACTIVE')))`, j.MappingID, now, j.MappingVersion, j.ProviderID, j.AdapterVersion, j.SubjectType, j.SubjectID, j.SubjectLifecycleVersion); err != nil {
			return existing, false, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at) VALUES($1,$2,'CREATED',jsonb_build_object('job_id',$3::text,'attempts',$4::integer,'result','UNCONFIRMED'),$5)`, uuid.Must(uuid.NewV7()), id, j.ID.String(), j.Attempts, now); err != nil {
		return existing, false, err
	}
	if err = appendOutbox(ctx, tx, "account.manual-review.created.v1", id, 1, map[string]any{"case_id": id, "case_type": "COMMERCE_CUSTOMER_SYNC_REPAIR", "resource_type": "COMMERCE_CUSTOMER_SYNC_JOB", "resource_id": j.ID, "severity": "P2"}); err != nil {
		return existing, false, err
	}
	c, err := scanCommerceRepairCase(tx.QueryRow(ctx, `SELECT `+commerceRepairCaseColumns+` FROM commerce_customer_repair_cases WHERE id=$1`, id))
	return c, true, err
}

func (r *PostgresRepository) QuarantineCommerceRepair(ctx context.Context, principal identity.Principal, jobID uuid.UUID) (out CommerceRepairCase, err error) {
	if jobID == uuid.Nil {
		return out, ErrInvalidCommerceRepair
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	seed, _, _, err := loadCommerceRepairJob(ctx, tx, jobID, "")
	if err != nil {
		return out, err
	}
	actor, err := commerceRepairActor(ctx, tx, principal)
	if err != nil {
		return out, err
	}
	locks, err := lockCommerceRepairAuthorization(ctx, tx, seed, platformAuthorizationRequirement{UserID: actor, Permission: PermissionCommerceRepairsRequest})
	if err != nil {
		return out, err
	}
	j, status, until, _, _, err := lockCommerceRepairJob(ctx, tx, seed)
	if err != nil {
		return out, err
	}
	now, err := approvalDatabaseTime(ctx, tx)
	if err != nil {
		return out, err
	}
	if _, _, _, err = platformAuthorizationAt(ctx, tx, locks[0], now); err != nil {
		return out, err
	}
	var created bool
	out, created, err = ensureCommerceRepairCaseTx(ctx, tx, j, status, until, commerceRepairManual, now)
	if err != nil {
		return out, err
	}
	if created {
		err = appendAudit(ctx, tx, actor, principal.SessionID, nil, "commerce.repair.quarantine", "commerce_sync_job", j.ID.String(), "SUCCEEDED", map[string]any{"case_id": out.ID})
		if err != nil {
			return out, err
		}
	}
	if err = recheckCommerceRepairAuthorities(ctx, tx, commerceRepairAuthority{Locked: locks[0]}); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (r *PostgresRepository) SyncCommerceRepairCases(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	if limit > 500 {
		limit = 500
	}
	// Bound pool acquisition and non-row-lock writes as well as row discovery.
	// Successfully committed cases remain available when a later item times out.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT j.id FROM commerce_customer_sync_jobs j WHERE j.commerce_repair_case_id IS NULL AND j.task_category IN ('CUSTOMER_PROVISIONING','PROFILE_SYNC')
AND (j.status='REPAIR_REQUIRED' OR j.status='LEASED' AND j.lease_until<=clock_timestamp())
ORDER BY j.updated_at,j.id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	created := 0
	for _, id := range ids {
		added, e := r.syncCommerceRepairCase(ctx, id)
		var busy *pgconn.PgError
		if errors.As(e, &busy) && busy.Code == "55P03" {
			continue // Busy original attempt: retain it for the next scan.
		}
		if e != nil && !errors.Is(e, ErrConflict) {
			return created, e
		}
		if e == nil && added {
			created++
		}
	}
	return created, nil
}

func (r *PostgresRepository) syncCommerceRepairCase(ctx context.Context, id uuid.UUID) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	seed, _, _, err := loadCommerceRepairJob(ctx, tx, id, "")
	if err != nil {
		return false, err
	}
	j, status, until, _, _, err := lockCommerceRepairJobMode(ctx, tx, seed, true)
	if err != nil {
		return false, err
	}
	now, err := approvalDatabaseTime(ctx, tx)
	if err != nil {
		return false, err
	}
	_, added, err := ensureCommerceRepairCaseTx(ctx, tx, j, status, until, commerceRepairAutomatic, now)
	if err != nil {
		return false, err
	}
	return added, tx.Commit(ctx)
}
