package securityadmin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	BootstrapUsername = "admin"
	maxFailures       = 5
)

var (
	ErrCredentialInvalid       = errors.New("bootstrap credential is invalid")
	ErrCredentialExpired       = errors.New("bootstrap credential has expired")
	ErrCredentialLocked        = errors.New("bootstrap credential is locked")
	ErrBreakGlassNotConfirmed  = errors.New("all super administrators being unreachable must be explicitly confirmed")
	ErrBreakGlassReasonInvalid = errors.New("break-glass reason must contain 20 to 2000 characters")
)

type IdentityDirectory interface {
	EnsureBootstrapAdmin(context.Context, string) (string, error)
	ActivateBootstrapAdmin(context.Context, string, string, string) error
	DisableUserAndConfirm(context.Context, string) error
}

type Manager struct {
	pool      *pgxpool.Pool
	directory IdentityDirectory
	issuer    string
	now       func() time.Time
}

type BootstrapIssue struct {
	CredentialID uuid.UUID
	Generation   int64
	Secret       string
	ExpiresAt    time.Time
	Activated    bool
}

type BreakGlassRequest struct {
	WorkloadIdentity string
	TargetUsername   string
	Reason           string
	Evidence         []byte
	Confirmation     string
	Duration         time.Duration
	FormalPassword   string
}

func NewManager(ctx context.Context, databaseURL, issuer string, directory IdentityDirectory) (*Manager, error) {
	if strings.TrimSpace(databaseURL) == "" || strings.TrimSpace(issuer) == "" || directory == nil {
		return nil, errors.New("security administration is not fully configured")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Manager{pool: pool, directory: directory, issuer: strings.TrimSpace(issuer), now: func() time.Time { return time.Now().UTC() }}, nil
}

func (m *Manager) Close() { m.pool.Close() }

func GenerateBootstrapCredential() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(random)
	return secret, nil
}

func bootstrapVerifier(deploymentIdentity string, generation int64, secret string) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("SUPER_ADMIN_ACTIVATION\x00%s\x00%d\x00%s", deploymentIdentity, generation, secret)))
}

func (m *Manager) IssueBootstrap(ctx context.Context, deploymentIdentity, mode string, ttl time.Duration) (BootstrapIssue, error) {
	deploymentIdentity = strings.TrimSpace(deploymentIdentity)
	mode = strings.ToUpper(strings.TrimSpace(mode))
	if len(deploymentIdentity) < 3 || len(deploymentIdentity) > 200 {
		return BootstrapIssue{}, errors.New("deployment identity must contain 3 to 200 characters")
	}
	if mode != "INTERACTIVE_SET_PASSWORD" && mode != "ONE_TIME_BOOTSTRAP" {
		return BootstrapIssue{}, errors.New("unsupported bootstrap mode")
	}
	if ttl == 0 {
		ttl = 15 * time.Minute
	}
	if ttl < time.Minute || ttl > 60*time.Minute {
		return BootstrapIssue{}, errors.New("bootstrap TTL must be between 1 and 60 minutes")
	}
	subject, err := m.directory.EnsureBootstrapAdmin(ctx, BootstrapUsername)
	if err != nil {
		return BootstrapIssue{}, err
	}
	var secret string
	if mode == "ONE_TIME_BOOTSTRAP" {
		value, err := GenerateBootstrapCredential()
		if err != nil {
			return BootstrapIssue{}, err
		}
		secret = value
	}
	now := m.now()
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return BootstrapIssue{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('pinvou-super-admin-bootstrap'))`); err != nil {
		return BootstrapIssue{}, err
	}
	userID, activated, err := ensureBootstrapUser(ctx, tx, m.issuer, subject, now)
	if err != nil || activated {
		if err == nil {
			err = tx.Commit(ctx)
		}
		return BootstrapIssue{Activated: activated}, err
	}
	// A pre-activation identity must remain unable to establish a session even
	// after a previous interrupted deployment attempt.
	if err := m.directory.DisableUserAndConfirm(ctx, subject); err != nil {
		return BootstrapIssue{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE super_admin_bootstrap_credentials SET status='SUPERSEDED',verifier=NULL,updated_at=$2 WHERE deployment_identity=$1 AND status IN ('PENDING','REDEEMING')`, deploymentIdentity, now); err != nil {
		return BootstrapIssue{}, err
	}
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(deployment_generation),0)+1 FROM super_admin_bootstrap_credentials WHERE deployment_identity=$1`, deploymentIdentity).Scan(&generation); err != nil {
		return BootstrapIssue{}, err
	}
	id := uuid.Must(uuid.NewV7())
	expiresAt := now.Add(ttl)
	var verifierBytes []byte
	if secret != "" {
		verifier := bootstrapVerifier(deploymentIdentity, generation, secret)
		verifierBytes = verifier[:]
	}
	if _, err := tx.Exec(ctx, `INSERT INTO super_admin_bootstrap_credentials(id,deployment_identity,deployment_generation,user_id,identity_subject,mode,verifier,status,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,'PENDING',$8,$9,$9)`, id, deploymentIdentity, generation, userID, subject, mode, verifierBytes, expiresAt, now); err != nil {
		return BootstrapIssue{}, err
	}
	if err := appendSecurityAudit(ctx, tx, userID, "security.super-admin.bootstrap.issued", id.String(), map[string]any{"deployment_identity": deploymentIdentity, "generation": generation, "mode": mode, "expires_at": expiresAt}, now); err != nil {
		return BootstrapIssue{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BootstrapIssue{}, err
	}
	return BootstrapIssue{CredentialID: id, Generation: generation, Secret: secret, ExpiresAt: expiresAt}, nil
}

func (m *Manager) MarkDelivered(ctx context.Context, credentialID uuid.UUID) error {
	command, err := m.pool.Exec(ctx, `UPDATE super_admin_bootstrap_credentials SET delivery_consumed_at=COALESCE(delivery_consumed_at,$2),updated_at=$2 WHERE id=$1 AND mode='ONE_TIME_BOOTSTRAP' AND status='PENDING'`, credentialID, m.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrCredentialInvalid
	}
	return nil
}

func (m *Manager) ActivateInteractive(ctx context.Context, deploymentIdentity, password string) error {
	return m.activate(ctx, deploymentIdentity, "", password, "INTERACTIVE_SET_PASSWORD")
}

func (m *Manager) Redeem(ctx context.Context, deploymentIdentity, credential, password string) error {
	return m.activate(ctx, deploymentIdentity, credential, password, "ONE_TIME_BOOTSTRAP")
}

func (m *Manager) activate(ctx context.Context, deploymentIdentity, credential, password, mode string) error {
	if err := identityadapter.ValidatePrivilegedPassword(password, BootstrapUsername); err != nil {
		return err
	}
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('pinvou-super-admin-bootstrap'))`); err != nil {
		return err
	}
	var id, userID uuid.UUID
	var subject, status string
	var verifier []byte
	var expiresAt time.Time
	var deliveredAt *time.Time
	var failures int
	var generation int64
	deploymentIdentity = strings.TrimSpace(deploymentIdentity)
	err = tx.QueryRow(ctx, `SELECT id,user_id,identity_subject,status,verifier,expires_at,delivery_consumed_at,failed_attempts,deployment_generation FROM super_admin_bootstrap_credentials WHERE deployment_identity=$1 AND mode=$2 ORDER BY deployment_generation DESC LIMIT 1 FOR UPDATE`, deploymentIdentity, mode).Scan(&id, &userID, &subject, &status, &verifier, &expiresAt, &deliveredAt, &failures, &generation)
	if errors.Is(err, pgx.ErrNoRows) || status != "PENDING" {
		return ErrCredentialInvalid
	}
	if err != nil {
		return err
	}
	now := m.now()
	if !expiresAt.After(now) {
		_, _ = tx.Exec(ctx, `UPDATE super_admin_bootstrap_credentials SET status='EXPIRED',verifier=NULL,updated_at=$2 WHERE id=$1`, id, now)
		_ = appendSecurityAlert(ctx, tx, "HIGH", "SUPER_ADMIN_BOOTSTRAP_EXPIRED", "bootstrap_credential", id.String(), map[string]any{}, now)
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return ErrCredentialExpired
	}
	if mode == "ONE_TIME_BOOTSTRAP" {
		if deliveredAt == nil {
			return ErrCredentialInvalid
		}
		candidate := bootstrapVerifier(deploymentIdentity, generation, credential)
		if len(verifier) != sha256.Size || !hmac.Equal(verifier, candidate[:]) {
			failures++
			newStatus := "PENDING"
			if failures >= maxFailures {
				newStatus = "LOCKED"
			}
			_, _ = tx.Exec(ctx, `UPDATE super_admin_bootstrap_credentials SET failed_attempts=$2,status=$3,verifier=CASE WHEN $3='LOCKED' THEN NULL ELSE verifier END,updated_at=$4 WHERE id=$1`, id, failures, newStatus, now)
			_ = appendSecurityAudit(ctx, tx, userID, "security.super-admin.bootstrap.denied", id.String(), map[string]any{"failed_attempts": failures, "locked": newStatus == "LOCKED"}, now)
			if newStatus == "LOCKED" {
				_ = appendSecurityAlert(ctx, tx, "CRITICAL", "SUPER_ADMIN_BOOTSTRAP_LOCKED", "bootstrap_credential", id.String(), map[string]any{"failed_attempts": failures}, now)
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			if newStatus == "LOCKED" {
				return ErrCredentialLocked
			}
			return ErrCredentialInvalid
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE super_admin_bootstrap_credentials SET status='REDEEMING',updated_at=$2 WHERE id=$1`, id, now); err != nil {
		return err
	}
	if err := m.directory.ActivateBootstrapAdmin(ctx, subject, BootstrapUsername, password); err != nil {
		return err
	}
	identityActivated, committed := true, false
	defer func() {
		if identityActivated && !committed {
			compensationContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = m.directory.DisableUserAndConfirm(compensationContext, subject)
		}
	}()
	if _, err := tx.Exec(ctx, `UPDATE users SET status='ACTIVE',version=version+1,membership_version=membership_version+1,updated_at=$2 WHERE id=$1 AND status='BOOTSTRAP_PENDING'`, userID, now); err != nil {
		return err
	}
	assignmentID := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,created_by,source,grant_source,starts_at,created_at,idempotency_key) VALUES($1,$2,'PLATFORM_SUPER_ADMIN',$2,'SECURITY_BOOTSTRAP','SECURITY_BOOTSTRAP',$3,$3,$4) ON CONFLICT DO NOTHING`, assignmentID, userID, now, fmt.Sprintf("bootstrap-%s", id)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE super_admin_bootstrap_credentials SET status='CONSUMED',verifier=NULL,consumed_at=$2,updated_at=$2 WHERE id=$1`, id, now); err != nil {
		return err
	}
	if err := appendSecurityAudit(ctx, tx, userID, "security.super-admin.bootstrap.activated", id.String(), map[string]any{"mode": mode, "sessions_revoked": true}, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

func (m *Manager) BreakGlass(ctx context.Context, request BreakGlassRequest) error {
	request.WorkloadIdentity = strings.TrimSpace(request.WorkloadIdentity)
	request.TargetUsername = strings.TrimSpace(request.TargetUsername)
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Confirmation != "ALL_SUPER_ADMINS_UNREACHABLE" {
		return ErrBreakGlassNotConfirmed
	}
	if len(request.Reason) < 20 || len(request.Reason) > 2000 {
		return ErrBreakGlassReasonInvalid
	}
	if len(request.WorkloadIdentity) < 3 || len(request.WorkloadIdentity) > 200 || len(request.Evidence) == 0 {
		return errors.New("workload identity and incident evidence are required")
	}
	if request.Duration == 0 {
		request.Duration = 30 * time.Minute
	}
	if request.Duration < 5*time.Minute || request.Duration > 60*time.Minute {
		return errors.New("break-glass duration must be between 5 and 60 minutes")
	}
	if err := identityadapter.ValidatePrivilegedPassword(request.FormalPassword, request.TargetUsername); err != nil {
		return err
	}
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('pinvou-super-admin-break-glass'))`); err != nil {
		return err
	}
	var userID uuid.UUID
	var subject string
	err = tx.QueryRow(ctx, `SELECT u.id,i.subject FROM users u JOIN identities i ON i.user_id=u.id AND i.issuer=$2 WHERE lower(u.username)=lower($1) AND u.status<>'DELETED' FOR UPDATE OF u`, request.TargetUsername, m.issuer).Scan(&userID, &subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("break-glass target was not found")
	}
	if err != nil {
		return err
	}
	if err := m.directory.ActivateBootstrapAdmin(ctx, subject, request.TargetUsername, request.FormalPassword); err != nil {
		return err
	}
	identityActivated, committed := true, false
	defer func() {
		if identityActivated && !committed {
			compensationContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = m.directory.DisableUserAndConfirm(compensationContext, subject)
		}
	}()
	now := m.now()
	expiresAt := now.Add(request.Duration)
	if _, err := tx.Exec(ctx, `UPDATE users SET status='ACTIVE',version=version+1,membership_version=membership_version+1,updated_at=$2 WHERE id=$1`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE role_assignments SET revoked_at=expires_at WHERE user_id=$1 AND role_code='PLATFORM_SUPER_ADMIN' AND organization_id IS NULL AND revoked_at IS NULL AND expires_at<=now()`, userID); err != nil {
		return err
	}
	var assignmentID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM role_assignments WHERE user_id=$1 AND role_code='PLATFORM_SUPER_ADMIN' AND organization_id IS NULL AND revoked_at IS NULL AND starts_at<=now() AND (expires_at IS NULL OR expires_at>now()) ORDER BY expires_at NULLS LAST LIMIT 1`, userID).Scan(&assignmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		assignmentID = uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,created_by,source,grant_source,starts_at,expires_at,created_at,idempotency_key) VALUES($1,$2,'PLATFORM_SUPER_ADMIN',$2,'BREAK_GLASS','BREAK_GLASS',$3,$4,$3,$5)`, assignmentID, userID, now, expiresAt, "break-glass-"+assignmentID.String()); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	operationID := uuid.Must(uuid.NewV7())
	evidenceDigest := sha256.Sum256(request.Evidence)
	if _, err := tx.Exec(ctx, `INSERT INTO break_glass_operations(id,workload_identity,target_user_id,role_assignment_id,reason,unreachable_evidence_sha256,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, operationID, request.WorkloadIdentity, userID, assignmentID, request.Reason, evidenceDigest[:], expiresAt, now); err != nil {
		return err
	}
	details := map[string]any{"workload_identity": request.WorkloadIdentity, "target_user_id": userID, "reason": request.Reason, "expires_at": expiresAt, "role_assignment_id": assignmentID}
	if err := appendSecurityAlert(ctx, tx, "CRITICAL", "BREAK_GLASS_ACTIVATED", "break_glass_operation", operationID.String(), details, now); err != nil {
		return err
	}
	if err := appendSecurityAudit(ctx, tx, userID, "security.break-glass.activated", operationID.String(), details, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

func ensureBootstrapUser(ctx context.Context, tx pgx.Tx, issuer, subject string, now time.Time) (uuid.UUID, bool, error) {
	var userID uuid.UUID
	var status, existingSubject string
	err := tx.QueryRow(ctx, `SELECT u.id,u.status,COALESCE(i.subject,'') FROM users u LEFT JOIN identities i ON i.user_id=u.id AND i.issuer=$2 WHERE lower(u.username)=lower($1) FOR UPDATE OF u`, BootstrapUsername, issuer).Scan(&userID, &status, &existingSubject)
	if errors.Is(err, pgx.ErrNoRows) {
		userID = uuid.Must(uuid.NewV7())
		status = "BOOTSTRAP_PENDING"
		_, err = tx.Exec(ctx, `INSERT INTO users(id,user_code,status,username,display_name,membership_version,version,created_at,updated_at) VALUES($1,generate_unique_user_code(),'BOOTSTRAP_PENDING',$2,$2,1,1,$3,$3)`, userID, BootstrapUsername, now)
		if err != nil {
			return uuid.Nil, false, err
		}
	} else if err != nil {
		return uuid.Nil, false, err
	}
	if existingSubject != "" && existingSubject != subject {
		return uuid.Nil, false, errors.New("bootstrap Account user is linked to a different Identity subject")
	}
	var subjectOwner *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT (SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2)`, issuer, subject).Scan(&subjectOwner); err != nil {
		return uuid.Nil, false, err
	}
	if subjectOwner != nil && *subjectOwner != userID {
		return uuid.Nil, false, errors.New("bootstrap Identity subject is linked to a different Account user")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4) ON CONFLICT (issuer,subject) DO UPDATE SET last_seen_at=EXCLUDED.last_seen_at`, issuer, subject, userID, now); err != nil {
		return uuid.Nil, false, err
	}
	var activated bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_assignments WHERE user_id=$1 AND role_code='PLATFORM_SUPER_ADMIN' AND organization_id IS NULL AND revoked_at IS NULL AND starts_at<=now() AND (expires_at IS NULL OR expires_at>now()))`, userID).Scan(&activated); err != nil {
		return uuid.Nil, false, err
	}
	if !activated && status != "BOOTSTRAP_PENDING" {
		_, err = tx.Exec(ctx, `UPDATE users SET status='BOOTSTRAP_PENDING',updated_at=$2 WHERE id=$1`, userID, now)
	}
	return userID, activated, err
}

func appendSecurityAudit(ctx context.Context, tx pgx.Tx, actor uuid.UUID, action, resourceID string, details map[string]any, now time.Time) error {
	payload, _ := json.Marshal(details)
	_, err := tx.Exec(ctx, `INSERT INTO audit_logs(id,actor_user_id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at) VALUES($1,$2,$3,'security_recovery',$4,'SUCCEEDED',$5,$5,$6,$7)`, uuid.Must(uuid.NewV7()), actor, action, resourceID, "offline-cli-"+uuid.NewString(), payload, now)
	return err
}

func appendSecurityAlert(ctx context.Context, tx pgx.Tx, severity, eventType, resourceType, resourceID string, details map[string]any, now time.Time) error {
	payload, _ := json.Marshal(details)
	_, err := tx.Exec(ctx, `INSERT INTO security_alerts(id,severity,event_type,resource_type,resource_id,details,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, uuid.Must(uuid.NewV7()), severity, eventType, resourceType, resourceID, payload, now)
	return err
}
