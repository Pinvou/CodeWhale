package token

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrCredentialWindowBlocked = errors.New("credential window blocked")
	ErrCredentialLaunchExpired = errors.New("credential launch expired")
	ErrCredentialReplay        = errors.New("credential launch replayed")
	ErrCredentialProbeClaimed  = errors.New("credential validation probe already claimed")
)

type CredentialLaunchRequest struct {
	Action                    string `json:"action"`
	ExpectedAccountVersion    int64  `json:"expected_account_version"`
	ExpectedCredentialVersion int    `json:"expected_credential_version"`
	Slot                      string `json:"slot"`
	Reason                    string `json:"reason"`
}

// CredentialIsolationAuthoritySnapshot freezes the exact Account
// authorization and Token governance generation selected for a launch. It is
// intentionally never serialized to the browser.
type CredentialIsolationAuthoritySnapshot struct {
	ActorID              uuid.UUID
	Issuer               string
	Subject              string
	AuthorizationVersion int64
	GateEpoch            int64
	RequiredPermission   string // In-memory only; launch/session persistence defaults to tokens.manage.
}

// CredentialIsolationAuthorityGuard is the APT-138 integration seam. The
// implementation must compare the frozen Account authorization and governance
// epoch while the caller holds the launch/session row locks. No credential
// state transition is allowed when the guard is absent or the facts changed.
type CredentialIsolationAuthorityGuard interface {
	LockAndCheckCredentialIsolationAuthority(context.Context, pgx.Tx, CredentialIsolationAuthoritySnapshot) error
}

type credentialIsolationAuthorityContextKey struct{}

func withCredentialIsolationAuthority(ctx context.Context, snapshot CredentialIsolationAuthoritySnapshot) context.Context {
	return context.WithValue(ctx, credentialIsolationAuthorityContextKey{}, snapshot)
}

func (r *PostgresRepository) WithCredentialIsolationAuthorityGuard(guard CredentialIsolationAuthorityGuard) *PostgresRepository {
	r.credentialAuthGuard = guard
	return r
}

func (r *PostgresRepository) recheckCredentialIsolationAuthority(ctx context.Context, tx pgx.Tx, snapshot CredentialIsolationAuthoritySnapshot) error {
	if snapshot.ActorID == uuid.Nil || strings.TrimSpace(snapshot.Issuer) == "" || strings.TrimSpace(snapshot.Subject) == "" || snapshot.AuthorizationVersion < 1 || snapshot.GateEpoch < 1 || r.credentialAuthGuard == nil {
		return ErrTokenRouteActivationChanged
	}
	return r.credentialAuthGuard.LockAndCheckCredentialIsolationAuthority(ctx, tx, snapshot)
}

func credentialSessionAuthority(item credentialIsolationSession) CredentialIsolationAuthoritySnapshot {
	return CredentialIsolationAuthoritySnapshot{ActorID: item.ActorID, Issuer: item.ActorIssuer, Subject: item.ActorSubject, AuthorizationVersion: item.AuthorizationVersion, GateEpoch: item.GateEpoch}
}

func (r *PostgresRepository) commitCredentialIsolationMutation(ctx context.Context, tx pgx.Tx, snapshot CredentialIsolationAuthoritySnapshot) error {
	if err := r.recheckCredentialIsolationAuthority(ctx, tx, snapshot); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) recheckCredentialValidationAuthority(ctx context.Context, tx pgx.Tx, item TokenCommandResult) error {
	if r.routeActivationGuard == nil {
		return nil
	} // Uninstalled repository fixtures; production always installs both guards.
	principal, ok := identity.FromContext(ctx)
	selected, selectionOK := ctx.Value(tokenRouteActivationContextKey{}).(tokenRouteActivationSelection)
	if !ok || !selectionOK || selected.State != "ACTIVE" || !selected.transactional {
		return ErrTokenRouteActivationChanged
	}
	return r.recheckCredentialIsolationAuthority(ctx, tx, CredentialIsolationAuthoritySnapshot{ActorID: item.ActorUserID, Issuer: principal.Issuer, Subject: principal.Subject, AuthorizationVersion: item.AuthorizationVersion, GateEpoch: selected.Epoch, RequiredPermission: "platform.tokens.credentials.validate"})
}

type CredentialLaunch struct {
	LaunchID       uuid.UUID `json:"launch_id"`
	ResultRef      uuid.UUID `json:"result_ref"`
	TokenAccountID uuid.UUID `json:"token_account_id"`
	Action         string    `json:"action"`
	Status         string    `json:"status"`
	ExpiresAt      time.Time `json:"expires_at"`
	AuditRequestID string    `json:"audit_request_id"`
}

type CredentialIsolationResult struct {
	ResultRef         uuid.UUID  `json:"result_ref"`
	TokenAccountID    uuid.UUID  `json:"token_account_id"`
	Action            string     `json:"action"`
	Status            string     `json:"status"`
	CredentialID      *uuid.UUID `json:"credential_id,omitempty"`
	CredentialVersion *int       `json:"credential_version,omitempty"`
	CredentialStatus  *string    `json:"credential_status,omitempty"`
	ValidationStatus  *string    `json:"validation_status,omitempty"`
	Fingerprint       *string    `json:"fingerprint,omitempty"`
	StableErrorCode   *string    `json:"error_code,omitempty"`
	AuditRequestID    string     `json:"audit_request_id"`
	WindowConfirmed   bool       `json:"window_confirmed"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type credentialIsolationSession struct {
	ID                        uuid.UUID
	LaunchID                  uuid.UUID
	ResultRef                 uuid.UUID
	ActorID                   uuid.UUID
	ActorIssuer               string
	ActorSubject              string
	AuthorizationVersion      int64
	GateEpoch                 int64
	TokenAccountID            uuid.UUID
	Action                    string
	ExpectedAccountVersion    int64
	ExpectedCredentialVersion int
	Slot                      string
	CredentialOrigin          string
	BrowserDigest             string
	StateDigest               string
	NonceDigest               string
	Verifier                  string
	CSRFDigest                string
	Status                    string
	Attempts                  int
	HeartbeatAt               *time.Time
	ExpiresAt                 time.Time
}

func (s *Service) executeGovernedCredentialValidation(ctx context.Context, repository *PostgresRepository, reference, actorID uuid.UUID, envelope TokenCommandEnvelope) (TokenCommandResult, error) {
	var command struct {
		TokenAccountID uuid.UUID `json:"token_account_id"`
		CredentialID   uuid.UUID `json:"credential_id"`
	}
	var expected struct {
		ExpectedAccountVersion int64 `json:"expected_account_version"`
		CredentialVersion      int   `json:"credential_version"`
	}
	if decodeCommand(envelope.Command, &command) != nil || json.Unmarshal(envelope.ExpectedVersions, &expected) != nil || command.TokenAccountID == uuid.Nil || command.CredentialID == uuid.Nil || expected.ExpectedAccountVersion < 1 || expected.CredentialVersion < 1 {
		return TokenCommandResult{}, ErrTokenCommandInvalid
	}
	claimed, credential, secret, err := repository.claimCredentialValidationProbe(ctx, reference, command.TokenAccountID, command.CredentialID, actorID, expected.ExpectedAccountVersion, expected.CredentialVersion)
	if err != nil {
		return TokenCommandResult{}, err
	}
	if !claimed {
		if credential.CommandResult.Status != "PENDING" {
			return credential.CommandResult, nil
		}
		return credential.CommandResult, ErrCredentialProbeClaimed
	}
	valid := s.validator != nil && s.validator.Validate(ctx, credential.BaseURL, secret, credential.AllowedModels) == nil
	clear(secret)
	stableCode := ""
	if !valid {
		stableCode = "CREDENTIAL_CHECK_FAILED"
	}
	return repository.recordGovernedCredentialValidation(ctx, reference, command.TokenAccountID, command.CredentialID, actorID, expected.ExpectedAccountVersion, expected.CredentialVersion, valid, stableCode)
}

type claimedCredentialValidation struct {
	Credential
	CommandResult TokenCommandResult
}

func (r *PostgresRepository) claimCredentialValidationProbe(ctx context.Context, reference, accountID, credentialID, actorID uuid.UUID, expectedAccountVersion int64, expectedCredentialVersion int) (bool, claimedCredentialValidation, []byte, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, claimedCredentialValidation{}, nil, err
	}
	defer tx.Rollback(ctx)
	item, err := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE command_reference=$1 FOR UPDATE`, reference))
	if err != nil {
		return false, claimedCredentialValidation{}, nil, err
	}
	if item.ActorUserID != actorID || item.CommandType != "TOKEN_CREDENTIAL_VALIDATE" || item.TargetResourceType != "PROVIDER_CREDENTIAL" || item.TargetResourceID != credentialID.String() {
		return false, claimedCredentialValidation{}, nil, ErrForbidden
	}
	if item.Status != "PENDING" {
		return false, claimedCredentialValidation{CommandResult: item}, nil, tx.Commit(ctx)
	}
	var claimedAt, leaseUntil *time.Time
	if err = tx.QueryRow(ctx, `SELECT credential_probe_claimed_at,credential_probe_lease_until FROM token_command_results WHERE command_reference=$1`, reference).Scan(&claimedAt, &leaseUntil); err != nil {
		return false, claimedCredentialValidation{}, nil, err
	}
	if claimedAt != nil {
		// A provider probe cannot be safely repeated after a process crash: the
		// remote side may have accepted it even when no response was persisted.
		// While the lease is live callers observe the original PENDING command;
		// once it expires the same reference is terminalized without another
		// provider call. This is the recoverable, fail-closed outcome.
		now := r.now().UTC()
		if leaseUntil == nil || leaseUntil.After(now) {
			return false, claimedCredentialValidation{CommandResult: item}, nil, tx.Commit(ctx)
		}
		if err = r.recheckCredentialValidationAuthority(ctx, tx, item); err != nil {
			return false, claimedCredentialValidation{}, nil, err
		}
		after, _ := json.Marshal(map[string]any{"token_account_id": accountID, "credential_id": credentialID, "credential_version": expectedCredentialVersion, "validation_status": "UNKNOWN"})
		redacted, _ := json.Marshal(map[string]any{"validation_status": "UNKNOWN", "error_code": "CREDENTIAL_PROBE_OUTCOME_UNKNOWN"})
		completed, completeErr := completeTokenCommandTx(ctx, tx, reference, "FAILED", after, redacted, "CREDENTIAL_PROBE_OUTCOME_UNKNOWN")
		if completeErr != nil {
			return false, claimedCredentialValidation{}, nil, completeErr
		}
		if err = r.recheckCredentialValidationAuthority(ctx, tx, item); err != nil {
			return false, claimedCredentialValidation{}, nil, err
		}
		return false, claimedCredentialValidation{CommandResult: completed}, nil, r.commitGovernedMutation(ctx, tx)
	}
	if err = r.recheckCredentialValidationAuthority(ctx, tx, item); err != nil {
		return false, claimedCredentialValidation{}, nil, err
	}
	var credential claimedCredentialValidation
	var ciphertext []byte
	var keyID string
	var accountVersion int64
	if err = tx.QueryRow(ctx, `SELECT account.management_version,credential.id,credential.token_account_id,credential.provider,credential.base_url,credential.ciphertext,credential.key_id,credential.fingerprint,credential.version,credential.status,credential.allowed_models FROM token_accounts account JOIN provider_credentials credential ON credential.token_account_id=account.id WHERE account.id=$1 AND account.management_mode='PLATFORM' AND credential.id=$2 AND credential.status='ACTIVE' FOR UPDATE OF account,credential`, accountID, credentialID).Scan(&accountVersion, &credential.ID, &credential.TokenAccountID, &credential.Provider, &credential.BaseURL, &ciphertext, &keyID, &credential.Fingerprint, &credential.Version, &credential.Status, &credential.AllowedModels); err != nil {
		return false, claimedCredentialValidation{}, nil, err
	}
	if accountVersion != expectedAccountVersion || credential.Version != expectedCredentialVersion {
		return false, claimedCredentialValidation{}, nil, ErrCatalogVersionConflict
	}
	secret, err := r.secrets.DecryptWithKeyID(keyID, ciphertext, []byte(accountID.String()+":"+credentialID.String()))
	if err != nil {
		return false, claimedCredentialValidation{}, nil, err
	}
	now := r.now().UTC()
	command, err := tx.Exec(ctx, `UPDATE token_command_results SET credential_probe_claimed_at=$2::timestamptz,credential_probe_lease_until=$2::timestamptz+interval '30 seconds' WHERE command_reference=$1 AND status='PENDING' AND credential_probe_claimed_at IS NULL`, reference, now)
	if err != nil || command.RowsAffected() != 1 {
		clear(secret)
		if err != nil {
			return false, claimedCredentialValidation{}, nil, err
		}
		return false, claimedCredentialValidation{}, nil, ErrCredentialProbeClaimed
	}
	credential.CommandResult = item
	if err = r.recheckCredentialValidationAuthority(ctx, tx, item); err != nil {
		clear(secret)
		return false, claimedCredentialValidation{}, nil, err
	}
	if err = r.commitGovernedMutation(ctx, tx); err != nil {
		clear(secret)
		return false, claimedCredentialValidation{}, nil, err
	}
	return true, credential, secret, nil
}

func (r *PostgresRepository) recordGovernedCredentialValidation(ctx context.Context, reference, accountID, credentialID, actorID uuid.UUID, expectedAccountVersion int64, expectedCredentialVersion int, valid bool, errorCode string) (TokenCommandResult, error) {
	status := "INVALID"
	if valid {
		status = "VALID"
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenCommandResult{}, err
	}
	defer tx.Rollback(ctx)
	claimed, err := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE command_reference=$1 FOR UPDATE`, reference))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenCommandResult{}, ErrNotFound
	}
	if err != nil {
		return TokenCommandResult{}, err
	}
	if claimed.ActorUserID != actorID || claimed.CommandType != "TOKEN_CREDENTIAL_VALIDATE" || claimed.TargetResourceType != "PROVIDER_CREDENTIAL" || claimed.TargetResourceID != credentialID.String() {
		return TokenCommandResult{}, ErrForbidden
	}
	if claimed.Status != "PENDING" {
		return claimed, tx.Commit(ctx)
	}
	if err = r.recheckCredentialValidationAuthority(ctx, tx, claimed); err != nil {
		return TokenCommandResult{}, err
	}
	var probeClaimedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT credential_probe_claimed_at FROM token_command_results WHERE command_reference=$1`, reference).Scan(&probeClaimedAt); err != nil || probeClaimedAt == nil {
		if err != nil {
			return TokenCommandResult{}, err
		}
		return TokenCommandResult{}, ErrCredentialProbeClaimed
	}
	var accountVersion int64
	var credentialVersion int
	if err = tx.QueryRow(ctx, `SELECT account.management_version,credential.version FROM token_accounts account JOIN provider_credentials credential ON credential.token_account_id=account.id WHERE account.id=$1 AND account.management_mode='PLATFORM' AND credential.id=$2 AND credential.status='ACTIVE' FOR UPDATE OF account,credential`, accountID, credentialID).Scan(&accountVersion, &credentialVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenCommandResult{}, ErrNotFound
		}
		return TokenCommandResult{}, err
	}
	if accountVersion != expectedAccountVersion || credentialVersion != expectedCredentialVersion {
		return TokenCommandResult{}, ErrCatalogVersionConflict
	}
	now := r.now().UTC()
	if _, err = tx.Exec(ctx, `UPDATE provider_credentials SET validation_status=$3,validated_at=$4,validation_checked_by=$5 WHERE token_account_id=$1 AND id=$2`, accountID, credentialID, status, now, actorID); err != nil {
		return TokenCommandResult{}, err
	}
	if err = recordTokenAudit(ctx, tx, actorID, "TOKEN_CREDENTIAL_VALIDATED", "PROVIDER_CREDENTIAL", credentialID, `jsonb_build_object('token_account_id',$6,'validation_status',$7,'stable_error_code',NULLIF($8,''),'credential_version',$9)`, accountID, status, errorCode, credentialVersion); err != nil {
		return TokenCommandResult{}, err
	}
	var item AdminCredentialSafeMetadata
	if err = tx.QueryRow(ctx, `SELECT id,provider,fingerprint,version,status,validation_status,allowed_models,validated_at,created_by,created_at,COALESCE(revoked_at,retired_at,activated_at,validated_at,created_at) FROM provider_credentials WHERE token_account_id=$1 AND id=$2`, accountID, credentialID).Scan(&item.ID, &item.CredentialType, &item.Fingerprint, &item.Version, &item.Status, &item.ValidationStatus, &item.AllowedModels, &item.ValidatedAt, &item.ResponsibleBy, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return TokenCommandResult{}, err
	}
	redacted, _ := json.Marshal(AdminCredentialSafeValidationResult{ValidationStatus: item.ValidationStatus, ErrorCode: errorCode, Credential: item})
	after, _ := json.Marshal(map[string]any{"token_account_id": accountID, "expected_account_version": expectedAccountVersion, "credential_id": credentialID, "credential_version": expectedCredentialVersion, "validation_status": item.ValidationStatus})
	completed, err := completeTokenCommandTx(ctx, tx, reference, "SUCCEEDED", after, redacted, "")
	if err != nil {
		return TokenCommandResult{}, err
	}
	if err = r.recheckCredentialValidationAuthority(ctx, tx, claimed); err != nil {
		return TokenCommandResult{}, err
	}
	return completed, r.commitGovernedMutation(ctx, tx)
}

func credentialDigest(values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(digest[:])
}

func randomCredentialToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func canonicalCredentialLaunch(accountID, actorID uuid.UUID, input CredentialLaunchRequest) (CredentialLaunchRequest, string, error) {
	input.Action = strings.ToUpper(strings.TrimSpace(input.Action))
	input.Slot = strings.ToUpper(strings.TrimSpace(input.Slot))
	input.Reason = strings.TrimSpace(input.Reason)
	if accountID == uuid.Nil || actorID == uuid.Nil || !slices.Contains([]string{"ENROLL", "ROTATE"}, input.Action) || input.ExpectedAccountVersion < 1 || input.ExpectedCredentialVersion < 0 || input.Slot == "" || len(input.Slot) > 64 || utf8.RuneCountInString(input.Reason) < 10 || utf8.RuneCountInString(input.Reason) > 2000 {
		return CredentialLaunchRequest{}, "", ErrTokenCommandInvalid
	}
	if input.Action == "ENROLL" && input.ExpectedCredentialVersion != 0 || input.Action == "ROTATE" && input.ExpectedCredentialVersion < 1 {
		return CredentialLaunchRequest{}, "", ErrTokenCommandInvalid
	}
	payload, _ := json.Marshal(struct {
		AccountID uuid.UUID `json:"token_account_id"`
		CredentialLaunchRequest
	}{accountID, input})
	return input, credentialDigest(string(payload)), nil
}

func (r *PostgresRepository) CreateCredentialLaunch(ctx context.Context, accountID, actorID uuid.UUID, input CredentialLaunchRequest, idempotencyKey, auditRequestID string) (CredentialLaunch, error) {
	input, requestDigest, err := canonicalCredentialLaunch(accountID, actorID, input)
	if err != nil {
		return CredentialLaunch{}, err
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 128 || strings.TrimSpace(auditRequestID) == "" {
		return CredentialLaunch{}, ErrTokenCommandInvalid
	}
	keyDigest := credentialDigest(idempotencyKey)
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CredentialLaunch{}, err
	}
	defer tx.Rollback(ctx)
	authority, ok := ctx.Value(credentialIsolationAuthorityContextKey{}).(CredentialIsolationAuthoritySnapshot)
	if !ok || authority.ActorID != actorID || r.recheckCredentialIsolationAuthority(ctx, tx, authority) != nil {
		return CredentialLaunch{}, ErrTokenRouteActivationChanged
	}
	var existing CredentialLaunch
	var existingDigest string
	var existingAuthority CredentialIsolationAuthoritySnapshot
	err = tx.QueryRow(ctx, `SELECT launch.id,launch.result_ref,launch.token_account_id,launch.action,launch.status,launch.expires_at,result.audit_request_id,launch.actor_user_id,launch.actor_issuer,launch.actor_subject,launch.authorization_version,launch.gate_epoch FROM credential_launches launch JOIN credential_isolation_results result ON result.launch_id=launch.id WHERE launch.actor_user_id=$1 AND launch.action=$2 AND launch.idempotency_key_digest=$3 FOR UPDATE OF launch,result`, actorID, input.Action, keyDigest).Scan(&existing.LaunchID, &existing.ResultRef, &existing.TokenAccountID, &existing.Action, &existing.Status, &existing.ExpiresAt, &existing.AuditRequestID, &existingAuthority.ActorID, &existingAuthority.Issuer, &existingAuthority.Subject, &existingAuthority.AuthorizationVersion, &existingAuthority.GateEpoch)
	if err == nil {
		if err = tx.QueryRow(ctx, `SELECT request_digest FROM credential_launches WHERE id=$1`, existing.LaunchID).Scan(&existingDigest); err != nil {
			return CredentialLaunch{}, err
		}
		if existingDigest != requestDigest || existingAuthority != authority {
			return CredentialLaunch{}, ErrIdempotencyConflict
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CredentialLaunch{}, err
	}
	var accountVersion int64
	if err = tx.QueryRow(ctx, `SELECT management_version FROM token_accounts WHERE id=$1 AND management_mode='PLATFORM' AND status='ACTIVE' FOR UPDATE`, accountID).Scan(&accountVersion); errors.Is(err, pgx.ErrNoRows) {
		return CredentialLaunch{}, ErrNotFound
	} else if err != nil {
		return CredentialLaunch{}, err
	}
	if accountVersion != input.ExpectedAccountVersion {
		return CredentialLaunch{}, ErrCatalogVersionConflict
	}
	var activeVersion int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(version) FILTER (WHERE status='ACTIVE'),0) FROM provider_credentials WHERE token_account_id=$1`, accountID).Scan(&activeVersion); err != nil {
		return CredentialLaunch{}, err
	}
	if activeVersion != input.ExpectedCredentialVersion {
		return CredentialLaunch{}, ErrCatalogVersionConflict
	}
	now := r.now().UTC()
	launchID, resultRef := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	expiresAt := now.Add(5 * time.Minute)
	if _, err = tx.Exec(ctx, `INSERT INTO credential_launches(id,result_ref,actor_user_id,actor_issuer,actor_subject,authorization_version,gate_epoch,token_account_id,action,expected_account_version,expected_credential_version,slot,request_digest,idempotency_key_digest,status,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'CREATED',$15,$16)`, launchID, resultRef, actorID, authority.Issuer, authority.Subject, authority.AuthorizationVersion, authority.GateEpoch, accountID, input.Action, input.ExpectedAccountVersion, input.ExpectedCredentialVersion, input.Slot, requestDigest, keyDigest, now, expiresAt); err != nil {
		return CredentialLaunch{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO credential_isolation_results(result_ref,launch_id,actor_user_id,token_account_id,action,status,audit_request_id,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'PENDING',$6,$7,$7)`, resultRef, launchID, actorID, accountID, input.Action, auditRequestID, now); err != nil {
		return CredentialLaunch{}, err
	}
	if err = recordTokenAudit(ctx, tx, actorID, "TOKEN_CREDENTIAL_"+input.Action+"_LAUNCHED", "TOKEN_ACCOUNT", accountID, `jsonb_build_object('action',$6,'slot',$7,'result_ref',$8,'request_id',$9)`, input.Action, input.Slot, resultRef, auditRequestID); err != nil {
		return CredentialLaunch{}, err
	}
	if err = r.commitCredentialIsolationMutation(ctx, tx, authority); err != nil {
		return CredentialLaunch{}, err
	}
	return CredentialLaunch{LaunchID: launchID, ResultRef: resultRef, TokenAccountID: accountID, Action: input.Action, Status: "CREATED", ExpiresAt: expiresAt, AuditRequestID: auditRequestID}, nil
}

func (r *PostgresRepository) CredentialIsolationResult(ctx context.Context, actorID, resultRef uuid.UUID) (CredentialIsolationResult, error) {
	var item CredentialIsolationResult
	err := r.pool.QueryRow(ctx, `SELECT result.result_ref,result.token_account_id,result.action,result.status,result.credential_id,result.credential_version,result.credential_status,result.validation_status,result.fingerprint,result.stable_error_code,result.audit_request_id,EXISTS(SELECT 1 FROM credential_isolation_sessions session WHERE session.launch_id=result.launch_id AND session.heartbeat_at IS NOT NULL),result.updated_at FROM credential_isolation_results result WHERE result.result_ref=$1 AND result.actor_user_id=$2`, resultRef, actorID).Scan(&item.ResultRef, &item.TokenAccountID, &item.Action, &item.Status, &item.CredentialID, &item.CredentialVersion, &item.CredentialStatus, &item.ValidationStatus, &item.Fingerprint, &item.StableErrorCode, &item.AuditRequestID, &item.WindowConfirmed, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialIsolationResult{}, ErrNotFound
	}
	return item, err
}

func (r *PostgresRepository) ConsumeCredentialLaunch(ctx context.Context, launchID uuid.UUID, credentialOrigin, browserBinding, state, nonce, verifier, csrf string) (credentialIsolationSession, error) {
	credentialOrigin = strings.TrimRight(strings.TrimSpace(credentialOrigin), "/")
	parsedOrigin, originErr := url.Parse(credentialOrigin)
	if launchID == uuid.Nil || originErr != nil || parsedOrigin.Host == "" || parsedOrigin.User != nil || parsedOrigin.Path != "" || parsedOrigin.RawQuery != "" || parsedOrigin.Fragment != "" || len(browserBinding) < 32 || len(state) < 32 || len(nonce) < 32 || len(verifier) < 43 || len(csrf) < 32 {
		return credentialIsolationSession{}, ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return credentialIsolationSession{}, err
	}
	defer tx.Rollback(ctx)
	var item credentialIsolationSession
	var expiresAt time.Time
	var status string
	err = tx.QueryRow(ctx, `SELECT result_ref,actor_user_id,actor_issuer,actor_subject,authorization_version,gate_epoch,token_account_id,action,expected_account_version,expected_credential_version,slot,status,expires_at FROM credential_launches WHERE id=$1 FOR UPDATE`, launchID).Scan(&item.ResultRef, &item.ActorID, &item.ActorIssuer, &item.ActorSubject, &item.AuthorizationVersion, &item.GateEpoch, &item.TokenAccountID, &item.Action, &item.ExpectedAccountVersion, &item.ExpectedCredentialVersion, &item.Slot, &status, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialIsolationSession{}, ErrNotFound
	} else if err != nil {
		return credentialIsolationSession{}, err
	}
	now := r.now().UTC()
	if status != "CREATED" {
		return credentialIsolationSession{}, ErrCredentialReplay
	}
	if !expiresAt.After(now) {
		_, _ = tx.Exec(ctx, `UPDATE credential_launches SET status='EXPIRED',stable_error_code='CREDENTIAL_LAUNCH_EXPIRED',completed_at=$2 WHERE id=$1`, launchID, now)
		_, _ = tx.Exec(ctx, `UPDATE credential_isolation_results SET status='EXPIRED',stable_error_code='CREDENTIAL_LAUNCH_EXPIRED',updated_at=$2 WHERE launch_id=$1`, launchID, now)
		_ = tx.Commit(ctx)
		return credentialIsolationSession{}, ErrCredentialLaunchExpired
	}
	if err = r.recheckCredentialIsolationAuthority(ctx, tx, CredentialIsolationAuthoritySnapshot{ActorID: item.ActorID, Issuer: item.ActorIssuer, Subject: item.ActorSubject, AuthorizationVersion: item.AuthorizationVersion, GateEpoch: item.GateEpoch}); err != nil {
		return credentialIsolationSession{}, err
	}
	item.ID, item.LaunchID, item.CredentialOrigin, item.BrowserDigest, item.StateDigest, item.NonceDigest, item.CSRFDigest, item.Status, item.ExpiresAt = uuid.Must(uuid.NewV7()), launchID, credentialOrigin, credentialDigest(browserBinding), credentialDigest(state), credentialDigest(nonce), credentialDigest(csrf), "AUTHENTICATING", expiresAt
	aad := []byte(item.ID.String() + ":" + launchID.String())
	ciphertext, err := r.secrets.Encrypt([]byte(verifier), aad)
	if err != nil {
		return credentialIsolationSession{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO credential_isolation_sessions(id,launch_id,actor_user_id,authorization_version,gate_epoch,credential_origin,browser_binding_digest,state_digest,nonce_digest,pkce_verifier_ciphertext,key_id,csrf_digest,status,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'AUTHENTICATING',$13,$14)`, item.ID, launchID, item.ActorID, item.AuthorizationVersion, item.GateEpoch, item.CredentialOrigin, item.BrowserDigest, item.StateDigest, item.NonceDigest, ciphertext, r.secrets.KeyID(), item.CSRFDigest, now, expiresAt); err != nil {
		return credentialIsolationSession{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE credential_launches SET status='CONSUMED',browser_binding_digest=$2,consumed_at=$3,heartbeat_deadline=$4 WHERE id=$1 AND status='CREATED'`, launchID, item.BrowserDigest, now, now.Add(10*time.Second)); err != nil {
		return credentialIsolationSession{}, err
	}
	if err = r.commitCredentialIsolationMutation(ctx, tx, credentialSessionAuthority(item)); err != nil {
		return credentialIsolationSession{}, err
	}
	item.Verifier = verifier
	return item, nil
}

func secureCredentialValue(expectedDigest, value string) bool {
	actual := credentialDigest(value)
	return subtle.ConstantTimeCompare([]byte(expectedDigest), []byte(actual)) == 1
}

func (r *PostgresRepository) CredentialIsolationSession(ctx context.Context, sessionID uuid.UUID) (credentialIsolationSession, error) {
	var item credentialIsolationSession
	var ciphertext []byte
	var keyID string
	err := r.pool.QueryRow(ctx, `SELECT session.id,launch.id,launch.result_ref,launch.actor_user_id,launch.actor_issuer,launch.actor_subject,session.authorization_version,session.gate_epoch,launch.token_account_id,launch.action,launch.expected_account_version,launch.expected_credential_version,launch.slot,session.credential_origin,session.browser_binding_digest,session.state_digest,session.nonce_digest,session.pkce_verifier_ciphertext,session.key_id,session.csrf_digest,session.status,launch.attempt_count,session.heartbeat_at,session.expires_at FROM credential_isolation_sessions session JOIN credential_launches launch ON launch.id=session.launch_id WHERE session.id=$1`, sessionID).Scan(&item.ID, &item.LaunchID, &item.ResultRef, &item.ActorID, &item.ActorIssuer, &item.ActorSubject, &item.AuthorizationVersion, &item.GateEpoch, &item.TokenAccountID, &item.Action, &item.ExpectedAccountVersion, &item.ExpectedCredentialVersion, &item.Slot, &item.CredentialOrigin, &item.BrowserDigest, &item.StateDigest, &item.NonceDigest, &ciphertext, &keyID, &item.CSRFDigest, &item.Status, &item.Attempts, &item.HeartbeatAt, &item.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialIsolationSession{}, ErrNotFound
	} else if err != nil {
		return credentialIsolationSession{}, err
	}
	if item.Status != "AUTHENTICATING" && item.Status != "READY" {
		return item, nil
	}
	plaintext, err := r.secrets.DecryptWithKeyID(keyID, ciphertext, []byte(item.ID.String()+":"+item.LaunchID.String()))
	if err != nil {
		return credentialIsolationSession{}, err
	}
	item.Verifier = string(plaintext)
	clear(plaintext)
	return item, nil
}

// CheckCredentialIsolationSessionAuthority revalidates the frozen authority
// before the callback performs any IdP token exchange. Authenticate and final
// credential persistence repeat the check under their own row locks.
func (r *PostgresRepository) CheckCredentialIsolationSessionAuthority(ctx context.Context, sessionID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var snapshot CredentialIsolationAuthoritySnapshot
	var sessionStatus, launchStatus string
	if err = tx.QueryRow(ctx, `SELECT launch.actor_user_id,launch.actor_issuer,launch.actor_subject,session.authorization_version,session.gate_epoch,session.status,launch.status FROM credential_isolation_sessions session JOIN credential_launches launch ON launch.id=session.launch_id WHERE session.id=$1 FOR UPDATE OF session,launch`, sessionID).Scan(&snapshot.ActorID, &snapshot.Issuer, &snapshot.Subject, &snapshot.AuthorizationVersion, &snapshot.GateEpoch, &sessionStatus, &launchStatus); err != nil {
		return ErrForbidden
	}
	if sessionStatus == "AUTHENTICATING" && launchStatus != "CONSUMED" || sessionStatus == "READY" && launchStatus != "AUTHENTICATED" || sessionStatus != "AUTHENTICATING" && sessionStatus != "READY" {
		return ErrForbidden
	}
	if err = r.recheckCredentialIsolationAuthority(ctx, tx, snapshot); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AuthenticateCredentialSession(ctx context.Context, sessionID uuid.UUID, actorID uuid.UUID, authTime time.Time) error {
	now := r.now().UTC()
	if actorID == uuid.Nil || authTime.IsZero() || authTime.After(now) || now.Sub(authTime) > 15*time.Minute {
		return ErrForbidden
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var launchID uuid.UUID
	var heartbeatAt, heartbeatDeadline, expiresAt time.Time
	var authority CredentialIsolationAuthoritySnapshot
	var sessionStatus, launchStatus string
	if err = tx.QueryRow(ctx, `SELECT launch.id,launch.actor_user_id,launch.actor_issuer,launch.actor_subject,session.authorization_version,session.gate_epoch,session.status,launch.status,session.heartbeat_at,launch.heartbeat_deadline,session.expires_at FROM credential_isolation_sessions session JOIN credential_launches launch ON launch.id=session.launch_id WHERE session.id=$1 AND session.actor_user_id=$2 FOR UPDATE OF session,launch`, sessionID, actorID).Scan(&launchID, &authority.ActorID, &authority.Issuer, &authority.Subject, &authority.AuthorizationVersion, &authority.GateEpoch, &sessionStatus, &launchStatus, &heartbeatAt, &heartbeatDeadline, &expiresAt); err != nil {
		return ErrForbidden
	}
	// The ten-second bound applies only to the first browser heartbeat. OIDC
	// authentication may legitimately take longer, up to the five-minute
	// session TTL, so never reinterpret an old but on-time heartbeat as loss
	// of the credential window.
	if sessionStatus != "AUTHENTICATING" || launchStatus != "CONSUMED" || !expiresAt.After(now) || heartbeatAt.After(now) || heartbeatDeadline.Before(heartbeatAt) {
		return ErrForbidden
	}
	if err = r.recheckCredentialIsolationAuthority(ctx, tx, authority); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE credential_isolation_sessions SET status='READY',auth_time=$3 WHERE id=$1 AND actor_user_id=$2 AND status='AUTHENTICATING'`, sessionID, actorID, authTime.UTC())
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrForbidden
	}
	command, err = tx.Exec(ctx, `UPDATE credential_launches SET status='AUTHENTICATED' WHERE id=$1 AND status='CONSUMED'`, launchID)
	if err != nil || command.RowsAffected() != 1 {
		return ErrCredentialReplay
	}
	return r.commitCredentialIsolationMutation(ctx, tx, authority)
}

// ConfirmCredentialSession records the first browser heartbeat and returns the
// exact session from the same serializable transaction. A missed deadline never
// redirects to the IdP and cannot race a Portal-side revocation.
func (r *PostgresRepository) ConfirmCredentialSession(ctx context.Context, sessionID uuid.UUID, browserBinding string) (credentialIsolationSession, error) {
	now := r.now().UTC()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return credentialIsolationSession{}, err
	}
	defer tx.Rollback(ctx)
	var item credentialIsolationSession
	var ciphertext []byte
	var keyID, launchStatus string
	var heartbeatDeadline time.Time
	err = tx.QueryRow(ctx, `SELECT session.id,launch.id,launch.result_ref,launch.actor_user_id,launch.actor_issuer,launch.actor_subject,session.authorization_version,session.gate_epoch,launch.token_account_id,launch.action,launch.expected_account_version,launch.expected_credential_version,launch.slot,session.credential_origin,session.browser_binding_digest,session.state_digest,session.nonce_digest,session.pkce_verifier_ciphertext,session.key_id,session.csrf_digest,session.status,launch.attempt_count,session.heartbeat_at,session.expires_at,launch.status,launch.heartbeat_deadline FROM credential_isolation_sessions session JOIN credential_launches launch ON launch.id=session.launch_id WHERE session.id=$1 FOR UPDATE OF session,launch`, sessionID).Scan(&item.ID, &item.LaunchID, &item.ResultRef, &item.ActorID, &item.ActorIssuer, &item.ActorSubject, &item.AuthorizationVersion, &item.GateEpoch, &item.TokenAccountID, &item.Action, &item.ExpectedAccountVersion, &item.ExpectedCredentialVersion, &item.Slot, &item.CredentialOrigin, &item.BrowserDigest, &item.StateDigest, &item.NonceDigest, &ciphertext, &keyID, &item.CSRFDigest, &item.Status, &item.Attempts, &item.HeartbeatAt, &item.ExpiresAt, &launchStatus, &heartbeatDeadline)
	if err != nil || !secureCredentialValue(item.BrowserDigest, browserBinding) || !item.ExpiresAt.After(now) {
		return credentialIsolationSession{}, ErrForbidden
	}
	if err = r.recheckCredentialIsolationAuthority(ctx, tx, CredentialIsolationAuthoritySnapshot{ActorID: item.ActorID, Issuer: item.ActorIssuer, Subject: item.ActorSubject, AuthorizationVersion: item.AuthorizationVersion, GateEpoch: item.GateEpoch}); err != nil {
		return credentialIsolationSession{}, err
	}
	if item.Status == "AUTHENTICATING" {
		if launchStatus != "CONSUMED" || item.HeartbeatAt != nil || heartbeatDeadline.Before(now) {
			return credentialIsolationSession{}, ErrForbidden
		}
		if command, updateErr := tx.Exec(ctx, `UPDATE credential_isolation_sessions SET heartbeat_at=$2 WHERE id=$1 AND heartbeat_at IS NULL AND status='AUTHENTICATING'`, sessionID, now); updateErr != nil || command.RowsAffected() != 1 {
			if updateErr != nil {
				return credentialIsolationSession{}, updateErr
			}
			return credentialIsolationSession{}, ErrForbidden
		}
		item.HeartbeatAt = &now
	} else if item.Status == "COMPLETED" {
		if launchStatus != "COMPLETED" || item.HeartbeatAt == nil {
			return credentialIsolationSession{}, ErrForbidden
		}
		if err = r.commitCredentialIsolationMutation(ctx, tx, credentialSessionAuthority(item)); err != nil {
			return credentialIsolationSession{}, err
		}
		return item, nil
	} else if item.Status != "READY" || launchStatus != "AUTHENTICATED" || item.HeartbeatAt == nil {
		return credentialIsolationSession{}, ErrForbidden
	}
	plaintext, err := r.secrets.DecryptWithKeyID(keyID, ciphertext, []byte(item.ID.String()+":"+item.LaunchID.String()))
	if err != nil {
		return credentialIsolationSession{}, err
	}
	item.Verifier = string(plaintext)
	clear(plaintext)
	if err = r.commitCredentialIsolationMutation(ctx, tx, credentialSessionAuthority(item)); err != nil {
		return credentialIsolationSession{}, err
	}
	return item, nil
}

func (r *PostgresRepository) RevokeCredentialLaunch(ctx context.Context, actorID, resultRef uuid.UUID, code string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	if actorID == uuid.Nil || resultRef == uuid.Nil || !slices.Contains([]string{"CREDENTIAL_WINDOW_BLOCKED", "CREDENTIAL_LAUNCH_CANCELLED", "CREDENTIAL_PERMISSION_REVOKED", "CREDENTIAL_FEATURE_DISABLED"}, code) {
		return ErrForbidden
	}
	now := r.now().UTC()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var launchID, accountID uuid.UUID
	var launchStatus string
	var createdAt time.Time
	var heartbeatDeadline *time.Time
	var sessionID *uuid.UUID
	var sessionStatus *string
	var heartbeatAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT id,token_account_id,status,created_at,heartbeat_deadline FROM credential_launches WHERE result_ref=$1 AND actor_user_id=$2 FOR UPDATE`, resultRef, actorID).Scan(&launchID, &accountID, &launchStatus, &createdAt, &heartbeatDeadline); errors.Is(err, pgx.ErrNoRows) {
		return ErrCredentialReplay
	} else if err != nil {
		return err
	}
	var loadedSessionID uuid.UUID
	var loadedSessionStatus string
	err = tx.QueryRow(ctx, `SELECT id,status,heartbeat_at FROM credential_isolation_sessions WHERE launch_id=$1 FOR UPDATE`, launchID).Scan(&loadedSessionID, &loadedSessionStatus, &heartbeatAt)
	if err == nil {
		sessionID, sessionStatus = &loadedSessionID, &loadedSessionStatus
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	allowed := code != "CREDENTIAL_WINDOW_BLOCKED" && slices.Contains([]string{"CREATED", "CONSUMED", "AUTHENTICATED"}, launchStatus)
	if code == "CREDENTIAL_WINDOW_BLOCKED" && heartbeatAt == nil {
		allowed = launchStatus == "CREATED" && !now.Before(createdAt.Add(10*time.Second)) ||
			(launchStatus == "CONSUMED" || launchStatus == "AUTHENTICATED") && heartbeatDeadline != nil && !now.Before(*heartbeatDeadline)
	}
	if !allowed {
		return ErrCredentialReplay
	}
	command, err := tx.Exec(ctx, `UPDATE credential_launches SET status='REVOKED',stable_error_code=$3,completed_at=$4 WHERE id=$1 AND actor_user_id=$2 AND status=$5`, launchID, actorID, code, now, launchStatus)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrCredentialReplay
	}
	if sessionID != nil && sessionStatus != nil {
		command, err = tx.Exec(ctx, `UPDATE credential_isolation_sessions SET status='REVOKED',pkce_verifier_ciphertext='\x'::bytea,key_id='' WHERE id=$1 AND status=$2`, *sessionID, *sessionStatus)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return ErrCredentialReplay
		}
	}
	command, err = tx.Exec(ctx, `UPDATE credential_isolation_results SET status='REVOKED',stable_error_code=$2,updated_at=$3 WHERE result_ref=$1 AND status='PENDING'`, resultRef, code, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrCredentialReplay
	}
	if err = recordTokenAudit(ctx, tx, actorID, "TOKEN_CREDENTIAL_LAUNCH_REVOKED", "TOKEN_ACCOUNT", accountID, `jsonb_build_object('result_ref',$6,'launch_id',$7,'stable_error_code',$8)`, resultRef, launchID, code); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ExpireCredentialIsolation atomically terminalizes abandoned or expired
// launches. It is deliberately bounded so the janitor cannot monopolize the
// credential database during recovery.
func (r *PostgresRepository) ExpireCredentialIsolation(ctx context.Context, maximum int) (int, error) {
	if maximum < 1 || maximum > 500 {
		return 0, ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	now := r.now().UTC()
	rows, err := tx.Query(ctx, `SELECT launch.id,launch.result_ref,launch.actor_user_id,launch.token_account_id,
		CASE WHEN launch.expires_at<=$1 THEN 'EXPIRED' ELSE 'REVOKED' END,
		CASE WHEN launch.expires_at<=$1 THEN 'CREDENTIAL_LAUNCH_EXPIRED' ELSE 'CREDENTIAL_WINDOW_BLOCKED' END
		FROM credential_launches launch
		WHERE launch.status IN ('CREATED','CONSUMED','AUTHENTICATED')
		AND (launch.expires_at<=$1 OR (launch.status IN ('CONSUMED','AUTHENTICATED') AND launch.heartbeat_deadline<$1
			AND NOT EXISTS(SELECT 1 FROM credential_isolation_sessions session WHERE session.launch_id=launch.id AND session.heartbeat_at IS NOT NULL)))
		ORDER BY launch.expires_at,launch.id LIMIT $2 FOR UPDATE OF launch SKIP LOCKED`, now, maximum)
	if err != nil {
		return 0, err
	}
	type abandoned struct {
		launchID, resultRef, actorID, accountID uuid.UUID
		status, code                            string
	}
	items := []abandoned{}
	for rows.Next() {
		var item abandoned
		if err = rows.Scan(&item.launchID, &item.resultRef, &item.actorID, &item.accountID, &item.status, &item.code); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, item := range items {
		if _, err = tx.Exec(ctx, `UPDATE credential_launches SET status=$2,stable_error_code=$3,completed_at=$4 WHERE id=$1`, item.launchID, item.status, item.code, now); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `UPDATE credential_isolation_sessions SET status=$2,pkce_verifier_ciphertext='\x'::bytea,key_id='' WHERE launch_id=$1 AND status IN ('AUTHENTICATING','READY')`, item.launchID, item.status); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `UPDATE credential_isolation_results SET status=$2,stable_error_code=$3,updated_at=$4 WHERE result_ref=$1 AND status='PENDING'`, item.resultRef, item.status, item.code, now); err != nil {
			return 0, err
		}
		if err = recordTokenAudit(ctx, tx, item.actorID, "TOKEN_CREDENTIAL_LAUNCH_"+item.status, "TOKEN_ACCOUNT", item.accountID, `jsonb_build_object('result_ref',$6,'launch_id',$7,'stable_error_code',$8)`, item.resultRef, item.launchID, item.code); err != nil {
			return 0, err
		}
	}
	return len(items), tx.Commit(ctx)
}

func (r *PostgresRepository) CompleteCredentialSession(ctx context.Context, sessionID uuid.UUID, credentialOrigin, browserBinding, csrf, provider, baseURL string, allowedModels []string, secret []byte, validator CredentialValidator, policy ProviderURLPolicy) (CredentialIsolationResult, error) {
	defer clear(secret)
	item, err := r.CredentialIsolationSession(ctx, sessionID)
	if err != nil {
		return CredentialIsolationResult{}, err
	}
	now := r.now().UTC()
	provider, baseURL, allowedModels = strings.ToUpper(strings.TrimSpace(provider)), strings.TrimRight(strings.TrimSpace(baseURL), "/"), normalizeModels(allowedModels)
	if item.Status != "READY" || item.CredentialOrigin != strings.TrimRight(strings.TrimSpace(credentialOrigin), "/") || !item.ExpiresAt.After(now) || !secureCredentialValue(item.BrowserDigest, browserBinding) || !secureCredentialValue(item.CSRFDigest, csrf) || provider == "" || len(secret) < 8 || len(allowedModels) == 0 || policy.Validate(baseURL) != nil {
		return CredentialIsolationResult{}, ErrForbidden
	}
	if err = r.CheckCredentialIsolationSessionAuthority(ctx, sessionID); err != nil {
		return CredentialIsolationResult{}, err
	}
	if validator == nil || validator.Validate(ctx, baseURL, secret, allowedModels) != nil {
		if failureErr := r.recordCredentialValidationFailure(ctx, item, now); failureErr != nil {
			return CredentialIsolationResult{}, failureErr
		}
		return CredentialIsolationResult{}, ErrInvalidSecret
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CredentialIsolationResult{}, err
	}
	defer tx.Rollback(ctx)
	var lockedStatus, lockedBrowserDigest, lockedCSRFDigest string
	var lockedExpiry time.Time
	if err = tx.QueryRow(ctx, `SELECT session.status,session.browser_binding_digest,session.csrf_digest,session.expires_at FROM credential_isolation_sessions session JOIN credential_launches launch ON launch.id=session.launch_id WHERE session.id=$1 AND launch.id=$2 AND launch.status='AUTHENTICATED' FOR UPDATE OF session,launch`, sessionID, item.LaunchID).Scan(&lockedStatus, &lockedBrowserDigest, &lockedCSRFDigest, &lockedExpiry); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CredentialIsolationResult{}, ErrCredentialReplay
		}
		return CredentialIsolationResult{}, err
	}
	if lockedStatus != "READY" || !lockedExpiry.After(now) || !secureCredentialValue(lockedBrowserDigest, browserBinding) || !secureCredentialValue(lockedCSRFDigest, csrf) {
		return CredentialIsolationResult{}, ErrForbidden
	}
	if err = r.recheckCredentialIsolationAuthority(ctx, tx, CredentialIsolationAuthoritySnapshot{ActorID: item.ActorID, Issuer: item.ActorIssuer, Subject: item.ActorSubject, AuthorizationVersion: item.AuthorizationVersion, GateEpoch: item.GateEpoch}); err != nil {
		return CredentialIsolationResult{}, err
	}
	var accountVersion int64
	if err = tx.QueryRow(ctx, `SELECT management_version FROM token_accounts WHERE id=$1 AND status='ACTIVE' AND management_mode='PLATFORM' FOR UPDATE`, item.TokenAccountID).Scan(&accountVersion); err != nil {
		return CredentialIsolationResult{}, err
	}
	if accountVersion != item.ExpectedAccountVersion {
		return CredentialIsolationResult{}, ErrCatalogVersionConflict
	}
	rows, err := tx.Query(ctx, `SELECT id,version,status FROM provider_credentials WHERE token_account_id=$1 ORDER BY version,id FOR UPDATE`, item.TokenAccountID)
	if err != nil {
		return CredentialIsolationResult{}, err
	}
	var activeVersion, maximumVersion int
	var priorActiveID uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var version int
		var status string
		if err = rows.Scan(&id, &version, &status); err != nil {
			rows.Close()
			return CredentialIsolationResult{}, err
		}
		if version > maximumVersion {
			maximumVersion = version
		}
		if status == "ACTIVE" {
			if activeVersion != 0 {
				rows.Close()
				return CredentialIsolationResult{}, ErrCatalogVersionConflict
			}
			activeVersion, priorActiveID = version, id
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return CredentialIsolationResult{}, err
	}
	rows.Close()
	if activeVersion != item.ExpectedCredentialVersion {
		return CredentialIsolationResult{}, ErrCatalogVersionConflict
	}
	if item.Action == "ENROLL" && activeVersion != 0 || item.Action == "ROTATE" && activeVersion < 1 {
		return CredentialIsolationResult{}, ErrCatalogVersionConflict
	}
	var routeCompatible bool
	if err = tx.QueryRow(ctx, `SELECT NOT EXISTS(
		SELECT 1 FROM routing_policies route JOIN model_identities identity ON identity.id=route.model_id
		WHERE route.token_account_id=$1 AND route.reference_status='RESOLVED'
		AND (identity.provider_id<>$2 OR NOT identity.upstream_model_ref=ANY($3::text[])))`, item.TokenAccountID, provider, allowedModels).Scan(&routeCompatible); err != nil {
		return CredentialIsolationResult{}, err
	}
	if !routeCompatible {
		return CredentialIsolationResult{}, ErrCatalogVersionConflict
	}
	credentialID, version := uuid.Must(uuid.NewV7()), maximumVersion+1
	ciphertext, err := r.secrets.Encrypt(secret, []byte(item.TokenAccountID.String()+":"+credentialID.String()))
	if err != nil {
		return CredentialIsolationResult{}, err
	}
	fingerprint := Fingerprint(secret)
	if _, err = tx.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status,validation_checked_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'CANDIDATE',$9,$10,$11,$10,'VALID',$11)`, credentialID, item.TokenAccountID, provider, baseURL, ciphertext, r.secrets.KeyID(), fingerprint, version, allowedModels, now, item.ActorID); err != nil {
		return CredentialIsolationResult{}, err
	}
	if err = recordTokenAudit(ctx, tx, item.ActorID, "TOKEN_CREDENTIAL_CANDIDATE_CREATED", "PROVIDER_CREDENTIAL", credentialID, `jsonb_build_object('token_account_id',$6,'credential_version',$7,'status','CANDIDATE','validation_status','VALID','fingerprint',$8,'result_ref',$9)`, item.TokenAccountID, version, fingerprint, item.ResultRef); err != nil {
		return CredentialIsolationResult{}, err
	}
	if priorActiveID != uuid.Nil {
		if command, updateErr := tx.Exec(ctx, `UPDATE provider_credentials SET status='RETIRING' WHERE id=$1 AND token_account_id=$2 AND status='ACTIVE' AND version=$3`, priorActiveID, item.TokenAccountID, activeVersion); updateErr != nil || command.RowsAffected() != 1 {
			if updateErr != nil {
				return CredentialIsolationResult{}, updateErr
			}
			return CredentialIsolationResult{}, ErrCatalogVersionConflict
		}
		if err = recordTokenAudit(ctx, tx, item.ActorID, "TOKEN_CREDENTIAL_RETIRING", "PROVIDER_CREDENTIAL", priorActiveID, `jsonb_build_object('token_account_id',$6,'credential_version',$7,'status','RETIRING','replacement_credential_id',$8,'result_ref',$9)`, item.TokenAccountID, activeVersion, credentialID, item.ResultRef); err != nil {
			return CredentialIsolationResult{}, err
		}
	}
	if command, updateErr := tx.Exec(ctx, `UPDATE provider_credentials SET status='ACTIVE',activated_at=$2 WHERE id=$1 AND status='CANDIDATE'`, credentialID, now); updateErr != nil || command.RowsAffected() != 1 {
		if updateErr != nil {
			return CredentialIsolationResult{}, updateErr
		}
		return CredentialIsolationResult{}, ErrCredentialReplay
	}
	if err = recordTokenAudit(ctx, tx, item.ActorID, "TOKEN_CREDENTIAL_ACTIVATED", "PROVIDER_CREDENTIAL", credentialID, `jsonb_build_object('token_account_id',$6,'credential_version',$7,'status','ACTIVE','validation_status','VALID','fingerprint',$8,'result_ref',$9)`, item.TokenAccountID, version, fingerprint, item.ResultRef); err != nil {
		return CredentialIsolationResult{}, err
	}
	if priorActiveID != uuid.Nil {
		if command, updateErr := tx.Exec(ctx, `UPDATE provider_credentials SET status='RETIRED',retired_at=$2 WHERE id=$1 AND status='RETIRING'`, priorActiveID, now); updateErr != nil || command.RowsAffected() != 1 {
			if updateErr != nil {
				return CredentialIsolationResult{}, updateErr
			}
			return CredentialIsolationResult{}, ErrCredentialReplay
		}
		if err = recordTokenAudit(ctx, tx, item.ActorID, "TOKEN_CREDENTIAL_RETIRED", "PROVIDER_CREDENTIAL", priorActiveID, `jsonb_build_object('token_account_id',$6,'credential_version',$7,'status','RETIRED','replacement_credential_id',$8,'result_ref',$9)`, item.TokenAccountID, activeVersion, credentialID, item.ResultRef); err != nil {
			return CredentialIsolationResult{}, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE token_accounts SET management_version=management_version+1,updated_at=$2 WHERE id=$1`, item.TokenAccountID, now); err != nil {
		return CredentialIsolationResult{}, err
	}
	command, err := tx.Exec(ctx, `UPDATE credential_isolation_sessions SET status='COMPLETED',pkce_verifier_ciphertext=decode('','hex'),key_id='' WHERE id=$1 AND status='READY'`, sessionID)
	if err != nil {
		return CredentialIsolationResult{}, err
	}
	if command.RowsAffected() != 1 {
		return CredentialIsolationResult{}, ErrCredentialReplay
	}
	command, err = tx.Exec(ctx, `UPDATE credential_launches SET status='COMPLETED',completed_at=$2 WHERE id=$1 AND status='AUTHENTICATED'`, item.LaunchID, now)
	if err != nil {
		return CredentialIsolationResult{}, err
	}
	if command.RowsAffected() != 1 {
		return CredentialIsolationResult{}, ErrCredentialReplay
	}
	command, err = tx.Exec(ctx, `UPDATE credential_isolation_results SET status='SUCCEEDED',credential_id=$2,credential_version=$3,credential_status='ACTIVE',validation_status='VALID',fingerprint=$4,updated_at=$5 WHERE result_ref=$1 AND status='PENDING'`, item.ResultRef, credentialID, version, fingerprint, now)
	if err != nil {
		return CredentialIsolationResult{}, err
	}
	if command.RowsAffected() != 1 {
		return CredentialIsolationResult{}, ErrCredentialReplay
	}
	if err = recordTokenAudit(ctx, tx, item.ActorID, "TOKEN_CREDENTIAL_"+item.Action+"_COMPLETED", "PROVIDER_CREDENTIAL", credentialID, `jsonb_build_object('token_account_id',$6,'credential_version',$7,'status','ACTIVE','validation_status','VALID','fingerprint',$8,'result_ref',$9)`, item.TokenAccountID, version, fingerprint, item.ResultRef); err != nil {
		return CredentialIsolationResult{}, err
	}
	if err = r.commitCredentialIsolationMutation(ctx, tx, credentialSessionAuthority(item)); err != nil {
		return CredentialIsolationResult{}, err
	}
	return r.CredentialIsolationResult(ctx, item.ActorID, item.ResultRef)
}

func (r *PostgresRepository) recordCredentialValidationFailure(ctx context.Context, item credentialIsolationSession, now time.Time) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var attempts int
	var launchStatus, sessionStatus string
	if err = tx.QueryRow(ctx, `SELECT launch.attempt_count,launch.status,session.status FROM credential_launches launch JOIN credential_isolation_sessions session ON session.launch_id=launch.id WHERE launch.id=$1 AND session.id=$2 FOR UPDATE OF launch,session`, item.LaunchID, item.ID).Scan(&attempts, &launchStatus, &sessionStatus); err != nil {
		return err
	}
	if launchStatus != "AUTHENTICATED" || sessionStatus != "READY" || attempts >= 3 {
		return ErrCredentialReplay
	}
	if err = r.recheckCredentialIsolationAuthority(ctx, tx, credentialSessionAuthority(item)); err != nil {
		return err
	}
	attempts++
	if attempts < 3 {
		if _, err = tx.Exec(ctx, `UPDATE credential_launches SET attempt_count=$2 WHERE id=$1`, item.LaunchID, attempts); err != nil {
			return err
		}
		return r.commitCredentialIsolationMutation(ctx, tx, credentialSessionAuthority(item))
	}
	if _, err = tx.Exec(ctx, `UPDATE credential_launches SET attempt_count=$2,status='FAILED',stable_error_code='CREDENTIAL_VALIDATION_FAILED',completed_at=$3 WHERE id=$1`, item.LaunchID, attempts, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE credential_isolation_sessions SET status='FAILED',pkce_verifier_ciphertext='\x'::bytea,key_id='' WHERE id=$1`, item.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE credential_isolation_results SET status='FAILED',stable_error_code='CREDENTIAL_VALIDATION_FAILED',updated_at=$2 WHERE result_ref=$1 AND status='PENDING'`, item.ResultRef, now); err != nil {
		return err
	}
	if err = recordTokenAudit(ctx, tx, item.ActorID, "TOKEN_CREDENTIAL_VALIDATION_FAILED", "TOKEN_ACCOUNT", item.TokenAccountID, `jsonb_build_object('result_ref',$6,'launch_id',$7,'attempt_count',$8,'stable_error_code','CREDENTIAL_VALIDATION_FAILED')`, item.ResultRef, item.LaunchID, attempts); err != nil {
		return err
	}
	return r.commitCredentialIsolationMutation(ctx, tx, credentialSessionAuthority(item))
}

func (s *Service) adminCreateCredentialLaunchFor(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, ok := adminUUID(w, r, "token_account_id")
		if !ok {
			return
		}
		var input CredentialLaunchRequest
		if api.DecodeJSON(w, r, &input) != nil {
			return
		}
		if input.Action != "" && strings.ToUpper(strings.TrimSpace(input.Action)) != action {
			api.WriteError(w, http.StatusBadRequest, "INVALID_CREDENTIAL_LAUNCH", "the credential launch action is invalid", httpserver.RequestID(r.Context()))
			return
		}
		input.Action = action
		principal, ok := identity.FromContext(r.Context())
		if !ok {
			api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "authentication is required", httpserver.RequestID(r.Context()))
			return
		}
		if principal.AuthenticatedAt.IsZero() || time.Since(principal.AuthenticatedAt) > 15*time.Minute {
			api.WriteError(w, http.StatusUnauthorized, "RECENT_AUTH_REQUIRED", "recent authentication is required", httpserver.RequestID(r.Context()))
			return
		}
		if s.authorizer == nil {
			api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "authorization is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		actorID, authorizationVersion, err := s.authorizer.AuthorizedUserIDAll(r.Context(), principal, "platform.tokens.manage")
		selection, selected := r.Context().Value(tokenRouteActivationContextKey{}).(tokenRouteActivationSelection)
		if err != nil || actorID == uuid.Nil || authorizationVersion < 1 || !selected || !selection.transactional || selection.State != "ACTIVE" || selection.Epoch < 1 {
			api.WriteError(w, http.StatusForbidden, "TOKEN_CREDENTIAL_AUTHORIZATION_INVALID", "credential authorization could not be frozen", httpserver.RequestID(r.Context()))
			return
		}
		authority := CredentialIsolationAuthoritySnapshot{ActorID: actorID, Issuer: strings.TrimSpace(principal.Issuer), Subject: strings.TrimSpace(principal.Subject), AuthorizationVersion: authorizationVersion, GateEpoch: selection.Epoch}
		repository, ok := s.repository.(*PostgresRepository)
		if !ok {
			api.WriteError(w, 503, "ADMIN_UNAVAILABLE", "credential isolation is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		result, err := repository.CreateCredentialLaunch(withCredentialIsolationAuthority(r.Context(), authority), accountID, actorID, input, r.Header.Get("Idempotency-Key"), httpserver.RequestID(r.Context()))
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		api.WriteJSON(w, http.StatusCreated, result)
	}
}

func (s *Service) adminCredentialIsolationResult(w http.ResponseWriter, r *http.Request) {
	resultRef, ok := adminUUID(w, r, "result_ref")
	if !ok {
		return
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		api.WriteError(w, 503, "ADMIN_UNAVAILABLE", "credential isolation is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.CredentialIsolationResult(r.Context(), tokenAdminActor(r), resultRef)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminRevokeCredentialLaunch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ReasonCode string `json:"reason_code"`
	}
	if api.DecodeJSON(w, r, &input) != nil {
		return
	}
	resultRef, ok := adminUUID(w, r, "result_ref")
	if !ok {
		return
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		api.WriteError(w, 503, "ADMIN_UNAVAILABLE", "credential isolation is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if err := repository.RevokeCredentialLaunch(r.Context(), tokenAdminActor(r), resultRef, input.ReasonCode); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
