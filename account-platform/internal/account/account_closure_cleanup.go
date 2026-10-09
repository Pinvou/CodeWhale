package account

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AccountClosureCleanupPreparation struct {
	IdentitySubjects        []string
	SubjectLifecycleVersion int64
}

func (r *PostgresRepository) PrepareAccountClosureCleanup(ctx context.Context, item *AccountClosureRequest) (AccountClosureCleanupPreparation, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockAccountClosureClaim(ctx, tx, *item); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	closureID, userID := item.ID, item.UserID
	var stage, closureStatus, userStatus string
	var preparedAt *time.Time
	var lifecycleVersion int64
	err = tx.QueryRow(ctx, `SELECT request.stage,request.status,request.cleanup_prepared_at,COALESCE(request.subject_lifecycle_version,0),users.status FROM account_closure_requests request JOIN users ON users.id=request.user_id WHERE request.id=$1 AND request.user_id=$2 FOR UPDATE OF request,users`, closureID, userID).Scan(&stage, &closureStatus, &preparedAt, &lifecycleVersion, &userStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountClosureCleanupPreparation{}, ErrNotFound
	}
	if err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if stage != "IDENTITY_CLEANUP" || (closureStatus != "IN_PROGRESS" && closureStatus != "RETRY" && closureStatus != "REPAIR_REQUIRED") || userStatus != "CLOSING" {
		return AccountClosureCleanupPreparation{}, ErrConflict
	}
	subjects, err := accountClosureIdentitySubjects(ctx, tx, userID)
	if err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if preparedAt != nil {
		if err := requireAccountClosureClaimTx(ctx, tx, *item); err != nil {
			return AccountClosureCleanupPreparation{}, err
		}
		return AccountClosureCleanupPreparation{IdentitySubjects: subjects, SubjectLifecycleVersion: lifecycleVersion}, tx.Commit(ctx)
	}
	var financialJobs bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commerce_customer_sync_jobs WHERE subject_type='USER' AND subject_id=$1 AND task_category='FINANCIAL' AND status NOT IN ('COMPLETED','CANCELLED_ACCOUNT_CLOSURE'))`, userID).Scan(&financialJobs); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if financialJobs {
		return AccountClosureCleanupPreparation{}, ErrConflict
	}
	var username, mobile, email *string
	if err := tx.QueryRow(ctx, `SELECT username,mobile_e164,email FROM users WHERE id=$1`, userID).Scan(&username, &mobile, &email); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	targetDigests := make([][]byte, 0, 8)
	if username != nil {
		targetDigests = append(targetDigests, invitationTargetDigest(*username))
	}
	if mobile != nil {
		targetDigests = append(targetDigests, invitationTargetDigest(*mobile))
	}
	if email != nil {
		targetDigests = append(targetDigests, invitationTargetDigest(normalizeEmail(*email)))
	}
	type pendingProfile struct {
		id                                uuid.UUID
		field, oldValue, newValue         string
		identityUpdated, accountCommitted *time.Time
	}
	rows, err := tx.Query(ctx, `SELECT id,field,old_value,new_value,identity_updated_at,account_committed_at FROM profile_change_requests WHERE user_id=$1 AND status NOT IN ('COMPLETED','CANCELLED_ACCOUNT_CLOSURE') ORDER BY id FOR UPDATE`, userID)
	if err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	profiles := make([]pendingProfile, 0)
	for rows.Next() {
		var item pendingProfile
		if err := rows.Scan(&item.id, &item.field, &item.oldValue, &item.newValue, &item.identityUpdated, &item.accountCommitted); err != nil {
			rows.Close()
			return AccountClosureCleanupPreparation{}, err
		}
		profiles = append(profiles, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return AccountClosureCleanupPreparation{}, err
	}
	rows.Close()
	now := r.now()
	for _, item := range profiles {
		if item.field == "USERNAME" {
			if item.oldValue != "" {
				targetDigests = append(targetDigests, invitationTargetDigest(item.oldValue))
			}
			targetDigests = append(targetDigests, invitationTargetDigest(item.newValue))
		} else {
			if item.oldValue != "" {
				targetDigests = append(targetDigests, invitationTargetDigest(item.oldValue))
			}
			targetDigests = append(targetDigests, invitationTargetDigest(item.newValue))
		}
		if item.identityUpdated != nil && item.accountCommitted == nil {
			if item.field == "USERNAME" {
				_, err = tx.Exec(ctx, `UPDATE users SET username=$2,profile_version=profile_version+1,version=version+1,updated_at=$3 WHERE id=$1`, userID, item.newValue, now)
			} else {
				_, err = tx.Exec(ctx, `UPDATE users SET mobile_e164=$2,mobile_verified_at=$3,profile_version=profile_version+1,version=version+1,updated_at=$3 WHERE id=$1`, userID, item.newValue, now)
			}
			if err != nil {
				return AccountClosureCleanupPreparation{}, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE profile_change_requests SET account_committed_at=CASE WHEN identity_updated_at IS NOT NULL THEN COALESCE(account_committed_at,$2) ELSE account_committed_at END,status='CANCELLED_ACCOUNT_CLOSURE',last_error_code='ACCOUNT_CLOSURE',version=version+1,updated_at=$2 WHERE id=$1`, item.id, now); err != nil {
			return AccountClosureCleanupPreparation{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE commerce_customer_sync_jobs SET status='CANCELLED_ACCOUNT_CLOSURE',lease_until=NULL,last_error_code='ACCOUNT_CLOSURE',completed_at=$2,updated_at=$2 WHERE subject_type='USER' AND subject_id=$1 AND task_category<>'FINANCIAL' AND status NOT IN ('COMPLETED','CANCELLED_ACCOUNT_CLOSURE')`, userID, now); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if err := r.cancelAccountClosureInvitations(ctx, tx, userID, targetDigests, now); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE role_bindings binding SET revoked_at=$2,revoked_by=$1 FROM memberships membership WHERE binding.membership_id=membership.id AND membership.user_id=$1 AND binding.revoked_at IS NULL`, userID, now); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE role_assignments SET revoked_at=$2,revoked_by=$1 WHERE user_id=$1 AND revoked_at IS NULL`, userID, now); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	membershipRows, err := tx.Query(ctx, `UPDATE memberships SET status='REVOKED',version=version+1,updated_at=$2 WHERE user_id=$1 AND status<>'REVOKED' RETURNING id,organization_id,version`, userID, now)
	if err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	type membershipEvent struct {
		id, organizationID uuid.UUID
		version            int64
	}
	events := make([]membershipEvent, 0)
	for membershipRows.Next() {
		var event membershipEvent
		if err := membershipRows.Scan(&event.id, &event.organizationID, &event.version); err != nil {
			membershipRows.Close()
			return AccountClosureCleanupPreparation{}, err
		}
		events = append(events, event)
	}
	membershipRows.Close()
	for _, event := range events {
		if err := appendOutbox(ctx, tx, "account.membership.changed.v1", event.id, event.version, map[string]any{"organization_id": event.organizationID, "user_id": userID, "membership_status": "REVOKED", "membership_version": event.version, "reason": "ACCOUNT_CLOSURE"}); err != nil {
			return AccountClosureCleanupPreparation{}, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if err := tx.QueryRow(ctx, `UPDATE users SET subject_lifecycle_version=subject_lifecycle_version+1,membership_version=membership_version+1,version=version+1,updated_at=$2 WHERE id=$1 RETURNING subject_lifecycle_version`, userID, now).Scan(&lifecycleVersion); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if err := requireAccountClosureClaimTx(ctx, tx, *item); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE account_closure_requests SET cleanup_prepared_at=$2,subject_lifecycle_version=$3,status='IN_PROGRESS',last_error_code=NULL,next_attempt_at=$2,version=version+1,updated_at=$2 WHERE id=$1`, closureID, now, lifecycleVersion); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_audit(id,closure_request_id,event_type,from_stage,to_stage,redacted_details,occurred_at) VALUES($1,$2,'IDENTITY_CLEANUP_PREPARED','IDENTITY_CLEANUP','IDENTITY_CLEANUP',jsonb_build_object('subject_lifecycle_version',$3::bigint,'memberships_terminated',$4::integer,'profile_changes_cancelled',$5::integer),$6)`, uuid.Must(uuid.NewV7()), closureID, lifecycleVersion, len(events), len(profiles), now); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if err := appendSecurityClosureAudit(ctx, tx, userID, now); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountClosureCleanupPreparation{}, err
	}
	item.Version++
	return AccountClosureCleanupPreparation{IdentitySubjects: subjects, SubjectLifecycleVersion: lifecycleVersion}, nil
}

func accountClosureIdentitySubjects(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT subject FROM identities WHERE user_id=$1 ORDER BY issuer,subject`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]string, 0)
	for rows.Next() {
		var subject string
		if err := rows.Scan(&subject); err != nil {
			return nil, err
		}
		items = append(items, subject)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) cancelAccountClosureInvitations(ctx context.Context, tx pgx.Tx, userID uuid.UUID, digests [][]byte, now time.Time) error {
	ids := make(map[uuid.UUID]uuid.UUID)
	rows, err := tx.Query(ctx, `SELECT id,organization_id FROM invitations WHERE status='PENDING' AND invited_by=$1 FOR UPDATE`, userID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, organizationID uuid.UUID
		if scanErr := rows.Scan(&id, &organizationID); scanErr != nil {
			rows.Close()
			return scanErr
		}
		ids[id] = organizationID
	}
	rows.Close()
	for _, digest := range digests {
		rows, err = tx.Query(ctx, `SELECT id,organization_id FROM invitations WHERE status='PENDING' AND target_digest=$1 FOR UPDATE`, digest)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, organizationID uuid.UUID
			if scanErr := rows.Scan(&id, &organizationID); scanErr != nil {
				rows.Close()
				return scanErr
			}
			ids[id] = organizationID
		}
		rows.Close()
	}
	for id, organizationID := range ids {
		if _, err := tx.Exec(ctx, `UPDATE invitations SET status='CANCELLED_ACCOUNT_CLOSURE',email_normalized=NULL,target_digest=CASE WHEN target_type='LINK' THEN NULL ELSE gen_random_bytes(32) END,target_hint=NULL,token_hash=gen_random_bytes(32),revoked_at=$2,version=version+1,updated_at=$2 WHERE id=$1 AND status='PENDING'`, id, now); err != nil {
			return err
		}
		if err := appendAudit(ctx, tx, userID, "", &organizationID, "invitation.cancelled.account_closure", "invitation", id.String(), "SUCCEEDED", map[string]any{"reason": "ACCOUNT_CLOSURE"}); err != nil {
			return err
		}
	}
	return nil
}

func (r *PostgresRepository) MarkAccountClosureIdentityDeleted(ctx context.Context, item *AccountClosureRequest, lifecycleVersion int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockAccountClosureClaim(ctx, tx, *item); err != nil {
		return err
	}
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE account_closure_requests SET identity_deleted_at=COALESCE(identity_deleted_at,$2),status='IN_PROGRESS',last_error_code=NULL,next_attempt_at=$2,version=version+1,updated_at=$2 WHERE id=$1 AND stage='IDENTITY_CLEANUP' AND subject_lifecycle_version=$3 AND cleanup_prepared_at IS NOT NULL AND version=$4 AND lease_until>clock_timestamp()`, item.ID, now, lifecycleVersion, item.Version)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	item.Version++
	return nil
}

func (r *PostgresRepository) MarkAccountClosureGrantsIsolated(ctx context.Context, item *AccountClosureRequest, lifecycleVersion int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockAccountClosureClaim(ctx, tx, *item); err != nil {
		return err
	}
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE account_closure_requests SET grants_isolated_at=COALESCE(grants_isolated_at,$2),status='IN_PROGRESS',last_error_code=NULL,next_attempt_at=$2,version=version+1,updated_at=$2 WHERE id=$1 AND stage='IDENTITY_CLEANUP' AND subject_lifecycle_version=$3 AND cleanup_prepared_at IS NOT NULL AND version=$4 AND lease_until>clock_timestamp()`, item.ID, now, lifecycleVersion, item.Version)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	item.Version++
	return nil
}

func (r *PostgresRepository) ScheduleAccountClosureCleanupError(ctx context.Context, item *AccountClosureRequest, code string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockAccountClosureClaim(ctx, tx, *item); err != nil {
		return err
	}
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE account_closure_requests SET status=CASE WHEN sla_expires_at<=$2::timestamptz THEN 'REPAIR_REQUIRED' ELSE 'RETRY' END,last_error_code=$3,next_attempt_at=CASE WHEN sla_expires_at<=$2::timestamptz THEN NULL ELSE $2::timestamptz+interval '30 seconds' END,lease_until=NULL,version=version+1,updated_at=$2 WHERE id=$1 AND stage IN ('IDENTITY_CLEANUP','ANONYMIZED') AND version=$4 AND lease_until>clock_timestamp()`, item.ID, now, code, item.Version)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func (r *PostgresRepository) AnonymizeAccountClosure(ctx context.Context, item *AccountClosureRequest, lifecycleVersion int64) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockAccountClosureClaim(ctx, tx, *item); err != nil {
		return err
	}
	closureID, userID := item.ID, item.UserID
	var userCode, stage string
	var identityDeletedAt, grantsIsolatedAt, replayClearedAt *time.Time
	var currentLifecycle int64
	err = tx.QueryRow(ctx, `SELECT users.user_code,users.subject_lifecycle_version,request.stage,request.identity_deleted_at,request.grants_isolated_at,request.token_replays_cleared_at FROM account_closure_requests request JOIN users ON users.id=request.user_id WHERE request.id=$1 AND request.user_id=$2 FOR UPDATE OF request,users`, closureID, userID).Scan(&userCode, &currentLifecycle, &stage, &identityDeletedAt, &grantsIsolatedAt, &replayClearedAt)
	if err != nil {
		return err
	}
	if stage == "ANONYMIZED" || stage == "COMPLETED" {
		return tx.Commit(ctx)
	}
	if stage != "IDENTITY_CLEANUP" || identityDeletedAt == nil || grantsIsolatedAt == nil || replayClearedAt == nil || currentLifecycle != lifecycleVersion {
		return ErrConflict
	}
	var unsafe bool
	if err := tx.QueryRow(ctx, `SELECT
EXISTS(SELECT 1 FROM memberships WHERE user_id=$1 AND status<>'REVOKED') OR
EXISTS(SELECT 1 FROM profile_change_requests WHERE user_id=$1 AND status NOT IN ('COMPLETED','CANCELLED_ACCOUNT_CLOSURE')) OR
EXISTS(SELECT 1 FROM commerce_customer_sync_jobs WHERE subject_type='USER' AND subject_id=$1 AND status NOT IN ('COMPLETED','CANCELLED_ACCOUNT_CLOSURE'))`, userID).Scan(&unsafe); err != nil {
		return err
	}
	if unsafe {
		return ErrConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE sms_challenges SET mobile_e164='closed:'||gen_random_uuid()::text,provider_message_id=NULL,updated_at=$2 WHERE id IN (SELECT sms_challenge_id FROM profile_change_requests WHERE user_id=$1 AND sms_challenge_id IS NOT NULL UNION SELECT sms_challenge_id FROM password_reset_operations WHERE user_id=$1 UNION SELECT sms_challenge_id FROM registration_operations WHERE candidate_user_id=$1 OR committed_user_id=$1)`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE profile_change_requests SET old_value='closed:'||gen_random_uuid()::text,new_value='closed:'||gen_random_uuid()::text,identity_issuer='closed',identity_subject='closed:'||gen_random_uuid()::text,updated_at=$2 WHERE user_id=$1`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE password_reset_operations SET identity_issuer='closed',identity_subject='closed:'||gen_random_uuid()::text WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE registration_operations SET username_normalized='closed_'||replace(gen_random_uuid()::text,'-',''),mobile_e164='closed:'||gen_random_uuid()::text,identity_subject=NULL,updated_at=$2 WHERE candidate_user_id=$1 OR committed_user_id=$1`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE commerce_customer_sync_jobs SET contact_mobile_e164=NULL,updated_at=$2 WHERE subject_type='USER' AND subject_id=$1`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE commerce_customer_mappings SET status='DETACHED',external_reference_redacted=NULL,last_error_code=NULL,detached_at=COALESCE(detached_at,$2),version=version+1,updated_at=$2 WHERE subject_type='USER' AND subject_id=$1 AND status<>'DETACHED'`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM identities WHERE user_id=$1`, userID); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE users SET status='DELETED',username='closed_'||replace(gen_random_uuid()::text,'-',''),mobile_e164='closed:'||gen_random_uuid()::text,mobile_verified_at=NULL,display_name=NULL,email=NULL,anonymized_at=$2,profile_version=profile_version+1,membership_version=membership_version+1,version=version+1,updated_at=$2 WHERE id=$1 AND status='CLOSING'`, userID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_subject_retention_records(user_id,user_code,retention_class,retained_until,status,created_at) VALUES($1,$2,'ACCOUNT_CLOSURE_LEGAL_RECORD',$3,'RETAINED',$4) ON CONFLICT(user_id) DO NOTHING`, userID, userCode, now.AddDate(3, 0, 0), now); err != nil {
		return err
	}
	if err := requireAccountClosureClaimTx(ctx, tx, *item); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE account_closure_requests SET stage='ANONYMIZED',status='IN_PROGRESS',blocker_snapshot='[]'::jsonb,last_error_code=NULL,next_attempt_at=$2,lease_until=NULL,version=version+1,updated_at=$2 WHERE id=$1 AND stage='IDENTITY_CLEANUP'`, closureID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_audit(id,closure_request_id,event_type,from_stage,to_stage,redacted_details,occurred_at) VALUES($1,$2,'SUBJECT_ANONYMIZED','IDENTITY_CLEANUP','ANONYMIZED',jsonb_build_object('subject_lifecycle_version',$3::bigint,'retained_until',$4::timestamptz),$5)`, uuid.Must(uuid.NewV7()), closureID, lifecycleVersion, now.AddDate(3, 0, 0), now); err != nil {
		return err
	}
	if err := appendOutbox(ctx, tx, "account.user.anonymized.v1", closureID, lifecycleVersion, map[string]any{"closure_request_id": closureID, "user_id": userID, "subject_lifecycle_version": lifecycleVersion, "anonymized_at": now}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) CompleteAccountClosure(ctx context.Context, item *AccountClosureRequest) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockAccountClosureClaim(ctx, tx, *item); err != nil {
		return err
	}
	closureID := item.ID
	var published bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.user.anonymized.v1' AND published_at IS NOT NULL)`, closureID).Scan(&published); err != nil {
		return err
	}
	if !published {
		return ErrConflict
	}
	now := r.now()
	if err := requireAccountClosureClaimTx(ctx, tx, *item); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE account_closure_requests SET stage='COMPLETED',status='COMPLETED',next_attempt_at=NULL,lease_until=NULL,completed_at=$2,version=version+1,updated_at=$2 WHERE id=$1 AND stage='ANONYMIZED' AND token_replays_cleared_at IS NOT NULL`, closureID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_audit(id,closure_request_id,event_type,from_stage,to_stage,redacted_details,occurred_at) VALUES($1,$2,'COMPLETED','ANONYMIZED','COMPLETED','{"outbox_confirmed":true}'::jsonb,$3)`, uuid.Must(uuid.NewV7()), closureID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) PurgeExpiredAccountClosureRetention(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	command, err := r.pool.Exec(ctx, `WITH candidates AS (SELECT user_id FROM account_subject_retention_records WHERE status='RETAINED' AND legal_hold=false AND retained_until<=now() ORDER BY retained_until,user_id FOR UPDATE SKIP LOCKED LIMIT $1) UPDATE account_subject_retention_records record SET status='PURGED',purged_at=now() FROM candidates WHERE record.user_id=candidates.user_id`, limit)
	if err != nil {
		return 0, err
	}
	return command.RowsAffected(), nil
}

func (r *PostgresRepository) AccountClosureState(ctx context.Context, userID uuid.UUID) (string, int64, error) {
	var stage string
	var lifecycleVersion int64
	err := r.pool.QueryRow(ctx, `SELECT stage,COALESCE(subject_lifecycle_version,0) FROM account_closure_requests WHERE user_id=$1`, userID).Scan(&stage, &lifecycleVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrNotFound
	}
	return stage, lifecycleVersion, err
}

func (r *PostgresRepository) MarkAccountClosureReplayCleared(ctx context.Context, item *AccountClosureRequest, lifecycleVersion int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockAccountClosureClaim(ctx, tx, *item); err != nil {
		return err
	}
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE account_closure_requests SET token_replays_cleared_at=COALESCE(token_replays_cleared_at,$2),version=version+1,updated_at=$2 WHERE id=$1 AND user_id=$5 AND stage='IDENTITY_CLEANUP' AND subject_lifecycle_version=$3 AND cleanup_prepared_at IS NOT NULL AND identity_deleted_at IS NOT NULL AND grants_isolated_at IS NOT NULL AND version=$4 AND lease_until>clock_timestamp()`, item.ID, now, lifecycleVersion, item.Version, item.UserID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	item.Version++
	return nil
}

func lockAccountClosureClaim(ctx context.Context, tx pgx.Tx, item AccountClosureRequest) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM account_closure_requests WHERE id=$1 AND user_id=$2 AND version=$3 AND status IN ('IN_PROGRESS','RETRY','BLOCKED') AND lease_until>clock_timestamp() FOR UPDATE`, item.ID, item.UserID, item.Version).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	// A predicate evaluated before a row-lock wait is not a fresh lease proof.
	return requireAccountClosureClaimTx(ctx, tx, item)
}

func requireAccountClosureClaimTx(ctx context.Context, tx pgx.Tx, item AccountClosureRequest) error {
	var live bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_closure_requests WHERE id=$1 AND user_id=$2 AND version=$3 AND lease_until>clock_timestamp() AND status IN ('IN_PROGRESS','RETRY','BLOCKED'))`, item.ID, item.UserID, item.Version).Scan(&live)
	if err == nil && !live {
		return ErrConflict
	}
	return err
}

func (r *PostgresRepository) RequireAccountClosureClaim(ctx context.Context, item AccountClosureRequest) error {
	var live bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_closure_requests WHERE id=$1 AND user_id=$2 AND version=$3 AND lease_until>clock_timestamp() AND status IN ('IN_PROGRESS','RETRY','BLOCKED'))`, item.ID, item.UserID, item.Version).Scan(&live)
	if err == nil && !live {
		return ErrConflict
	}
	return err
}
