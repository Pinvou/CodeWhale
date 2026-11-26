package account

import (
	"context"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const registrationIdentityOutcomeUnknown = "IDENTITY_CREATE_OUTCOME_UNKNOWN"
const registrationIdentityQueryWindow = 30 * time.Minute

func registrationIdentityEscalated(op RegistrationOperation) bool {
	return op.Status == "REPAIR_REQUIRED" && op.LastErrorCode == registrationIdentityOutcomeUnknown
}

// Manual inspection never enters the first-create dispatch path. Both sides of
// the read-only remote query require the same durable unknown operation and an
// exclusive recovery claim; a missing result is not evidence of non-creation.
func registrationIdentityUnknown(op RegistrationOperation) bool {
	return (op.Status == "IDENTITY_CREATING" || registrationIdentityEscalated(op)) &&
		op.IdentityCreateAttemptedAt != nil &&
		op.IdentitySubject == "" && op.IdentityCreatedAt == nil && op.UserID == nil
}

// Compare against the cutoff derived from the cycle clock. An active lease
// may still record a result; no later cycle should lease an overdue unknown.
const registrationIdentityEscalationDue = `
 status='IDENTITY_CREATING' AND identity_create_attempted_at IS NOT NULL AND identity_create_attempted_at<=$3
 AND identity_subject IS NULL AND identity_created_at IS NULL AND committed_user_id IS NULL`

func (r *PostgresRepository) escalateUnknownRegistrationIdentities(ctx context.Context, tx pgx.Tx, now, cutoff time.Time, limit int) error {
	rows, err := tx.Query(ctx, `
WITH due AS (
 SELECT id FROM registration_operations
 WHERE `+registrationIdentityEscalationDue+`
   AND (recovery_lease_until IS NULL OR recovery_lease_until<=$1)
 ORDER BY identity_create_attempted_at,id
 LIMIT $2 FOR UPDATE SKIP LOCKED
)
UPDATE registration_operations ro
SET status='REPAIR_REQUIRED',last_error_code=$4,recovery_lease_until=NULL,
    next_retry_at=$1,version=version+1,updated_at=$1
FROM due WHERE ro.id=due.id
RETURNING ro.id,ro.identity_create_attempted_at`, now, limit, cutoff, registrationIdentityOutcomeUnknown)
	if err != nil {
		return err
	}
	candidates := make([]accountManualReviewCandidate, 0, limit)
	for rows.Next() {
		candidate := accountManualReviewCandidate{
			caseType: "REGISTRATION_REPAIR", resourceType: "REGISTRATION_OPERATION", severity: "P2",
		}
		var attemptedAt time.Time
		if err := rows.Scan(&candidate.resourceID, &attemptedAt); err != nil {
			rows.Close()
			return err
		}
		candidate.createdAt = attemptedAt.Add(registrationIdentityQueryWindow)
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, candidate := range candidates {
		// Publish the case with the status so a quick operator recovery cannot
		// move the operation on before the periodic case scanner observes it.
		if _, err := insertAccountManualReviewCase(ctx, tx, candidate, now); err != nil {
			return err
		}
	}
	return nil
}

type registrationIdentityQueryRepository interface {
	ClaimRegistrationIdentityQuery(context.Context, identity.Principal, RegistrationOperation) (RegistrationOperation, error)
	RecordRegistrationIdentityQuery(context.Context, identity.Principal, RegistrationOperation, string) error
}

func (s *MobileAuthService) queryUnknownRegistration(ctx context.Context, principal identity.Principal, observed RegistrationOperation) error {
	repository, ok := s.repository.(registrationIdentityQueryRepository)
	if !ok || !registrationIdentityUnknown(observed) {
		return ErrRegistrationConflict
	}
	operation, err := repository.ClaimRegistrationIdentityQuery(ctx, principal, observed)
	if err != nil {
		return err
	}
	queryCtx, cancel, err := withIdentityRecoveryClaim(ctx, "registration", operation.ID, operation.RecoveryGeneration, operation.RecoveryLeaseUntil)
	if err != nil {
		return err
	}
	defer cancel()
	subject, found, queryErr := s.identities.FindMobileUserByRegistrationOperation(queryCtx, operation.ID)
	if queryErr != nil || !found {
		subject = ""
	}
	if err = repository.RecordRegistrationIdentityQuery(queryCtx, principal, operation, strings.TrimSpace(subject)); err != nil {
		return err
	}
	if strings.TrimSpace(subject) == "" {
		return nil // Still unknown: keep the reservation and any manual-review state.
	}
	// The only remaining work is the existing idempotent Account commit. Do not
	// reread a status into the generic recovery path that can dispatch Create.
	operation.Status, operation.IdentitySubject = "IDENTITY_CREATED", subject
	_, err = s.resumeRegistration(queryCtx, operation)
	if err == ErrRegistrationRecovering {
		return nil
	}
	return err
}

func (r *PostgresRepository) ClaimRegistrationIdentityQuery(ctx context.Context, principal identity.Principal, observed RegistrationOperation) (RegistrationOperation, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RegistrationOperation{}, err
	}
	defer tx.Rollback(ctx)
	actor, locks, err := registrationQueryAuthorization(ctx, tx, principal)
	if err != nil {
		return RegistrationOperation{}, err
	}
	op, err := scanRegistrationOperation(tx.QueryRow(ctx, registrationOperationSelect+` WHERE ro.id=$1 FOR UPDATE OF ro`, observed.ID))
	if err != nil {
		return RegistrationOperation{}, err
	}
	now, err := approvalDatabaseTime(ctx, tx)
	if err != nil {
		return RegistrationOperation{}, err
	}
	if _, _, _, err = platformAuthorizationAt(ctx, tx, locks, now); err != nil {
		return RegistrationOperation{}, err
	}
	if !registrationIdentityUnknown(op) || op.Version != observed.Version || (op.RecoveryLeaseUntil != nil && op.RecoveryLeaseUntil.After(now)) {
		return RegistrationOperation{}, ErrRegistrationConflict
	}
	until := now.Add(30 * time.Second).Truncate(time.Microsecond)
	op.RecoveryGeneration++
	op.RecoveryLeaseUntil = &until
	if _, err = tx.Exec(ctx, `UPDATE registration_operations SET recovery_generation=$2,recovery_lease_until=$3,next_retry_at=$3,updated_at=$4 WHERE id=$1`, op.ID, op.RecoveryGeneration, until, now); err != nil {
		return RegistrationOperation{}, err
	}
	if err = appendAudit(ctx, tx, actor, principal.SessionID, nil, "registration.repair.identity.query.requested", "registration_operation", op.ID.String(), "SUCCEEDED", nil); err != nil {
		return RegistrationOperation{}, err
	}
	return op, tx.Commit(ctx)
}

func (r *PostgresRepository) RecordRegistrationIdentityQuery(ctx context.Context, principal identity.Principal, claimed RegistrationOperation, subject string) error {
	claim, ok := ctx.Value(identityRecoveryClaimKey{}).(identityRecoveryClaim)
	if !ok || claim.kind != "registration" || claim.id != claimed.ID || claim.generation != claimed.RecoveryGeneration || claimed.RecoveryLeaseUntil == nil || !claim.until.Equal(*claimed.RecoveryLeaseUntil) {
		return ErrIdentityRecoveryClaimLost
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actor, locks, err := registrationQueryAuthorization(ctx, tx, principal)
	if err != nil {
		return err
	}
	if err = r.lockIdentityRecoveryClaim(ctx, tx, "registration", claimed.ID); err != nil {
		return err
	}
	op, err := scanRegistrationOperation(tx.QueryRow(ctx, registrationOperationSelect+` WHERE ro.id=$1 FOR UPDATE OF ro`, claimed.ID))
	if err != nil {
		return err
	}
	now, err := approvalDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	if _, _, _, err = platformAuthorizationAt(ctx, tx, locks, now); err != nil {
		return err
	}
	if !registrationIdentityUnknown(op) || op.Version != claimed.Version || op.RecoveryLeaseUntil == nil || !op.RecoveryLeaseUntil.After(now) {
		return ErrRegistrationConflict
	}
	if subject == "" {
		_, err = tx.Exec(ctx, `UPDATE registration_operations SET attempts=attempts+1,
next_retry_at=$2::timestamptz+LEAST(interval '5 minutes',interval '5 seconds'*power(2,LEAST(attempts,6))),
last_error_code=CASE WHEN status='REPAIR_REQUIRED' THEN last_error_code ELSE 'IDENTITY_UNAVAILABLE' END,
version=version+1,updated_at=$2 WHERE id=$1`, op.ID, now)
	} else {
		_, err = tx.Exec(ctx, `UPDATE registration_operations SET status='IDENTITY_CREATED',identity_subject=$2,
identity_created_at=$3,lease_expires_at=NULL,next_retry_at=$3,last_error_code=NULL,version=version+1,updated_at=$3 WHERE id=$1`, op.ID, subject, now)
	}
	if err != nil {
		return err
	}
	if err = appendAudit(ctx, tx, actor, principal.SessionID, nil, "registration.repair.identity.query", "registration_operation", op.ID.String(), "SUCCEEDED", map[string]any{"identity_found": subject != ""}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Match ordinary platform mutation lock order: actor, role, auth version, then
// registration. Re-evaluate permission against the DB clock after row waits.
func registrationQueryAuthorization(ctx context.Context, tx pgx.Tx, principal identity.Principal) (actorID uuid.UUID, locked lockedPlatformAuthorization, err error) {
	actor, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return actor, locked, err
	}
	locks, err := lockPlatformAuthorizationFacts(ctx, tx, platformAuthorizationRequirement{UserID: actor, Permission: "platform.registrations.repair"})
	if err != nil {
		return actor, locked, err
	}
	return actor, locks[0], nil
}
