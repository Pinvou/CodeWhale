package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	accountClosureConfirmation  = "CLOSE_MY_ACCOUNT"
	accountClosurePolicyVersion = 1
	accountClosureRecentAuth    = 5 * time.Minute
	accountClosureSLA           = 30 * time.Minute
)

type AccountClosureRequest struct {
	ID                uuid.UUID               `json:"closure_request_id"`
	UserID            uuid.UUID               `json:"user_id,omitempty"`
	Status            string                  `json:"status"`
	Stage             string                  `json:"stage"`
	PolicyVersion     int                     `json:"policy_version"`
	Blockers          []AccountClosureBlocker `json:"blockers"`
	Attempts          int                     `json:"attempts"`
	LastErrorCode     string                  `json:"last_error_code,omitempty"`
	NextAttemptAt     *time.Time              `json:"next_attempt_at,omitempty"`
	ReauthenticatedAt time.Time               `json:"reauthenticated_at"`
	SLAExpiresAt      time.Time               `json:"sla_expires_at"`
	Version           int64                   `json:"version"`
	CreatedAt         time.Time               `json:"created_at"`
	UpdatedAt         time.Time               `json:"updated_at"`
	CompletedAt       *time.Time              `json:"completed_at,omitempty"`
	Replayed          bool                    `json:"replayed"`
}

type AccountClosureRepairFilter struct {
	Status string
	Stage  string
}

type AccountClosurePublic struct {
	ID                uuid.UUID               `json:"closure_request_id"`
	Status            string                  `json:"status"`
	Stage             string                  `json:"stage"`
	PolicyVersion     int                     `json:"policy_version"`
	Blockers          []AccountClosureBlocker `json:"blockers"`
	Attempts          int                     `json:"attempts"`
	LastErrorCode     string                  `json:"last_error_code,omitempty"`
	NextAttemptAt     *time.Time              `json:"next_attempt_at,omitempty"`
	ReauthenticatedAt time.Time               `json:"reauthenticated_at"`
	SLAExpiresAt      time.Time               `json:"sla_expires_at"`
	Version           int64                   `json:"version"`
	CreatedAt         time.Time               `json:"created_at"`
	UpdatedAt         time.Time               `json:"updated_at"`
	CompletedAt       *time.Time              `json:"completed_at,omitempty"`
	Replayed          bool                    `json:"replayed"`
}

func closureDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

const accountClosureSelect = `SELECT id,user_id,status,stage,policy_version,blocker_snapshot,attempts,COALESCE(last_error_code,''),next_attempt_at,reauthenticated_at,sla_expires_at,version,created_at,updated_at,completed_at FROM account_closure_requests`

func scanAccountClosure(row interface{ Scan(...any) error }) (AccountClosureRequest, error) {
	var item AccountClosureRequest
	var blockers json.RawMessage
	err := row.Scan(&item.ID, &item.UserID, &item.Status, &item.Stage, &item.PolicyVersion, &blockers, &item.Attempts, &item.LastErrorCode, &item.NextAttemptAt, &item.ReauthenticatedAt, &item.SLAExpiresAt, &item.Version, &item.CreatedAt, &item.UpdatedAt, &item.CompletedAt)
	if err == nil && len(blockers) != 0 {
		err = json.Unmarshal(blockers, &item.Blockers)
	}
	if item.Blockers == nil {
		item.Blockers = []AccountClosureBlocker{}
	}
	return item, err
}

func (r *PostgresRepository) CreateAccountClosure(ctx context.Context, principal identity.Principal, idempotencyKey, confirmation string) (AccountClosureRequest, error) {
	idempotencyKey, confirmation = strings.TrimSpace(idempotencyKey), strings.TrimSpace(confirmation)
	now := r.now()
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 128 || confirmation != accountClosureConfirmation || principal.AuthenticatedAt.IsZero() || principal.AuthenticatedAt.After(now.Add(time.Minute)) || now.Sub(principal.AuthenticatedAt) > accountClosureRecentAuth {
		return AccountClosureRequest{}, ErrForbidden
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AccountClosureRequest{}, err
	}
	defer tx.Rollback(ctx)
	var userID uuid.UUID
	var userStatus string
	if err := tx.QueryRow(ctx, `SELECT u.id,u.status FROM identities identity JOIN users u ON u.id=identity.user_id WHERE identity.issuer=$1 AND identity.subject=$2 FOR UPDATE OF u`, principal.Issuer, principal.Subject).Scan(&userID, &userStatus); errors.Is(err, pgx.ErrNoRows) {
		return AccountClosureRequest{}, ErrNotFound
	} else if err != nil {
		return AccountClosureRequest{}, err
	}
	if existing, err := scanAccountClosure(tx.QueryRow(ctx, accountClosureSelect+` WHERE user_id=$1`, userID)); err == nil {
		existing.Replayed = true
		return existing, tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return AccountClosureRequest{}, err
	}
	if userStatus != "ACTIVE" {
		return AccountClosureRequest{}, ErrConflict
	}
	blockers, gateErr := accountClosureRoleBlockers(checkAccountClosureRoleGateTx(ctx, tx, userID))
	if gateErr != nil {
		return AccountClosureRequest{}, gateErr
	}
	request := AccountClosureRequest{ID: uuid.Must(uuid.NewV7()), UserID: userID, Status: "IN_PROGRESS", Stage: "CLOSURE_REQUESTED", PolicyVersion: accountClosurePolicyVersion, Blockers: blockers, ReauthenticatedAt: principal.AuthenticatedAt.UTC(), SLAExpiresAt: now.Add(accountClosureSLA), Version: 1, CreatedAt: now, UpdatedAt: now}
	if len(blockers) != 0 {
		request.Status = "BLOCKED"
	}
	encodedBlockers, err := json.Marshal(blockers)
	if err != nil {
		return AccountClosureRequest{}, err
	}
	keyDigest := closureDigest(idempotencyKey)
	fingerprint := closureDigest("account-closure-v1\n" + confirmation)
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_requests(id,user_id,idempotency_key_digest,request_fingerprint,status,stage,policy_version,blocker_snapshot,reauthenticated_at,sla_expires_at,version,created_at,updated_at,next_attempt_at) VALUES($1,$2,$3,$4,$5,'CLOSURE_REQUESTED',$6,$7,$8,$9,1,$10,$10,$10)`, request.ID, userID, keyDigest, fingerprint, request.Status, accountClosurePolicyVersion, encodedBlockers, request.ReauthenticatedAt, request.SLAExpiresAt, now); err != nil {
		return AccountClosureRequest{}, err
	}
	if len(blockers) == 0 {
		if _, err := tx.Exec(ctx, `UPDATE account_closure_requests SET stage='QUIESCING',version=2,updated_at=$2 WHERE id=$1`, request.ID, now); err != nil {
			return AccountClosureRequest{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET status='CLOSING',version=version+1,updated_at=$2 WHERE id=$1 AND status='ACTIVE'`, userID, now); err != nil {
			return AccountClosureRequest{}, err
		}
		request.Stage, request.Version = "QUIESCING", 2
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_audit(id,closure_request_id,event_type,actor_user_id,from_stage,to_stage,redacted_details,occurred_at) VALUES($1,$2,'REQUESTED',$3,'CLOSURE_REQUESTED',$4,jsonb_build_object('policy_version',$5,'recent_auth',true,'pre_quiet_blocker_codes',(SELECT COALESCE(jsonb_agg(value->>'code'),'[]'::jsonb) FROM jsonb_array_elements($6::jsonb) value)),$7)`, uuid.Must(uuid.NewV7()), request.ID, userID, request.Stage, accountClosurePolicyVersion, encodedBlockers, now); err != nil {
		return AccountClosureRequest{}, err
	}
	return request, tx.Commit(ctx)
}

func (r *PostgresRepository) AccountClosureForPrincipal(ctx context.Context, principal identity.Principal) (AccountClosureRequest, error) {
	item, err := scanAccountClosure(r.pool.QueryRow(ctx, accountClosureSelect+` WHERE user_id=(SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2)`, principal.Issuer, principal.Subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountClosureRequest{}, ErrNotFound
	}
	return item, err
}

func (r *PostgresRepository) ClaimAccountClosure(ctx context.Context) (*AccountClosureRequest, error) {
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	item, err := scanAccountClosure(tx.QueryRow(ctx, accountClosureSelect+` WHERE status IN ('IN_PROGRESS','RETRY','BLOCKED') AND stage IN ('CLOSURE_REQUESTED','QUIESCING','FINANCIAL_CLEARANCE','IDENTITY_CLEANUP','ANONYMIZED') AND next_attempt_at<=$1 AND (lease_until IS NULL OR lease_until<$1) ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE account_closure_requests SET lease_until=$2,attempts=attempts+1,version=version+1,updated_at=$1 WHERE id=$3`, now, now.Add(30*time.Second), item.ID); err != nil {
		return nil, err
	}
	item.Attempts++
	item.Version++
	item.UpdatedAt = now
	return &item, tx.Commit(ctx)
}

func accountClosureRoleBlockers(err error) ([]AccountClosureBlocker, error) {
	switch {
	case err == nil:
		return []AccountClosureBlocker{}, nil
	case errors.Is(err, ErrLastOwner):
		return []AccountClosureBlocker{{Code: "ORGANIZATION_OWNERSHIP_REQUIRED", Domain: "ACCOUNT", ResourceType: "ORGANIZATION", UserAction: "TRANSFER_OWNERSHIP"}}, nil
	case errors.Is(err, ErrLastActiveSuperAdmin):
		return []AccountClosureBlocker{{Code: "LAST_ACTIVE_SUPER_ADMIN", Domain: "ACCOUNT", ResourceType: "PLATFORM_ROLE", UserAction: "HANDOFF_SUPER_ADMIN"}}, nil
	default:
		return nil, err
	}
}

func (r *PostgresRepository) SaveAccountClosureRequestedBlockers(ctx context.Context, item AccountClosureRequest, blockers []AccountClosureBlocker) error {
	now := r.now()
	encoded, err := json.Marshal(blockers)
	if err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE account_closure_requests SET status='BLOCKED',blocker_snapshot=$2,next_attempt_at=$3,lease_until=NULL,last_error_code=NULL,version=version+1,updated_at=$4 WHERE id=$1 AND version=$5 AND stage='CLOSURE_REQUESTED'`, item.ID, encoded, now.Add(time.Minute), now, item.Version)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_audit(id,closure_request_id,event_type,from_stage,to_stage,redacted_details,occurred_at) VALUES($1,$2,'PRE_QUIET_BLOCKERS_EVALUATED','CLOSURE_REQUESTED','CLOSURE_REQUESTED',jsonb_build_object('blocker_codes',(SELECT COALESCE(jsonb_agg(value->>'code'),'[]'::jsonb) FROM jsonb_array_elements($3::jsonb) value)),$4)`, uuid.Must(uuid.NewV7()), item.ID, encoded, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) EnterAccountClosureQuiescing(ctx context.Context, item AccountClosureRequest) error {
	now := r.now()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := checkAccountClosureRoleGateTx(ctx, tx, item.UserID); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE account_closure_requests SET status='IN_PROGRESS',stage='QUIESCING',blocker_snapshot='[]'::jsonb,next_attempt_at=$2,lease_until=NULL,last_error_code=NULL,version=version+1,updated_at=$2 WHERE id=$1 AND version=$3 AND stage='CLOSURE_REQUESTED'`, item.ID, now, item.Version)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	userCommand, err := tx.Exec(ctx, `UPDATE users SET status='CLOSING',version=version+1,updated_at=$2 WHERE id=$1 AND status='ACTIVE'`, item.UserID, now)
	if err != nil {
		return err
	}
	if userCommand.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_audit(id,closure_request_id,event_type,from_stage,to_stage,redacted_details,occurred_at) VALUES($1,$2,'QUIET_PERIOD_ENTERED','CLOSURE_REQUESTED','QUIESCING','{}'::jsonb,$3)`, uuid.Must(uuid.NewV7()), item.ID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) SaveAccountClosureBlockers(ctx context.Context, item AccountClosureRequest, blockers []AccountClosureBlocker) error {
	now := r.now()
	encoded, err := json.Marshal(blockers)
	if err != nil {
		return err
	}
	status := "BLOCKED"
	if len(blockers) == 0 {
		status = "IN_PROGRESS"
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE account_closure_requests SET status=$2,stage='FINANCIAL_CLEARANCE',blocker_snapshot=$3,next_attempt_at=$4,lease_until=NULL,last_error_code=NULL,version=version+1,updated_at=$5 WHERE id=$1 AND version=$6 AND stage IN ('QUIESCING','FINANCIAL_CLEARANCE')`, item.ID, status, encoded, now.Add(time.Minute), now, item.Version)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_audit(id,closure_request_id,event_type,from_stage,to_stage,redacted_details,occurred_at) VALUES($1,$2,'BLOCKERS_EVALUATED',$3,'FINANCIAL_CLEARANCE',jsonb_build_object('blocker_codes',(SELECT COALESCE(jsonb_agg(value->>'code'),'[]'::jsonb) FROM jsonb_array_elements($4::jsonb) value),'ready',$5),$6)`, uuid.Must(uuid.NewV7()), item.ID, item.Stage, encoded, len(blockers) == 0, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdvanceAccountClosureToIdentityCleanup(ctx context.Context, item AccountClosureRequest) error {
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE account_closure_requests SET status='IN_PROGRESS',stage='IDENTITY_CLEANUP',blocker_snapshot='[]'::jsonb,next_attempt_at=$2,lease_until=NULL,last_error_code=NULL,version=version+1,updated_at=$2 WHERE id=$1 AND version=$3 AND stage IN ('QUIESCING','FINANCIAL_CLEARANCE')`, item.ID, now, item.Version)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_audit(id,closure_request_id,event_type,from_stage,to_stage,redacted_details,occurred_at) VALUES($1,$2,'FINANCIAL_CLEARANCE_PASSED',$3,'IDENTITY_CLEANUP','{"all_blockers_cleared":true}'::jsonb,$4)`, uuid.Must(uuid.NewV7()), item.ID, item.Stage, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) ScheduleAccountClosureError(ctx context.Context, item AccountClosureRequest, code string) error {
	now := r.now()
	status := "RETRY"
	next := any(now.Add(time.Duration(min(item.Attempts, 10)) * time.Minute))
	if !item.SLAExpiresAt.After(now) {
		status, next = "REPAIR_REQUIRED", nil
	}
	_, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET status=$2,last_error_code=$3,next_attempt_at=$4,lease_until=NULL,version=version+1,updated_at=$5 WHERE id=$1 AND version=$6`, item.ID, status, strings.TrimSpace(code), next, now, item.Version)
	return err
}

func (r *PostgresRepository) ListAccountClosureRepairs(ctx context.Context, filter AccountClosureRepairFilter) ([]AccountClosureRequest, error) {
	filter.Status, filter.Stage = strings.ToUpper(strings.TrimSpace(filter.Status)), strings.ToUpper(strings.TrimSpace(filter.Stage))
	rows, err := r.pool.Query(ctx, accountClosureSelect+` WHERE ($1='' OR status=$1) AND ($2='' OR stage=$2) AND status<>'COMPLETED' ORDER BY updated_at DESC,id LIMIT 100`, filter.Status, filter.Stage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AccountClosureRequest, 0)
	for rows.Next() {
		item, err := scanAccountClosure(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) RetryAccountClosure(ctx context.Context, principal identity.Principal, closureID uuid.UUID, key, reason string) (AccountClosureRequest, error) {
	key, reason = strings.TrimSpace(key), strings.TrimSpace(reason)
	if closureID == uuid.Nil || len(key) < 16 || len(key) > 128 || reason == "" || len(reason) > 500 {
		return AccountClosureRequest{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AccountClosureRequest{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil || actorID == nil {
		if err != nil {
			return AccountClosureRequest{}, err
		}
		return AccountClosureRequest{}, ErrForbidden
	}
	keyDigest := closureDigest(key)
	fingerprint := closureDigest(closureID.String() + "\n" + reason)
	var existingFingerprint string
	err = tx.QueryRow(ctx, `SELECT request_fingerprint FROM account_closure_commands WHERE actor_user_id=$1 AND idempotency_key_digest=$2`, *actorID, keyDigest).Scan(&existingFingerprint)
	if err == nil {
		if existingFingerprint != fingerprint {
			return AccountClosureRequest{}, ErrConflict
		}
		item, loadErr := scanAccountClosure(tx.QueryRow(ctx, accountClosureSelect+` WHERE id=$1`, closureID))
		item.Replayed = loadErr == nil
		return item, loadErr
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AccountClosureRequest{}, err
	}
	item, err := scanAccountClosure(tx.QueryRow(ctx, accountClosureSelect+` WHERE id=$1 FOR UPDATE`, closureID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountClosureRequest{}, ErrNotFound
	}
	if err != nil {
		return AccountClosureRequest{}, err
	}
	if item.Stage != "CLOSURE_REQUESTED" && item.Stage != "QUIESCING" && item.Stage != "FINANCIAL_CLEARANCE" && item.Stage != "IDENTITY_CLEANUP" && item.Stage != "ANONYMIZED" || item.Status == "COMPLETED" {
		return AccountClosureRequest{}, ErrConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_commands(id,closure_request_id,actor_user_id,idempotency_key_digest,request_fingerprint,command_type,reason,created_at) VALUES($1,$2,$3,$4,$5,'RETRY_STAGE',$6,$7)`, uuid.Must(uuid.NewV7()), closureID, *actorID, keyDigest, fingerprint, reason, now); err != nil {
		return AccountClosureRequest{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE account_closure_requests SET status='RETRY',last_error_code=NULL,next_attempt_at=$2,lease_until=NULL,version=version+1,updated_at=$2 WHERE id=$1`, closureID, now); err != nil {
		return AccountClosureRequest{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_audit(id,closure_request_id,event_type,actor_user_id,from_stage,to_stage,redacted_details,occurred_at) VALUES($1,$2,'MANUAL_RETRY_REQUESTED',$3,$4,$4,jsonb_build_object('reason',$5),$6)`, uuid.Must(uuid.NewV7()), closureID, *actorID, item.Stage, reason, now); err != nil {
		return AccountClosureRequest{}, err
	}
	item.Status, item.LastErrorCode, item.NextAttemptAt, item.UpdatedAt, item.Version = "RETRY", "", &now, now, item.Version+1
	return item, tx.Commit(ctx)
}

type AccountClosureWorker struct {
	repository *PostgresRepository
	clients    []AccountClosureClient
	identities IdentityProvisioner
	grants     AccountClosureGrantClient
	interval   time.Duration
}

func NewAccountClosureWorker(repository *PostgresRepository, interval time.Duration, clients ...AccountClosureClient) *AccountClosureWorker {
	return &AccountClosureWorker{repository: repository, clients: append([]AccountClosureClient(nil), clients...), interval: interval}
}

func (w *AccountClosureWorker) WithCleanup(identities IdentityProvisioner, grants AccountClosureGrantClient) *AccountClosureWorker {
	w.identities, w.grants = identities, grants
	return w
}

func (w *AccountClosureWorker) Run(ctx context.Context) {
	if w.interval <= 0 {
		w.interval = 5 * time.Second
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		w.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *AccountClosureWorker) runOnce(ctx context.Context) {
	_, _ = w.repository.PurgeExpiredAccountClosureRetention(ctx, 100)
	for processed := 0; processed < 20; processed++ {
		item, err := w.repository.ClaimAccountClosure(ctx)
		if err != nil || item == nil {
			return
		}
		blockers, gateErr := accountClosureRoleBlockers(w.repository.CheckAccountClosureRoleGate(ctx, item.UserID))
		if gateErr != nil {
			_ = w.repository.ScheduleAccountClosureError(ctx, *item, "ACCOUNT_GATE_UNAVAILABLE")
			continue
		}
		if item.Stage == "CLOSURE_REQUESTED" {
			if len(blockers) != 0 {
				_ = w.repository.SaveAccountClosureRequestedBlockers(ctx, *item, blockers)
				continue
			}
			if err := w.repository.EnterAccountClosureQuiescing(ctx, *item); err != nil {
				_ = w.repository.ScheduleAccountClosureError(ctx, *item, "QUIET_PERIOD_ENTRY_FAILED")
			}
			continue
		}
		if item.Stage == "IDENTITY_CLEANUP" {
			if w.identities == nil || w.grants == nil {
				_ = w.repository.ScheduleAccountClosureCleanupError(ctx, item.ID, "CLEANUP_DEPENDENCY_UNAVAILABLE")
				continue
			}
			preparation, err := w.repository.PrepareAccountClosureCleanup(ctx, item.ID, item.UserID)
			if err != nil {
				_ = w.repository.ScheduleAccountClosureCleanupError(ctx, item.ID, "ACCOUNT_CLEANUP_PREPARATION_FAILED")
				continue
			}
			identityFailed := false
			for _, subject := range preparation.IdentitySubjects {
				if err := w.identities.DeleteUser(ctx, subject); err != nil {
					_ = w.repository.ScheduleAccountClosureCleanupError(ctx, item.ID, "IDENTITY_DELETE_UNAVAILABLE")
					identityFailed = true
					break
				}
			}
			if identityFailed {
				continue
			}
			if err := w.repository.MarkAccountClosureIdentityDeleted(ctx, item.ID, preparation.SubjectLifecycleVersion); err != nil {
				_ = w.repository.ScheduleAccountClosureCleanupError(ctx, item.ID, "IDENTITY_DELETE_COMMIT_FAILED")
				continue
			}
			if err := w.grants.IsolateGrants(ctx, item.UserID, item.ID, preparation.SubjectLifecycleVersion); err != nil {
				_ = w.repository.ScheduleAccountClosureCleanupError(ctx, item.ID, "GRANT_ISOLATION_FAILED")
				continue
			}
			if err := w.repository.MarkAccountClosureGrantsIsolated(ctx, item.ID, preparation.SubjectLifecycleVersion); err != nil {
				_ = w.repository.ScheduleAccountClosureCleanupError(ctx, item.ID, "GRANT_ISOLATION_COMMIT_FAILED")
				continue
			}
			if err := w.repository.AnonymizeAccountClosure(ctx, item.ID, item.UserID, preparation.SubjectLifecycleVersion); err != nil {
				_ = w.repository.ScheduleAccountClosureCleanupError(ctx, item.ID, "ANONYMIZATION_FAILED")
			}
			continue
		}
		if item.Stage == "ANONYMIZED" {
			if err := w.repository.CompleteAccountClosure(ctx, item.ID); err != nil {
				_ = w.repository.ScheduleAccountClosureCleanupError(ctx, item.ID, "OUTBOX_CONFIRMATION_PENDING")
			}
			continue
		}
		failed := false
		for _, client := range w.clients {
			assessment, err := client.Assess(ctx, item.UserID)
			if err != nil {
				_ = w.repository.ScheduleAccountClosureError(ctx, *item, "DOMAIN_ASSESSMENT_UNAVAILABLE")
				failed = true
				break
			}
			blockers = append(blockers, assessment.Blockers...)
		}
		if failed {
			continue
		}
		if len(blockers) != 0 {
			_ = w.repository.SaveAccountClosureBlockers(ctx, *item, blockers)
			continue
		}
		_ = w.repository.AdvanceAccountClosureToIdentityCleanup(ctx, *item)
	}
}

func userVisibleClosure(item AccountClosureRequest) AccountClosurePublic {
	for index := range item.Blockers {
		item.Blockers[index].ResourceID = ""
	}
	return AccountClosurePublic{ID: item.ID, Status: item.Status, Stage: item.Stage, PolicyVersion: item.PolicyVersion, Blockers: item.Blockers, Attempts: item.Attempts, LastErrorCode: item.LastErrorCode, NextAttemptAt: item.NextAttemptAt, ReauthenticatedAt: item.ReauthenticatedAt, SLAExpiresAt: item.SLAExpiresAt, Version: item.Version, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, CompletedAt: item.CompletedAt, Replayed: item.Replayed}
}
