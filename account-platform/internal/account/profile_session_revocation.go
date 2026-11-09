package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/profilerevocation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrProfileSessionRevocationUnconfirmed = errors.New("profile session revocation has no confirmed receipt")
var ErrProfileSessionRevocationCapacity = errors.New("profile revocation capacity is exhausted")

const DefaultProfileRevocationCapacity = 4096

// Configure before accepting profile requests or starting recovery workers.
func (r *PostgresRepository) ConfigureProfileRevocationCapacity(capacity int) error {
	if capacity < 1 || capacity > 1000000 {
		return errors.New("profile revocation capacity must be between 1 and 1000000")
	}
	r.profileRevocationCapacity = capacity
	return nil
}

// Persist the first-dispatch decision before contacting the adapter. Retrying
// cannot grant a second first attempt, including after either process restarts.
func (r *PostgresRepository) StartProfileSessionRevocation(ctx context.Context, id uuid.UUID) (bool, error) {
	first := false
	err := r.profileRecoveryWrite(ctx, id, func(tx pgx.Tx) error {
		var c ProfileChange
		var err error
		c, err = scanProfileChange(tx.QueryRow(ctx, profileChangeSelect+` WHERE id=$1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if c.AccountCommittedAt == nil || c.Status == "CANCELLED_ACCOUNT_CLOSURE" || c.Status == "SUPERSEDED" {
			return ErrProfileChangeConflict
		}
		if c.SessionsRevocationStartedAt != nil || c.SessionsRevokedAt != nil {
			return nil
		}
		// Serialize global admission before granting the first attempt. Unknown
		// outcomes keep their slot, while a confirmed receipt releases admission
		// capacity without deleting the durable replay evidence.
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('profile-revocation-admission',0))`); err != nil {
			return err
		}
		capacity := r.profileRevocationCapacity
		if capacity == 0 {
			capacity = DefaultProfileRevocationCapacity
		}
		var pending int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM public.profile_session_revocation_receipts WHERE state<>'CONFIRMED' LIMIT $1) active`, capacity).Scan(&pending); err != nil {
			return err
		}
		if pending >= capacity {
			return ErrProfileSessionRevocationCapacity
		}
		if _, err = tx.Exec(ctx, `INSERT INTO public.profile_session_revocation_receipts(operation_id,state,created_at) VALUES($1,'RESERVED',$2)`, id, r.now()); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE profile_change_requests SET sessions_revocation_started_at=$2,version=version+1,updated_at=$2 WHERE id=$1`, id, r.now()); err != nil {
			return err
		}
		first = true
		return appendAudit(ctx, tx, c.UserID, "", nil, "account.profile_change.revocation_started", "profile_change", id.String(), "SUCCEEDED", map[string]any{"stage": "SESSION_REVOCATION"})
	})
	return first, err
}

func validProfileRevocationCommand(command profilerevocation.Command) bool {
	return command.OperationID != uuid.Nil && command.DispatchToken != uuid.Nil &&
		strings.TrimSpace(command.Subject) != "" && len(command.Subject) <= 128
}

func (r *PostgresRepository) ClaimProfileRevocation(ctx context.Context, command profilerevocation.Command) (profilerevocation.Result, error) {
	result := profilerevocation.Result{}
	if !validProfileRevocationCommand(command) {
		return result, profilerevocation.ErrConflict
	}
	err := r.profileRecoveryWrite(ctx, command.OperationID, func(tx pgx.Tx) error {
		var subject, status string
		var committed, started bool
		if err := tx.QueryRow(ctx, `SELECT identity_subject,status,account_committed_at IS NOT NULL,sessions_revocation_started_at IS NOT NULL FROM profile_change_requests WHERE id=$1`, command.OperationID).Scan(&subject, &status, &committed, &started); err != nil {
			return err
		}
		if subject != command.Subject {
			return profilerevocation.ErrConflict
		}
		if !committed || !started {
			return profilerevocation.ErrUnconfirmed
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM public.profile_session_revocation_receipts WHERE operation_id=$1 FOR UPDATE`, command.OperationID).Scan(&state); err != nil {
			return err
		}
		if state == "CONFIRMED" {
			result.State = "CONFIRMED"
			return nil
		}
		if state != "RESERVED" || command.ReplayOnly {
			return profilerevocation.ErrUnconfirmed
		}
		var userStatus string
		if err := tx.QueryRow(ctx, `SELECT u.status FROM users u JOIN profile_change_requests p ON p.user_id=u.id WHERE p.id=$1`, command.OperationID).Scan(&userStatus); err != nil {
			return err
		}
		if userStatus != "ACTIVE" || status == "CANCELLED_ACCOUNT_CLOSURE" || status == "SUPERSEDED" {
			return profilerevocation.ErrUnconfirmed
		}
		digest := sha256.Sum256(command.DispatchToken[:])
		if _, err := tx.Exec(ctx, `UPDATE public.profile_session_revocation_receipts SET state='DISPATCHED',dispatch_token_hash=$2,dispatched_at=$3 WHERE operation_id=$1`, command.OperationID, digest[:], r.now()); err != nil {
			return err
		}
		result.State = "DISPATCH"
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = profilerevocation.ErrUnconfirmed
	}
	return result, err
}

func (r *PostgresRepository) ConfirmProfileRevocation(ctx context.Context, command profilerevocation.Command) error {
	if !validProfileRevocationCommand(command) {
		return profilerevocation.ErrConflict
	}
	return r.profileRecoveryWrite(ctx, command.OperationID, func(tx pgx.Tx) error {
		var subject string
		if err := tx.QueryRow(ctx, `SELECT identity_subject FROM profile_change_requests WHERE id=$1`, command.OperationID).Scan(&subject); err != nil {
			return err
		}
		if subject != command.Subject {
			return profilerevocation.ErrConflict
		}
		digest := sha256.Sum256(command.DispatchToken[:])
		tag, err := tx.Exec(ctx, `UPDATE public.profile_session_revocation_receipts SET state='CONFIRMED',confirmed_at=COALESCE(confirmed_at,$3) WHERE operation_id=$1 AND dispatch_token_hash=$2 AND state IN ('DISPATCHED','CONFIRMED')`, command.OperationID, digest[:], r.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return profilerevocation.ErrUnconfirmed
		}
		return nil
	})
}
