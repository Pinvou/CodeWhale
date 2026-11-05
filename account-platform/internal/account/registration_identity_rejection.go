package account

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func registrationIdentityRejectionCode(err error) string {
	switch {
	case errors.Is(err, ErrUsernameAlreadyRegistered):
		return "USERNAME_ALREADY_REGISTERED"
	case errors.Is(err, ErrMobileAlreadyRegistered):
		return "MOBILE_ALREADY_REGISTERED"
	default:
		return ""
	}
}

func registrationIdentityRejected(operation RegistrationOperation) bool {
	return operation.Status == "REPAIR_REQUIRED" && operation.IdentityCreateAttemptedAt != nil &&
		operation.IdentitySubject == "" && operation.UserID == nil &&
		(operation.LastErrorCode == "USERNAME_ALREADY_REGISTERED" || operation.LastErrorCode == "MOBILE_ALREADY_REGISTERED")
}

// A rejected create is not proof that creation was never dispatched. Keep the
// original operation/reservations/first grant and expose it for manual review;
// the existing compensation proof must still reject an absent identity subject.
func (r *PostgresRepository) MarkRegistrationIdentityRejected(ctx context.Context, operationID uuid.UUID, code string, firstVersion int64) error {
	if firstVersion <= 0 || code != "USERNAME_ALREADY_REGISTERED" && code != "MOBILE_ALREADY_REGISTERED" {
		return ErrRegistrationConflict
	}
	return r.identityRecoveryWrite(ctx, "registration", operationID, func(tx pgx.Tx) error {
		operation, err := scanRegistrationOperation(tx.QueryRow(ctx, registrationOperationSelect+` WHERE ro.id=$1 FOR UPDATE OF ro`, operationID))
		if err != nil {
			return err
		}
		if operation.Version < firstVersion {
			return ErrRegistrationConflict
		}
		if registrationIdentityRejected(operation) && operation.LastErrorCode == code {
			return nil
		}
		if operation.Status != "IDENTITY_CREATING" || operation.IdentityCreateAttemptedAt == nil || operation.IdentitySubject != "" || operation.UserID != nil {
			return ErrRegistrationConflict
		}
		// A concurrent lookup-only worker may have advanced backoff/version while
		// the one permitted first create was in flight. Compare the locked current
		// version, never overwrite newer identity/account facts, and fence that
		// worker before recording a durable visible rejection.
		command, err := tx.Exec(ctx, `UPDATE registration_operations SET status='REPAIR_REQUIRED',last_error_code=$2,
recovery_lease_until=NULL,next_retry_at=$3,attempts=attempts+1,version=version+1,updated_at=$3
WHERE id=$1 AND version=$4 AND status='IDENTITY_CREATING' AND identity_subject IS NULL AND committed_user_id IS NULL`, operationID, code, r.now(), operation.Version)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return ErrRegistrationConflict
		}
		return appendAdminAuditResult(ctx, tx, nil, identity.Principal{}, nil,
			"registration.identity.create.rejected", "registration_operation", operationID.String(), "FAILED",
			map[string]any{"reason": code, "first_attempt_version": firstVersion, "creation_never_dispatched": false, "manual_review_required": true})
	})
}
