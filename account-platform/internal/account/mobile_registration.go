package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The existing audit table is append-only in Account's API: it has no deletion,
// retention worker, or update route. These opaque prepare/complete records are
// committed with typed user/identity facts. They contain no name, phone, password
// or remote credential. An unrelated PROVISIONING user is never a retry intent.
func registrationIssuerDigest(issuer string) string {
	digest := sha256.Sum256([]byte(issuer))
	return hex.EncodeToString(digest[:])
}

const pendingMobileRegistrationSQL = `
SELECT u.id,u.username,u.mobile_e164 FROM users u
WHERE ($1::text IS NULL OR u.username=$1) AND u.mobile_e164=$2 AND u.status='PROVISIONING'
AND NOT EXISTS(SELECT 1 FROM identities i WHERE i.user_id=u.id)
AND EXISTS(SELECT 1 FROM audit_logs a WHERE a.resource_type='mobile_registration' AND a.resource_id=u.id::text
 AND a.actor_user_id=u.id AND a.action='account.mobile.registration.prepare' AND a.result='SUCCEEDED'
 AND a.change_summary->>'issuer_digest'=$3)
AND NOT EXISTS(SELECT 1 FROM audit_logs a WHERE a.resource_type='mobile_registration' AND a.resource_id=u.id::text
 AND a.action='account.mobile.registration.complete' AND a.result='SUCCEEDED')`

func (r *PostgresRepository) PendingMobileRegistration(ctx context.Context, issuer, username, mobile string) (MobileRegistration, error) {
	var registration MobileRegistration
	err := r.pool.QueryRow(ctx, pendingMobileRegistrationSQL, username, mobile, registrationIssuerDigest(issuer)).Scan(&registration.UserID, &registration.Username, &registration.Mobile)
	return registration, err
}

// Captcha creation has no username. Only an unfinished, explicitly prepared
// registration for this issuer may pass the existing-mobile gate. The later
// precheck and SMS send still require the exact username and phone facts.
func (r *PostgresRepository) MobileRegistrationRecoverable(ctx context.Context, issuer, mobile string) (bool, error) {
	var recoverable bool
	err := r.pool.QueryRow(ctx, "SELECT EXISTS("+pendingMobileRegistrationSQL+")", nil, mobile, registrationIssuerDigest(issuer)).Scan(&recoverable)
	return recoverable, err
}

func (r *PostgresRepository) PrepareMobileRegistration(ctx context.Context, issuer, username, mobile string) (MobileRegistration, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return MobileRegistration{}, err
	}
	defer tx.Rollback(ctx)
	// Serializing the phone first and username second also covers registrations
	// whose facts only overlap in one field. The following read sees new commits.
	for _, key := range []string{"registration-mobile:" + mobile, "registration-username:" + username} {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return MobileRegistration{}, err
		}
	}
	var registration MobileRegistration
	err = tx.QueryRow(ctx, pendingMobileRegistrationSQL+" FOR UPDATE OF u", username, mobile, registrationIssuerDigest(issuer)).Scan(&registration.UserID, &registration.Username, &registration.Mobile)
	if err == nil {
		return registration, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return MobileRegistration{}, err
	}
	var usernameExists, mobileExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(username)=lower($1)), EXISTS(SELECT 1 FROM users WHERE mobile_e164=$2 AND status<>'DELETED')`, username, mobile).Scan(&usernameExists, &mobileExists); err != nil {
		return MobileRegistration{}, err
	}
	if usernameExists {
		return MobileRegistration{}, ErrUsernameAlreadyRegistered
	}
	if mobileExists {
		return MobileRegistration{}, ErrMobileAlreadyRegistered
	}
	registration = MobileRegistration{UserID: uuid.Must(uuid.NewV7()), Username: username, Mobile: mobile}
	now := r.now()
	if _, err := tx.Exec(ctx, `INSERT INTO users(id,status,username,display_name,mobile_e164,mobile_verified_at,created_at,updated_at) VALUES($1,'PROVISIONING',$2,$2,$3,$4,$4,$4)`, registration.UserID, username, mobile, now); err != nil {
		return MobileRegistration{}, err
	}
	if err := appendAudit(ctx, tx, registration.UserID, "registration", nil, "account.mobile.registration.prepare", "mobile_registration", registration.UserID.String(), "SUCCEEDED", map[string]any{"issuer_digest": registrationIssuerDigest(issuer)}); err != nil {
		return MobileRegistration{}, err
	}
	return registration, tx.Commit(ctx)
}

func (r *PostgresRepository) CompleteMobileRegistration(ctx context.Context, issuer string, registration MobileRegistration, subject string, commerce bool) error {
	if registration.UserID == uuid.Nil || strings.TrimSpace(subject) == "" || len(subject) > 128 {
		return ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var username, mobile, status string
	if err := tx.QueryRow(ctx, `SELECT username,mobile_e164,status FROM users WHERE id=$1 FOR UPDATE`, registration.UserID).Scan(&username, &mobile, &status); err != nil {
		return err
	}
	if username != registration.Username || mobile != registration.Mobile || status != "PROVISIONING" && status != "ACTIVE" {
		return ErrConflict
	}
	var prepared, complete bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_logs WHERE resource_type='mobile_registration' AND resource_id=$1 AND actor_user_id=$2 AND action='account.mobile.registration.prepare' AND result='SUCCEEDED' AND change_summary->>'issuer_digest'=$3), EXISTS(SELECT 1 FROM audit_logs WHERE resource_type='mobile_registration' AND resource_id=$1 AND action='account.mobile.registration.complete' AND result='SUCCEEDED')`, registration.UserID.String(), registration.UserID, registrationIssuerDigest(issuer)).Scan(&prepared, &complete); err != nil {
		return err
	}
	if !prepared {
		return ErrConflict
	}
	var bound string
	err = tx.QueryRow(ctx, `SELECT subject FROM identities WHERE user_id=$1 AND issuer=$2`, registration.UserID, issuer).Scan(&bound)
	if complete {
		if err != nil || bound != subject {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			return err
		}
		return ErrConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4)`, issuer, subject, registration.UserID, now); err != nil {
		return err
	}
	if commerce {
		if _, err := tx.Exec(ctx, `INSERT INTO mall_user_sync_jobs(user_id,mobile_e164,status,next_attempt_at,created_at,updated_at) VALUES($1,$2,'PENDING',$3,$3,$3)`, registration.UserID, registration.Mobile, now); err != nil {
			return err
		}
	}
	if err := appendAudit(ctx, tx, registration.UserID, "registration", nil, "account.mobile.registration.complete", "mobile_registration", registration.UserID.String(), "SUCCEEDED", map[string]any{"mobile_verified": true}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
