package account

import (
	"context"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Manual inspection never enters the first-create dispatch path. Both sides of
// the read-only remote query require the same durable unknown operation and an
// exclusive recovery claim; a missing result is not evidence of non-creation.
func registrationIdentityUnknown(op RegistrationOperation) bool {
	return op.Status == "IDENTITY_CREATING" && op.IdentityCreateAttemptedAt != nil &&
		op.IdentitySubject == "" && op.IdentityCreatedAt == nil && op.UserID == nil
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
		return nil // Still unknown: keep the existing reservation and retry policy.
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
last_error_code='IDENTITY_UNAVAILABLE',version=version+1,updated_at=$2 WHERE id=$1`, op.ID, now)
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
