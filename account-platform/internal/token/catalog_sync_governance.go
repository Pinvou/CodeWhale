package token

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// executeGovernedCatalogSync is the APT-138-ready domain adapter. Production
// cannot reach it until the persistent governance activation reader selects
// ACTIVE. It deliberately performs the sole provider re-observation before
// opening the publication transaction, then revalidates all frozen catalog
// facts under the publication lock and persists the checked slice atomically.
func (s *Service) executeGovernedCatalogSync(ctx context.Context, repository *PostgresRepository, input runtimeTokenCommandBegin) (TokenCommandResult, bool, error) {
	if s.catalogSync == nil || repository == nil {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	if existing, err := existingCatalogSyncCommand(ctx, repository.pool, input); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return TokenCommandResult{}, false, err
	}
	models, observedVersion, err := s.catalogSync.discoverObservation(ctx)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	return repository.executeGovernedCatalogSyncTx(ctx, input, s.catalogSync, models, observedVersion)
}

func existingCatalogSyncCommand(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, input runtimeTokenCommandBegin) (TokenCommandResult, error) {
	item, err := scanTokenCommandResult(database.QueryRow(ctx, tokenCommandResultSelect+` WHERE actor_user_id=$1 AND command_type='TOKEN_MODEL_CATALOG_SYNC' AND idempotency_key_digest=$2`, input.ActorUserID, input.KeyDigest))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenCommandResult{}, ErrNotFound
	}
	if err != nil {
		return TokenCommandResult{}, err
	}
	if item.IdempotencyFingerprint != input.IdempotencyFingerprint {
		return TokenCommandResult{}, ErrTokenCommandResultMismatch
	}
	return item, nil
}

func (r *PostgresRepository) executeGovernedCatalogSyncTx(ctx context.Context, input runtimeTokenCommandBegin, synchronizer *CatalogSynchronizer, models []DiscoveredModel, observedVersion string) (TokenCommandResult, bool, error) {
	canonical := input.Envelope
	if input.ActorUserID == uuid.Nil || input.AuthorizationVersion < 1 || strings.TrimSpace(input.ActorPermission) == "" || strings.TrimSpace(input.AuditRequestID) == "" || canonical.CommandType != "TOKEN_MODEL_CATALOG_SYNC" || canonical.TargetResourceType != "MODEL_CATALOG" || canonical.TargetResourceID != "CURRENT" || len(input.RequestDigest) != 64 || len(input.KeyDigest) != 64 || len(input.IdempotencyFingerprint) != 64 || synchronizer == nil {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	policy, err := approvalpolicy.Lookup(canonical.CommandType)
	if err != nil || policy.Mode != approvalpolicy.AuditOnly {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	defer tx.Rollback(ctx)
	if existing, existingErr := existingCatalogSyncCommand(ctx, tx, input); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	if err := lockCatalogPublication(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	// A concurrent same-key command may have committed while this transaction
	// waited for the publication lock. Recover it without another provider call.
	if existing, existingErr := existingCatalogSyncCommand(ctx, tx, input); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	version, current, err := listCatalog(ctx, tx)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	var observedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&observedAt); err != nil {
		return TokenCommandResult{}, false, err
	}
	preview := catalogPreviewFromObservation(version, current, models, observedVersion, synchronizer.providerID, observedAt)
	snapshot := catalogSyncCommandSnapshot(preview)
	authoritativeExpected, authoritativeFingerprint, err := commandPrecheckFingerprint(canonical.CommandType, canonical.TargetResourceType, canonical.TargetResourceID, canonical.Command, snapshot)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if canonical.PrecheckFingerprint != authoritativeFingerprint || !tokenCommandExpectedVersionsMatch(canonical.ExpectedVersions, authoritativeExpected) {
		return TokenCommandResult{}, false, ErrCatalogVersionConflict
	}
	// APT-138 will own the persistent activation record and its state changes.
	// The adapter is already transaction-safe: lock and compare the activation
	// epoch selected by middleware immediately before the first immutable fact
	// write. Production remains PRE_ACTIVATION because it does not install the
	// reader or this guard yet.
	if err := r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	reference := uuid.Must(uuid.NewV7())
	insert, err := tx.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,before_state,audit_request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'PENDING',$15,$16) ON CONFLICT(actor_user_id,command_type,idempotency_key_digest) DO NOTHING`, reference, canonical.CommandType, policy.PolicyVersion, input.ActorUserID, input.ActorPermission, input.AuthorizationVersion, input.KeyDigest, input.IdempotencyFingerprint, input.RequestDigest, canonical.TargetResourceType, canonical.TargetResourceID, canonical.Reason, canonical.ExpectedVersions, canonical.PrecheckFingerprint, authoritativeExpected, input.AuditRequestID)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if insert.RowsAffected() == 0 {
		existing, existingErr := existingCatalogSyncCommand(ctx, tx, input)
		if existingErr != nil {
			return TokenCommandResult{}, false, existingErr
		}
		return existing, false, tx.Commit(ctx)
	}
	catalogVersion, changed, err := r.syncCatalogTx(ctx, tx, synchronizer.providerID, synchronizer.adapterVersion, models, synchronizer.contextLength, synchronizer.maximumOutput, input.ActorUserID)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	result, err := json.Marshal(map[string]any{"catalog_version": catalogVersion, "changed": changed})
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	item, err := completeRuntimeTokenCommandTx(ctx, tx, input.tokenCommandBegin, canonical, reference, input.IdempotencyFingerprint, authoritativeExpected, result, result)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	return item, true, tx.Commit(ctx)
}
