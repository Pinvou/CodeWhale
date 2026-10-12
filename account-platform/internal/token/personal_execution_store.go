package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) BeginUsage(ctx context.Context, request UsageRequest, snapshot map[string]any) (UsageRequest, bool, error) {
	if (request.OrganizationID != nil && *request.OrganizationID == uuid.Nil) || request.ID == uuid.Nil || len(request.IdempotencyKeyDigest) != 64 || request.IdempotencyNamespace != "model-inference" || request.CanonicalVersion <= 0 || len(request.RequestFingerprint) != 64 || request.UserID == uuid.Nil || request.SessionID == "" || request.TokenAccountID == uuid.Nil || request.CredentialID == uuid.Nil || request.WalletID == uuid.Nil || request.Model == "" || request.MaximumMicrocredits < 0 {
		return UsageRequest{}, false, ErrForbidden
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return UsageRequest{}, false, err
	}
	request.RequestSnapshot = append(json.RawMessage(nil), encoded...)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return UsageRequest{}, false, err
	}
	defer tx.Rollback(ctx)
	// Serialize this invocation before looking up its durable identity. A retry
	// waiting behind a peer must read that peer's accepted row before applying
	// limits for a new invocation, including at the exact admission threshold.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "usage:idempotency:"+request.UserID.String()+":"+request.IdempotencyNamespace+":"+request.IdempotencyKeyDigest); err != nil {
		return UsageRequest{}, false, err
	}
	existing, err := getUsageByIdempotency(ctx, tx, request.UserID, request.IdempotencyKeyDigest, true)
	if err == nil {
		if existing.CanonicalVersion != request.CanonicalVersion || existing.RequestFingerprint != request.RequestFingerprint {
			return UsageRequest{}, false, ErrIdempotencyConflict
		}
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return UsageRequest{}, false, err
	}
	if err := lockReplayUserTx(ctx, tx, request.UserID); err != nil {
		return UsageRequest{}, false, err
	}
	// Accepted immutable receipts above stay replayable after lifecycle changes.
	// Only new invocations participate in the organization lifecycle gate.
	if request.OrganizationID != nil {
		if err := lockActiveOrganization(ctx, tx, *request.OrganizationID); err != nil {
			return UsageRequest{}, false, err
		}
	}
	now := r.now()
	if err := admitUsage(ctx, tx, request, now); err != nil {
		return UsageRequest{}, false, err
	}

	request.Status, request.ExecutionStatus, request.ExecutionVersion, request.DeliveryStatus = "CREATED", "RECEIVED", 1, "NOT_STARTED"
	request.CancelStatus, request.SettlementStatus, request.StartedAt = "NOT_REQUESTED", "HOLD_PENDING", now
	command, err := tx.Exec(ctx, `INSERT INTO usage_requests(id,idempotency_namespace,idempotency_key_digest,canonicalization_version,request_fingerprint,user_id,session_id,organization_id,token_account_id,credential_id,wallet_id,model,model_id,catalog_version,model_capability_version_id,protocol_definition_id,route_generation,model_snapshot,status,execution_status,execution_version,delivery_status,maximum_microcredits,request_snapshot,started_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,'CREATED','RECEIVED',1,'NOT_STARTED',$19,$20,$21,$21) ON CONFLICT(user_id,idempotency_namespace,idempotency_key_digest) WHERE idempotency_key_digest IS NOT NULL DO NOTHING`, request.ID, request.IdempotencyNamespace, request.IdempotencyKeyDigest, request.CanonicalVersion, request.RequestFingerprint, request.UserID, request.SessionID, request.OrganizationID, request.TokenAccountID, request.CredentialID, request.WalletID, request.Model, request.ModelID, request.CatalogVersion, request.CapabilityVersionID, request.ProtocolDefinitionID, request.RouteGeneration, request.ModelSnapshot, request.MaximumMicrocredits, encoded, now)
	if err != nil {
		return UsageRequest{}, false, err
	}
	if command.RowsAffected() == 0 {
		existing, err = getUsageByIdempotency(ctx, tx, request.UserID, request.IdempotencyKeyDigest, true)
		if err != nil {
			return UsageRequest{}, false, err
		}
		if existing.CanonicalVersion != request.CanonicalVersion || existing.RequestFingerprint != request.RequestFingerprint {
			return UsageRequest{}, false, ErrIdempotencyConflict
		}
		return existing, false, tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `INSERT INTO usage_execution_transitions(id,usage_request_id,from_status,to_status,from_version,to_version,reason,occurred_at) VALUES($1,$2,'NONE','RECEIVED',0,1,'request accepted',$3)`, uuid.Must(uuid.NewV7()), request.ID, now)
	if err != nil {
		return UsageRequest{}, false, err
	}
	return request, true, tx.Commit(ctx)
}

func (r *PostgresRepository) ListUsageForContext(ctx context.Context, resolved AuthorizationContext, limit int, cursor *UsageHistoryCursor) ([]UserUsageRecord, error) {
	if resolved.UserID == uuid.Nil || limit < 1 || limit > 101 {
		return nil, ErrForbidden
	}
	query := `SELECT id,COALESCE(request_snapshot->>'logical_turn_id',''),COALESCE(model_snapshot->>'display_name',model),status,execution_status,delivery_status,cancel_status,settlement_status,actual_microcredits,usage_snapshot,started_at,terminal_at,updated_at
FROM usage_requests
WHERE user_id=$1 AND organization_id IS NULL
ORDER BY started_at DESC,id DESC LIMIT $2`
	arguments := []any{resolved.UserID, limit}
	if cursor != nil {
		query = `SELECT id,COALESCE(request_snapshot->>'logical_turn_id',''),COALESCE(model_snapshot->>'display_name',model),status,execution_status,delivery_status,cancel_status,settlement_status,actual_microcredits,usage_snapshot,started_at,terminal_at,updated_at
FROM usage_requests
WHERE user_id=$1 AND organization_id IS NULL AND (started_at,id)<($2,$3)
ORDER BY started_at DESC,id DESC LIMIT $4`
		arguments = []any{resolved.UserID, cursor.StartedAt, cursor.ID, limit}
	}
	if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil {
		query = `SELECT id,COALESCE(request_snapshot->>'logical_turn_id',''),COALESCE(model_snapshot->>'display_name',model),status,execution_status,delivery_status,cancel_status,settlement_status,actual_microcredits,usage_snapshot,started_at,terminal_at,updated_at
FROM usage_requests
WHERE user_id=$1 AND organization_id=$2
ORDER BY started_at DESC,id DESC LIMIT $3`
		arguments = []any{resolved.UserID, *resolved.OrganizationID, limit}
		if cursor != nil {
			query = `SELECT id,COALESCE(request_snapshot->>'logical_turn_id',''),COALESCE(model_snapshot->>'display_name',model),status,execution_status,delivery_status,cancel_status,settlement_status,actual_microcredits,usage_snapshot,started_at,terminal_at,updated_at
FROM usage_requests
WHERE user_id=$1 AND organization_id=$2 AND (started_at,id)<($3,$4)
ORDER BY started_at DESC,id DESC LIMIT $5`
			arguments = []any{resolved.UserID, *resolved.OrganizationID, cursor.StartedAt, cursor.ID, limit}
		}
	} else if resolved.ContextType != "PERSONAL" {
		return nil, ErrForbidden
	}
	rows, err := r.pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]UserUsageRecord, 0)
	for rows.Next() {
		var item UserUsageRecord
		var snapshot []byte
		if err := rows.Scan(&item.UsageRequestID, &item.LogicalTurnID, &item.Model, &item.status, &item.ExecutionStatus, &item.DeliveryStatus, &item.CancelStatus, &item.SettlementStatus, &item.ActualMicrocredits, &snapshot, &item.StartedAt, &item.CompletedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if len(snapshot) > 0 {
			var payload struct {
				Usage *Usage `json:"usage"`
			}
			if json.Unmarshal(snapshot, &payload) == nil && payload.Usage != nil {
				input, output, cached := payload.Usage.InputTokens, payload.Usage.OutputTokens, payload.Usage.CachedInputTokens
				item.InputTokens, item.OutputTokens, item.CachedInputTokens = &input, &output, &cached
			}
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) UsageHistoryProjectionReady(ctx context.Context) (bool, error) {
	var ready bool
	err := r.pool.QueryRow(ctx, `SELECT completed_at IS NOT NULL FROM usage_history_projection_state WHERE singleton`).Scan(&ready)
	return ready, err
}

func (r *PostgresRepository) BackfillUsageHistoryProjection(ctx context.Context, batchSize int) (int, bool, error) {
	var processed int
	var complete bool
	err := r.pool.QueryRow(ctx, `SELECT processed,complete FROM backfill_usage_history_projection($1)`, batchSize).Scan(&processed, &complete)
	return processed, complete, err
}

func (r *PostgresRepository) ListUsageTurnsForContext(ctx context.Context, resolved AuthorizationContext, limit int, cursor *UsageHistoryCursor) ([]UserUsageTurnRecord, error) {
	if resolved.UserID == uuid.Nil || limit < 1 || limit > 101 {
		return nil, ErrForbidden
	}
	organizationScopeID := uuid.Nil
	if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil {
		organizationScopeID = *resolved.OrganizationID
	} else if resolved.ContextType != "PERSONAL" {
		return nil, ErrForbidden
	}
	var cursorStartedAt *time.Time
	var cursorID *uuid.UUID
	if cursor != nil {
		cursorStartedAt, cursorID = &cursor.StartedAt, &cursor.ID
	}
	// The durable group-head projection makes deep pagination an indexed lookup.
	// SQL aggregation returns exactly one bounded row per selected logical turn,
	// regardless of how many model requests the turn contains.
	query := usageTurnProjectionQuery
	arguments := []any{resolved.UserID, organizationScopeID, cursorStartedAt, cursorID, limit}
	rows, err := r.pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]UserUsageTurnRecord, 0)
	for rows.Next() {
		var item UserUsageTurnRecord
		if err := rows.Scan(&item.UsageRequestID, &item.LogicalTurnID, &item.UsageRequestCount, &item.CancelUsageRequestID, &item.Model, &item.DisplayStatus, &item.ExecutionStatus, &item.DeliveryStatus, &item.CancelStatus, &item.SettlementStatus, &item.ActualMicrocredits, &item.ChargedMicrocredits, &item.InputTokens, &item.OutputTokens, &item.CachedInputTokens, &item.StartedAt, &item.cursorStartedAt, &item.CompletedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) MarkReserved(ctx context.Context, usageID, holdID, priceVersionID uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE usage_requests SET status='RESERVED',hold_id=$2,price_version_id=$3,updated_at=$4 WHERE id=$1 AND status='CREATED'`, usageID, holdID, priceVersionID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return r.ensureUsageState(ctx, usageID, "RESERVED", "UPSTREAM_ACTIVE", "SETTLEMENT_PENDING", "COMMITTED")
	}
	if err := transitionExecutionTx(ctx, tx, usageID, []string{"HOLD_RESERVING"}, "HOLD_RESERVED", "billing hold reserved", now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) IncreaseMaximum(ctx context.Context, usageID uuid.UUID, additional int64) error {
	if usageID == uuid.Nil || additional <= 0 {
		return ErrForbidden
	}
	command, err := r.pool.Exec(ctx, `UPDATE usage_requests SET maximum_microcredits=maximum_microcredits+$2,updated_at=$3 WHERE id=$1 AND status IN ('RESERVED','UPSTREAM_ACTIVE','SETTLEMENT_PENDING','REVIEW')`, usageID, additional, r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrForbidden
	}
	return nil
}

func (r *PostgresRepository) MarkUpstream(ctx context.Context, usageID uuid.UUID) error {
	command, err := r.pool.Exec(ctx, `UPDATE usage_requests SET status='UPSTREAM_ACTIVE',updated_at=$2 WHERE id=$1 AND status='RESERVED'`, usageID, r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return r.ensureUsageState(ctx, usageID, "UPSTREAM_ACTIVE", "SETTLEMENT_PENDING", "COMMITTED")
	}
	return nil
}

func (r *PostgresRepository) RecordUpstreamRequest(ctx context.Context, usageID uuid.UUID, evidence UpstreamRequestEvidence) error {
	evidence.ProviderID = strings.ToUpper(strings.TrimSpace(evidence.ProviderID))
	evidence.AdapterVersion = strings.TrimSpace(evidence.AdapterVersion)
	evidence.RequestID = strings.TrimSpace(evidence.RequestID)
	if usageID == uuid.Nil || evidence.ProviderID == "" || len(evidence.ProviderID) > 80 || evidence.AdapterVersion == "" || len(evidence.AdapterVersion) > 80 || evidence.RequestID == "" || len(evidence.RequestID) > 128 {
		return ErrForbidden
	}
	digest := sha256.Sum256([]byte(evidence.RequestID))
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `INSERT INTO usage_attempts(id,usage_request_id,provider_id,adapter_version,upstream_request_id,upstream_reference_digest,status,started_at)
VALUES($1,$2,$3,$4,$5,$6,'STARTED',$7)
ON CONFLICT(usage_request_id) DO UPDATE SET provider_id=EXCLUDED.provider_id,adapter_version=EXCLUDED.adapter_version,upstream_request_id=EXCLUDED.upstream_request_id,upstream_reference_digest=EXCLUDED.upstream_reference_digest
WHERE usage_attempts.provider_id=EXCLUDED.provider_id AND usage_attempts.adapter_version=EXCLUDED.adapter_version AND usage_attempts.upstream_reference_digest=EXCLUDED.upstream_reference_digest`, uuid.Must(uuid.NewV7()), usageID, evidence.ProviderID, evidence.AdapterVersion, evidence.RequestID, hex.EncodeToString(digest[:]), now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrForbidden
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usageID, "UPSTREAM_REFERENCE", "GATEWAY", "RECORDED", hex.EncodeToString(digest[:]), map[string]any{"provider_id": evidence.ProviderID, "adapter_version": evidence.AdapterVersion, "upstream_reference_digest": hex.EncodeToString(digest[:])}, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) MarkCommitted(ctx context.Context, usageID uuid.UUID, actual int64, snapshot map[string]any) error {
	if usageID == uuid.Nil || actual < 0 {
		return ErrForbidden
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	var walletID, tokenAccountID uuid.UUID
	err = tx.QueryRow(ctx, `UPDATE usage_requests SET status='COMMITTED',settlement_status='SETTLED',actual_microcredits=$2,usage_snapshot=$3,trusted_usage_snapshot=$3,trusted_usage_source=COALESCE(trusted_usage_source,'PROVIDER_FINAL_USAGE'),provider_terminal_at=COALESCE(provider_terminal_at,$4),final_usage_observed_at=COALESCE(final_usage_observed_at,$4),terminal_at=$4,updated_at=$4,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=NULL WHERE id=$1 AND status IN ('UPSTREAM_ACTIVE','SETTLEMENT_PENDING','REVIEW') RETURNING wallet_id,token_account_id`, usageID, actual, encoded, now).Scan(&walletID, &tokenAccountID)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE usage_attempts SET status='SUCCEEDED',ended_at=$2 WHERE usage_request_id=$1 AND status IN ('STARTED','STREAMING')`, usageID, now)
	}
	if err == nil {
		err = transitionExecutionTx(ctx, tx, usageID, []string{"SETTLEMENT_PENDING", "REVIEW"}, "SUCCEEDED", "billing settlement committed", now)
	}
	if err == nil {
		err = addReplayTombstone(ctx, tx, usageID, "SUCCEEDED", now)
	}
	if err == nil {
		err = appendUsageConvergenceEvidenceTx(ctx, tx, usageID, "SETTLEMENT", "BILLING", "COMMIT", fmt.Sprintf("actual:%d", actual), map[string]any{"actual_microcredits": actual, "snapshot": snapshot}, now)
	}
	if err == nil {
		if err := events.AppendOutbox(ctx, tx, "token.usage.settled.v1", usageID, 1, map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "token_account_id": tokenAccountID, "actual_microcredits": actual}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var status string
	var storedActual *int64
	if err := tx.QueryRow(ctx, `SELECT status,actual_microcredits FROM usage_requests WHERE id=$1`, usageID).Scan(&status, &storedActual); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if status != "COMMITTED" || storedActual == nil || *storedActual != actual {
		return ErrForbidden
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) MarkReleased(ctx context.Context, usageID uuid.UUID, reason string) error {
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE usage_requests SET status='RELEASED',settlement_status='RELEASED',error_class=$2,terminal_at=$3,updated_at=$3,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=NULL WHERE id=$1 AND status IN ('CREATED','RESERVED','UPSTREAM_ACTIVE','SETTLEMENT_PENDING','REVIEW')`, usageID, strings.TrimSpace(reason), now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return r.ensureUsageState(ctx, usageID, "RELEASED")
	}
	_, err = tx.Exec(ctx, `UPDATE usage_attempts SET status='FAILED',error_class=$2,ended_at=$3 WHERE usage_request_id=$1 AND status IN ('STARTED','STREAMING')`, usageID, strings.TrimSpace(reason), now)
	if err != nil {
		return err
	}
	if err := transitionExecutionTx(ctx, tx, usageID, []string{"RECEIVED", "HOLD_RESERVING", "HOLD_RESERVED", "DISPATCH_INTENT_PERSISTED", "UPSTREAM_DISPATCHING", "STREAMING", "SETTLEMENT_PENDING", "REVIEW"}, "FAILED", strings.TrimSpace(reason), now); err != nil {
		return err
	}
	if err := addReplayTombstone(ctx, tx, usageID, "FAILED", now); err != nil {
		return err
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usageID, "CUSTOMER_HOLD_RELEASE", "BILLING", "RELEASE", strings.TrimSpace(reason), map[string]any{"reason": strings.TrimSpace(reason), "external_success_inferred": false}, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func getUsageByIdempotency(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, userID uuid.UUID, digest string, lock bool) (UsageRequest, error) {
	query := `SELECT ` + usageSelectColumns + ` FROM usage_requests WHERE user_id=$1 AND idempotency_namespace='model-inference' AND idempotency_key_digest=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	var usage UsageRequest
	err := scanUsage(database.QueryRow(ctx, query, userID, digest), &usage)
	return usage, err
}

func (r *PostgresRepository) FindUsageByIdempotency(ctx context.Context, userID uuid.UUID, digest string) (UsageRequest, error) {
	usage, err := getUsageByIdempotency(ctx, r.pool, userID, digest, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return UsageRequest{}, ErrNotFound
	}
	return usage, err
}

const usageTurnProjectionQuery = `WITH page_groups AS (
  SELECT organization_id,usage_group_key,logical_turn_id,head_started_at,head_request_id
  FROM usage_history_groups
  WHERE user_id=$1 AND organization_scope_id=$2
    AND ($3::timestamptz IS NULL OR (head_started_at,head_request_id)<($3,$4::uuid))
  ORDER BY head_started_at DESC,head_request_id DESC
  LIMIT $5
), members AS (
  SELECT page.organization_id AS history_organization_id,
         page.usage_group_key AS history_group_key,
         page.logical_turn_id AS history_logical_turn_id,
         page.head_started_at AS history_head_started_at,
         page.head_request_id AS history_head_request_id,
         request.id,
         request.model,
         request.model_snapshot,
         request.status,
         request.execution_status,
         request.delivery_status,
         request.cancel_status,
         request.settlement_status,
         request.actual_microcredits,
         request.usage_snapshot,
         request.started_at,
         request.terminal_at,
         request.updated_at,
         CASE WHEN COALESCE(request.usage_snapshot->'usage'->>'input_tokens','') ~ '^[0-9]+$'
              THEN (request.usage_snapshot->'usage'->>'input_tokens')::bigint END AS history_input_tokens,
         CASE WHEN COALESCE(request.usage_snapshot->'usage'->>'output_tokens','') ~ '^[0-9]+$'
              THEN (request.usage_snapshot->'usage'->>'output_tokens')::bigint END AS history_output_tokens,
         CASE WHEN COALESCE(request.usage_snapshot->'usage'->>'cached_input_tokens','') ~ '^[0-9]+$'
              THEN (request.usage_snapshot->'usage'->>'cached_input_tokens')::bigint END AS history_cached_input_tokens,
         request.delivery_status<>'USER_STOPPED'
           AND request.execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING')
           AND request.status NOT IN ('COMMITTED','RELEASED','REVIEW')
           AND request.settlement_status NOT IN ('SETTLED','RELEASED','REVIEW') AS history_cancellable,
         CASE
           WHEN request.cancel_status='LOCAL_CONFIRMED' THEN 'CANCELLED_BEFORE_DISPATCH'
           WHEN request.settlement_status='SETTLED' OR request.status='COMMITTED' THEN 'SETTLED'
           WHEN request.settlement_status='RELEASED' OR request.status='RELEASED' THEN 'RELEASED'
           WHEN request.status='REVIEW' OR request.execution_status='REVIEW' OR request.settlement_status='REVIEW' THEN 'MANUAL_REVIEW'
           WHEN request.delivery_status='USER_STOPPED' THEN 'DISPLAY_STOPPED_EXECUTION_CONTINUES'
           WHEN request.status IN ('CREATED','RESERVED','UPSTREAM_ACTIVE') THEN 'IN_PROGRESS'
           WHEN request.status='SETTLEMENT_PENDING' THEN 'SETTLEMENT_PENDING'
           ELSE 'MANUAL_REVIEW'
         END AS history_display_status
  FROM page_groups page
  JOIN usage_history_members member
    ON member.user_id=$1
   AND member.organization_scope_id=$2
   AND member.usage_group_key=page.usage_group_key
  JOIN usage_requests request ON request.id=member.usage_request_id
)
SELECT history_head_request_id,COALESCE(history_logical_turn_id,''),count(*) AS usage_request_count,
       (array_agg(id ORDER BY started_at DESC,id DESC) FILTER (WHERE history_cancellable))[1] AS cancel_usage_request_id,
       (array_agg(COALESCE(model_snapshot->>'display_name',model) ORDER BY started_at DESC,id DESC))[1] AS model,
       CASE
         WHEN bool_or(history_display_status='MANUAL_REVIEW') THEN 'MANUAL_REVIEW'
         WHEN bool_or(history_cancellable OR history_display_status='IN_PROGRESS') THEN 'IN_PROGRESS'
         WHEN bool_or(history_display_status='DISPLAY_STOPPED_EXECUTION_CONTINUES') THEN 'DISPLAY_STOPPED_EXECUTION_CONTINUES'
         WHEN bool_or(history_display_status='SETTLEMENT_PENDING') THEN 'SETTLEMENT_PENDING'
         WHEN bool_or(history_display_status='SETTLED') THEN 'SETTLED'
         WHEN bool_and(history_display_status='CANCELLED_BEFORE_DISPATCH') THEN 'CANCELLED_BEFORE_DISPATCH'
         WHEN bool_and(history_display_status IN ('RELEASED','CANCELLED_BEFORE_DISPATCH')) THEN 'RELEASED'
         ELSE 'MANUAL_REVIEW'
       END AS display_status,
       CASE WHEN count(*)=1 THEN max(execution_status) ELSE '' END AS execution_status,
       CASE WHEN count(*)=1 THEN max(delivery_status) ELSE '' END AS delivery_status,
       CASE WHEN count(*)=1 THEN max(cancel_status) ELSE '' END AS cancel_status,
       CASE WHEN count(*)=1 THEN max(settlement_status) ELSE '' END AS settlement_status,
       CASE WHEN bool_and(actual_microcredits IS NOT NULL) THEN sum(actual_microcredits) END AS actual_microcredits,
       CASE WHEN bool_and(
                  history_display_status IN ('SETTLED','RELEASED','CANCELLED_BEFORE_DISPATCH')
                  AND (history_display_status<>'SETTLED' OR actual_microcredits IS NOT NULL)
            ) THEN sum(CASE WHEN history_display_status='SETTLED' THEN actual_microcredits ELSE 0 END) END AS charged_microcredits,
       CASE WHEN bool_and(history_input_tokens IS NOT NULL OR history_display_status='CANCELLED_BEFORE_DISPATCH')
            THEN sum(COALESCE(history_input_tokens,0)) END AS input_tokens,
       CASE WHEN bool_and(history_output_tokens IS NOT NULL OR history_display_status='CANCELLED_BEFORE_DISPATCH')
            THEN sum(COALESCE(history_output_tokens,0)) END AS output_tokens,
       CASE WHEN bool_and(history_cached_input_tokens IS NOT NULL OR history_display_status='CANCELLED_BEFORE_DISPATCH')
            THEN sum(COALESCE(history_cached_input_tokens,0)) END AS cached_input_tokens,
       min(started_at) AS started_at,
       history_head_started_at AS cursor_started_at,
       CASE WHEN bool_or(history_display_status IN ('IN_PROGRESS','DISPLAY_STOPPED_EXECUTION_CONTINUES','SETTLEMENT_PENDING','MANUAL_REVIEW'))
            THEN NULL ELSE max(terminal_at) END AS completed_at,
       max(updated_at) AS updated_at
FROM members
GROUP BY history_organization_id,history_group_key,history_logical_turn_id,history_head_started_at,history_head_request_id
ORDER BY history_head_started_at DESC,history_head_request_id DESC`

func (r *PostgresRepository) RecordConvergenceEvidence(ctx context.Context, usageID uuid.UUID, evidenceType, sourceType, decision, evidenceKey string, snapshot map[string]any) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usageID, evidenceType, sourceType, decision, evidenceKey, snapshot, r.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
