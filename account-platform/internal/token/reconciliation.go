package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ReconciliationItem struct {
	UsageRequest
	UsageSnapshot     []byte
	RequestSnapshot   []byte
	ProviderID        *string
	AdapterVersion    *string
	UpstreamRequestID *string
	Attempts          int
}

type ReconciliationStore interface {
	RecoverLegacyResponseStateSplits(context.Context, int) (int64, error)
	ClaimReconciliation(context.Context, int, time.Duration) ([]ReconciliationItem, error)
	RescheduleReconciliation(context.Context, uuid.UUID, string, time.Time) error
	StopReconciliation(context.Context, uuid.UUID, string) error
	MarkCommitted(context.Context, uuid.UUID, int64, map[string]any) error
	MarkReleased(context.Context, uuid.UUID, string) error
	RecordConvergenceEvidence(context.Context, uuid.UUID, string, string, string, string, map[string]any) error
}

type ownedReconciliationStore interface {
	RescheduleReconciliationOwned(context.Context, uuid.UUID, int, string, time.Time) error
}

type ownedStopReconciliationStore interface {
	StopReconciliationOwned(context.Context, uuid.UUID, int, string) error
}

func legacyResponseStateSplit(request UsageRequest) bool {
	return request.Status == "UPSTREAM_ACTIVE" && request.ExecutionStatus == "SETTLEMENT_PENDING"
}

func (r *PostgresRepository) RecoverLegacyResponseStateSplits(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id,status,execution_status FROM usage_requests WHERE status='UPSTREAM_ACTIVE' AND execution_status='SETTLEMENT_PENDING' AND reconciliation_stopped_at IS NULL ORDER BY updated_at,id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	candidates := make([]UsageRequest, 0, limit)
	for rows.Next() {
		var candidate UsageRequest
		if err := rows.Scan(&candidate.ID, &candidate.Status, &candidate.ExecutionStatus); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	now := r.now()
	var recovered int64
	for _, candidate := range candidates {
		if !legacyResponseStateSplit(candidate) {
			continue
		}
		command, err := tx.Exec(ctx, `UPDATE usage_requests SET status='REVIEW',settlement_status='USAGE_PENDING',error_class=COALESCE(error_class,'legacy_response_state_split'),reconciliation_next_at=$2,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=NULL,updated_at=$2 WHERE id=$1 AND status='UPSTREAM_ACTIVE' AND execution_status='SETTLEMENT_PENDING'`, candidate.ID, now)
		if err != nil {
			return 0, err
		}
		if command.RowsAffected() == 0 {
			continue
		}
		if err := appendUsageConvergenceEvidenceTx(ctx, tx, candidate.ID, "UNCERTAINTY", "RECONCILER", "REVIEW", "execution:SETTLEMENT_PENDING/domain:UPSTREAM_ACTIVE", map[string]any{"automatic_reconciliation_enabled": true, "legacy_evidence_type": "LEGACY_RESPONSE_STATE_SPLIT", "recovery_action": "RECOVERED", "prior_status": candidate.Status, "execution_status": candidate.ExecutionStatus}, now); err != nil {
			return 0, err
		}
		recovered++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return recovered, nil
}

func (r *PostgresRepository) ClaimReconciliation(ctx context.Context, limit int, lease time.Duration) ([]ReconciliationItem, error) {
	if limit < 1 || limit > 100 || lease < 10*time.Second || lease > 10*time.Minute {
		return nil, ErrForbidden
	}
	now := r.now()
	rows, err := r.pool.Query(ctx, `
WITH candidates AS (
    SELECT request.id,attempt.provider_id,attempt.adapter_version,attempt.upstream_request_id
    FROM usage_requests request
    LEFT JOIN usage_attempts attempt ON attempt.usage_request_id = request.id
    WHERE request.status IN ('SETTLEMENT_PENDING','REVIEW')
      -- Cancellation owns recovery of a possibly committed pre-dispatch hold.
      -- It must retain the exact facts used by the existing release proof.
      AND request.error_class IS DISTINCT FROM 'cancel_hold_recovery_unconfirmed'
      AND request.reconciliation_stopped_at IS NULL
      AND (request.reconciliation_next_at IS NULL OR request.reconciliation_next_at <= $1)
      AND (request.reconciliation_lease_until IS NULL OR request.reconciliation_lease_until <= $1)
    ORDER BY COALESCE(request.reconciliation_next_at, request.updated_at), request.id
    FOR UPDATE OF request SKIP LOCKED
    LIMIT $2
)
UPDATE usage_requests request
SET reconciliation_attempts = request.reconciliation_attempts + 1,
    reconciliation_lease_until = $3,
    reconciliation_last_error = NULL,
    updated_at = $1
FROM candidates
WHERE request.id = candidates.id
RETURNING request.id,request.idempotency_key_digest,request.user_id,request.session_id,request.organization_id,
          request.token_account_id,request.credential_id,request.wallet_id,request.price_version_id,request.hold_id,
          request.model,request.status,request.maximum_microcredits,request.actual_microcredits,request.error_class,
		  request.started_at,request.updated_at,request.usage_snapshot,request.request_snapshot,
		  candidates.provider_id,candidates.adapter_version,candidates.upstream_request_id,request.reconciliation_attempts`, now, limit, now.Add(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ReconciliationItem, 0)
	for rows.Next() {
		var item ReconciliationItem
		if err := rows.Scan(&item.ID, &item.IdempotencyKeyDigest, &item.UserID, &item.SessionID, &item.OrganizationID,
			&item.TokenAccountID, &item.CredentialID, &item.WalletID, &item.PriceVersionID, &item.HoldID,
			&item.Model, &item.Status, &item.MaximumMicrocredits, &item.ActualMicrocredits, &item.ErrorClass,
			&item.StartedAt, &item.UpdatedAt, &item.UsageSnapshot, &item.RequestSnapshot,
			&item.ProviderID, &item.AdapterVersion, &item.UpstreamRequestID, &item.Attempts); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) RescheduleReconciliation(ctx context.Context, usageID uuid.UUID, lastError string, nextAt time.Time) error {
	return r.rescheduleReconciliation(ctx, usageID, 0, lastError, nextAt, false)
}

// RescheduleReconciliationOwned only writes a result for the worker attempt
// that claimed the row. A late worker becomes a no-op instead of clearing a
// newer lease.
func (r *PostgresRepository) RescheduleReconciliationOwned(ctx context.Context, usageID uuid.UUID, attempt int, lastError string, nextAt time.Time) error {
	if attempt < 1 {
		return ErrForbidden
	}
	return r.rescheduleReconciliation(ctx, usageID, attempt, lastError, nextAt, true)
}

func (r *PostgresRepository) rescheduleReconciliation(ctx context.Context, usageID uuid.UUID, attempt int, lastError string, nextAt time.Time, owned bool) error {
	query := `UPDATE usage_requests
SET reconciliation_next_at=$2,reconciliation_lease_until=NULL,reconciliation_last_error=$3,updated_at=$4
WHERE id=$1 AND status IN ('SETTLEMENT_PENDING','REVIEW') AND reconciliation_stopped_at IS NULL
  AND error_class IS DISTINCT FROM 'cancel_hold_recovery_unconfirmed'`
	args := []any{usageID, nextAt.UTC(), truncateReconciliationError(lastError), r.now()}
	if owned {
		query += ` AND reconciliation_attempts=$5`
		args = append(args, attempt)
	}
	command, err := r.pool.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		if owned {
			return nil
		}
		return r.ensureUsageState(ctx, usageID, "COMMITTED", "RELEASED")
	}
	return nil
}

func (r *PostgresRepository) StopReconciliation(ctx context.Context, usageID uuid.UUID, reason string) error {
	return r.stopReconciliation(ctx, usageID, 0, reason, false)
}

// StopReconciliationOwned only writes a stop decision for the worker attempt
// that claimed the row. A late worker becomes a no-op instead of clearing a
// newer lease or appending stale stop evidence.
func (r *PostgresRepository) StopReconciliationOwned(ctx context.Context, usageID uuid.UUID, attempt int, reason string) error {
	if attempt < 1 {
		return ErrForbidden
	}
	return r.stopReconciliation(ctx, usageID, attempt, reason, true)
}

func (r *PostgresRepository) stopReconciliation(ctx context.Context, usageID uuid.UUID, attempt int, reason string, owned bool) error {
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if pending, err := lockMissingReferenceReleaseTx(ctx, tx, usageID); err != nil {
		return err
	} else if pending {
		return tx.Commit(ctx)
	}
	changed, err := r.stopReconciliationTxOwned(ctx, tx, usageID, attempt, reason, now, owned)
	if err != nil {
		return err
	}
	if !changed {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
		if owned {
			return nil
		}
		return r.ensureUsageState(ctx, usageID, "COMMITTED", "RELEASED")
	}
	return tx.Commit(ctx)
}

// The captured finisher calls this only after verifying its exact immutable intent.
// Public StopReconciliation still respects the shared deadline decision fence.
func (r *PostgresRepository) stopReconciliationTx(ctx context.Context, tx pgx.Tx, usageID uuid.UUID, reason string, now time.Time) (bool, error) {
	return r.stopReconciliationTxOwned(ctx, tx, usageID, 0, reason, now, false)
}

func (r *PostgresRepository) stopReconciliationTxOwned(ctx context.Context, tx pgx.Tx, usageID uuid.UUID, attempt int, reason string, now time.Time, owned bool) (bool, error) {
	// UPDATE evaluates this ownership predicate under its usage-row lock. A
	// previously claimed item cannot overwrite a later cancellation recovery or
	// append RECONCILIATION_STOPPED evidence that invalidates its release proof.
	query := `UPDATE usage_requests
SET status='REVIEW',settlement_status='REVIEW',cancel_status=CASE WHEN cancel_status IN ('CANCEL_REQUESTED','PROVIDER_SENT','PROVIDER_CONFIRMED') THEN 'RESULT_UNKNOWN' ELSE cancel_status END,error_class='automatic_reconciliation_exhausted',reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=$2,reconciliation_stopped_at=$3,updated_at=$3
WHERE id=$1 AND status IN ('SETTLEMENT_PENDING','REVIEW')
	  AND error_class IS DISTINCT FROM 'cancel_hold_recovery_unconfirmed'`
	args := []any{usageID, truncateReconciliationError(reason), now}
	if owned {
		query += ` AND reconciliation_attempts=$4`
		args = append(args, attempt)
	}
	command, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() == 0 {
		return false, nil
	}
	if trustedUsageDeadlineExceeded(reason) {
		if _, err := tx.Exec(ctx, `INSERT INTO usage_platform_loss_reviews(id,usage_request_id,cause_code,provider_cost_evidence,created_at) VALUES($1,$2,'TRUSTED_USAGE_DEADLINE_EXCEEDED',jsonb_build_object('customer_hold_released',true,'late_usage_customer_debit_prohibited',true),$3) ON CONFLICT(usage_request_id) DO NOTHING`, uuid.Must(uuid.NewV7()), usageID, now); err != nil {
			return false, err
		}
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usageID, "RECONCILIATION_STOPPED", "RECONCILER", "REVIEW", truncateReconciliationError(reason), map[string]any{"reason_code": truncateReconciliationError(reason), "automatic_high_risk_retry": false}, now); err != nil {
		return false, err
	}
	return true, nil
}

func trustedUsageDeadlineExceeded(reason string) bool {
	return strings.HasPrefix(reason, "TRUSTED_USAGE_DEADLINE_EXCEEDED:")
}

func truncateReconciliationError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		return value[:500]
	}
	return value
}

type UsageEvidence struct {
	Usage    *Usage
	Rejected bool
}

type UsageEvidenceProvider interface {
	ProviderID() string
	AdapterVersion() string
	Lookup(context.Context, string) (UsageEvidence, error)
}

type ReconciliationBilling interface {
	PriceByID(context.Context, uuid.UUID) (PriceVersion, error)
	Commit(context.Context, uuid.UUID, uuid.UUID, int64, Usage) error
	Release(context.Context, uuid.UUID, uuid.UUID, string) error
}

type NewAPIUsageEvidenceClient struct {
	providerID     string
	adapterVersion string
	baseURL        *url.URL
	token          string
	userID         int64
	client         *http.Client
}

func NewNewAPIUsageEvidenceClient(rawURL, token string, userID int64, policy ProviderURLPolicy) (*NewAPIUsageEvidenceClient, error) {
	return NewNewAPIUsageEvidenceProviderWithCA("NEW_API", "new-api-v1", rawURL, token, userID, "", policy)
}

func NewNewAPIUsageEvidenceProvider(providerID, adapterVersion, rawURL, token string, userID int64, policy ProviderURLPolicy) (*NewAPIUsageEvidenceClient, error) {
	return NewNewAPIUsageEvidenceProviderWithCA(providerID, adapterVersion, rawURL, token, userID, "", policy)
}

func NewNewAPIUsageEvidenceProviderWithCA(providerID, adapterVersion, rawURL, token string, userID int64, caFile string, policy ProviderURLPolicy) (*NewAPIUsageEvidenceClient, error) {
	providerID = strings.ToUpper(strings.TrimSpace(providerID))
	adapterVersion = strings.TrimSpace(adapterVersion)
	rawURL = strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if err := policy.Validate(rawURL); err != nil || providerID == "" || adapterVersion == "" || strings.TrimSpace(token) == "" || userID <= 0 {
		return nil, errors.New("invalid New API reconciliation configuration")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("invalid New API reconciliation URL")
	}
	transport, err := newProviderHTTPTransport(caFile)
	if err != nil {
		return nil, fmt.Errorf("configure New API reconciliation trust: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: observability.NewHTTPTransport(transport)}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &NewAPIUsageEvidenceClient{providerID: providerID, adapterVersion: adapterVersion, baseURL: parsed, token: token, userID: userID, client: client}, nil
}

func (c *NewAPIUsageEvidenceClient) ProviderID() string { return c.providerID }

func (c *NewAPIUsageEvidenceClient) AdapterVersion() string { return c.adapterVersion }

func (c *NewAPIUsageEvidenceClient) Lookup(ctx context.Context, newAPIRequestID string) (UsageEvidence, error) {
	newAPIRequestID = strings.TrimSpace(newAPIRequestID)
	if newAPIRequestID == "" || len(newAPIRequestID) > 128 {
		return UsageEvidence{}, errors.New("invalid New API request identifier")
	}
	items, err := c.logs(ctx, newAPIRequestID, 2)
	if err != nil {
		return UsageEvidence{}, err
	}
	if len(items) > 1 {
		return UsageEvidence{}, errors.New("New API returned multiple consumption logs")
	}
	if len(items) == 1 {
		usage, err := items[0].usage()
		if err != nil {
			return UsageEvidence{}, err
		}
		return UsageEvidence{Usage: &usage}, nil
	}
	items, err = c.logs(ctx, newAPIRequestID, 5)
	if err != nil {
		return UsageEvidence{}, err
	}
	for _, item := range items {
		if item.conclusiveRejection() {
			return UsageEvidence{Rejected: true}, nil
		}
	}
	return UsageEvidence{}, nil
}

type newAPILog struct {
	RequestID        string          `json:"request_id"`
	PromptTokens     int64           `json:"prompt_tokens"`
	CompletionTokens int64           `json:"completion_tokens"`
	Content          string          `json:"content"`
	Other            json.RawMessage `json:"other"`
}

func (item newAPILog) conclusiveRejection() bool {
	if item.PromptTokens != 0 || item.CompletionTokens != 0 {
		return false
	}
	message := strings.ToLower(item.Content)
	for _, marker := range []string{"invalid_api_key", "invalid api key", "unauthorized", "model_not_found", "model not found", "insufficient_quota", "quota exceeded", "额度不足", "无效的令牌", "模型不存在"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (c *NewAPIUsageEvidenceClient) logs(ctx context.Context, newAPIRequestID string, logType int) ([]newAPILog, error) {
	query := url.Values{"p": {"1"}, "page_size": {"2"}, "type": {strconv.Itoa(logType)}, "request_id": {newAPIRequestID}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL.String()+"/api/log/?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("New-Api-User", strconv.FormatInt(c.userID, 10))
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2048))
		return nil, fmt.Errorf("New API log lookup HTTP %d", response.StatusCode)
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Items []newAPILog `json:"items"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&payload); err != nil || !payload.Success {
		return nil, errors.New("invalid New API log response")
	}
	matched := make([]newAPILog, 0, len(payload.Data.Items))
	for _, item := range payload.Data.Items {
		if item.RequestID == newAPIRequestID {
			matched = append(matched, item)
		}
	}
	return matched, nil
}

func (item newAPILog) usage() (Usage, error) {
	cached := int64(0)
	if len(item.Other) > 0 && string(item.Other) != "null" && string(item.Other) != `""` {
		var encoded string
		if json.Unmarshal(item.Other, &encoded) == nil {
			item.Other = json.RawMessage(encoded)
		}
		var other struct {
			CacheTokens  int64 `json:"cache_tokens"`
			CachedTokens int64 `json:"cached_tokens"`
		}
		if json.Unmarshal(item.Other, &other) != nil {
			return Usage{}, errors.New("New API returned invalid usage metadata")
		}
		cached = max(other.CacheTokens, other.CachedTokens)
	}
	if item.PromptTokens < 0 || item.CompletionTokens < 0 || cached < 0 || cached > item.PromptTokens {
		return Usage{}, errors.New("New API returned invalid token usage")
	}
	usage := Usage{InputTokens: item.PromptTokens - cached, OutputTokens: item.CompletionTokens, CachedInputTokens: cached}
	return usage, nil
}

type UsageReconciler struct {
	store     ReconciliationStore
	billing   ReconciliationBilling
	evidence  UsageEvidenceProvider
	interval  time.Duration
	lease     time.Duration
	maxAge    time.Duration
	now       func() time.Time
	logger    *slog.Logger
	batchSize int
}

func (r *UsageReconciler) reschedule(ctx context.Context, item ReconciliationItem, reason string, nextAt time.Time) error {
	if owned, ok := r.store.(ownedReconciliationStore); ok {
		return owned.RescheduleReconciliationOwned(ctx, item.ID, item.Attempts, reason, nextAt)
	}
	return r.store.RescheduleReconciliation(ctx, item.ID, reason, nextAt)
}

func (r *UsageReconciler) stop(ctx context.Context, item ReconciliationItem, reason string) error {
	if owned, ok := r.store.(ownedStopReconciliationStore); ok {
		return owned.StopReconciliationOwned(ctx, item.ID, item.Attempts, reason)
	}
	return r.store.StopReconciliation(ctx, item.ID, reason)
}

var errManualReconciliationRequired = errors.New("manual reconciliation required")

func NewUsageReconciler(store ReconciliationStore, billing ReconciliationBilling, evidence UsageEvidenceProvider, interval time.Duration) *UsageReconciler {
	if interval < time.Second {
		interval = 15 * time.Second
	}
	return &UsageReconciler{store: store, billing: billing, evidence: evidence, interval: interval, lease: 45 * time.Second, maxAge: 24 * time.Hour, now: func() time.Time { return time.Now().UTC() }, logger: slog.Default().With("component", "usage-reconciler"), batchSize: 50}
}

func (r *UsageReconciler) Run(ctx context.Context) {
	r.runOnce(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

func (r *UsageReconciler) runOnce(ctx context.Context) {
	if store, ok := r.store.(missingReferenceDeadlineStore); ok {
		if err := store.RecoverStoppedMissingReferences(ctx, r.batchSize); err != nil {
			r.logger.Error("missing-reference recovery failed", "error", err)
		}
	}
	if _, err := r.store.RecoverLegacyResponseStateSplits(ctx, r.batchSize); err != nil {
		r.logger.Error("legacy response-state recovery failed", "error", err)
	}
	items, err := r.store.ClaimReconciliation(ctx, r.batchSize, r.lease)
	if err != nil {
		r.logger.Error("claim failed", "error", err)
		return
	}
	for _, item := range items {
		if handled, err := r.reconcileCapturedReferenceDeadline(ctx, item, "", false); handled {
			if err != nil {
				r.logger.Error("captured-reference recovery remains manual", "usage_request_id", item.ID, "error", err)
			}
			continue
		}
		if handled, err := r.reconcileMissingReferenceDeadline(ctx, item); handled {
			if err != nil {
				r.logger.Warn("missing-reference release pending", "usage_request_id", item.ID, "error", err)
				if writeErr := r.reschedule(ctx, item, "MISSING_UPSTREAM_REFERENCE_RELEASE_UNCONFIRMED", r.now().Add(reconciliationBackoff(item.Attempts))); writeErr != nil {
					r.logger.Error("missing-reference retry persistence failed", "usage_request_id", item.ID, "error", writeErr)
				}
			}
			continue
		}
		if handled, err := r.reconcileUsageWaiver(ctx, item); handled {
			if err != nil {
				_ = r.reschedule(ctx, item, "ADMIN_WAIVER_RESULT_UNKNOWN", r.now().Add(reconciliationBackoff(item.Attempts)))
			}
			continue
		}
		if err := r.reconcile(ctx, item); err != nil {
			r.logger.Warn("request reconciliation deferred", "usage_request_id", item.ID, "attempt", item.Attempts, "error", err)
			errorCode := reconciliationErrorCode(err)
			if errors.Is(err, errManualReconciliationRequired) {
				_ = r.stop(ctx, item, errorCode)
				continue
			}
			if r.now().Sub(item.StartedAt) >= r.maxAge {
				if item.HoldID != nil {
					if handled, writeErr := r.reconcileCapturedReferenceDeadline(ctx, item, errorCode, true); handled {
						if writeErr != nil {
							r.logger.Error("captured-reference deadline remains manual", "usage_request_id", item.ID, "error", writeErr)
						}
						continue
					}
					// Stores without the durable decision contract cannot authorize a release.
					if writeErr := r.stop(ctx, item, "HOLD_RELEASE_UNAVAILABLE:"+errorCode); writeErr != nil {
						r.logger.Error("manual stop failed", "error", writeErr)
					}
				} else if writeErr := r.stop(ctx, item, "MANUAL_RECONCILIATION_REQUIRED"); writeErr != nil {
					r.logger.Error("manual stop failed", "error", writeErr)
				}
				continue
			}
			_ = r.reschedule(ctx, item, errorCode, r.now().Add(reconciliationBackoff(item.Attempts)))
		}
	}
}

func reconciliationErrorCode(err error) string {
	switch {
	case errors.Is(err, errManualReconciliationRequired):
		return "MANUAL_RECONCILIATION_REQUIRED"
	case errors.Is(err, context.DeadlineExceeded):
		return "USAGE_EVIDENCE_TIMEOUT"
	default:
		return "USAGE_EVIDENCE_UNAVAILABLE"
	}
}

func (r *UsageReconciler) reconcile(ctx context.Context, item ReconciliationItem) error {
	if item.HoldID == nil {
		return fmt.Errorf("%w: usage request is missing hold", errManualReconciliationRequired)
	}
	var observation replayResponseObservation
	if json.Unmarshal(item.UsageSnapshot, &observation) == nil && observation.ResponseDisposition == responseDispositionRejectedBeforeGeneration {
		if err := r.store.RecordConvergenceEvidence(ctx, item.ID, "RESULT_OBSERVED", "GATEWAY", "RECORDED", fmt.Sprintf("http:%d", observation.HTTPStatus), map[string]any{"response_disposition": observation.ResponseDisposition, "http_status": observation.HTTPStatus}); err != nil {
			return err
		}
		if err := r.billing.Release(ctx, item.ID, *item.HoldID, "upstream rejected before generation"); err != nil {
			return err
		}
		return r.store.MarkReleased(ctx, item.ID, "reconciled_upstream_rejected_before_generation")
	}
	if item.PriceVersionID == nil {
		return fmt.Errorf("%w: usage request is missing price version", errManualReconciliationRequired)
	}
	if item.Status == "SETTLEMENT_PENDING" {
		var snapshot struct {
			Usage              *Usage `json:"usage"`
			ActualMicrocredits *int64 `json:"actual_microcredits"`
		}
		if err := json.Unmarshal(item.UsageSnapshot, &snapshot); err != nil || snapshot.Usage == nil || snapshot.ActualMicrocredits == nil {
			return fmt.Errorf("%w: settlement snapshot is invalid", errManualReconciliationRequired)
		}
		return r.commit(ctx, item, *snapshot.Usage, snapshot.ActualMicrocredits)
	}
	if item.ProviderID == nil || item.AdapterVersion == nil || item.UpstreamRequestID == nil || strings.TrimSpace(*item.UpstreamRequestID) == "" {
		return fmt.Errorf("%w: upstream request evidence was not captured", errManualReconciliationRequired)
	}
	if r.evidence == nil {
		return errors.New("usage evidence provider is unavailable")
	}
	if !strings.EqualFold(*item.ProviderID, r.evidence.ProviderID()) || *item.AdapterVersion != r.evidence.AdapterVersion() {
		return fmt.Errorf("%w: no evidence adapter matches historical provider %s/%s", errManualReconciliationRequired, *item.ProviderID, *item.AdapterVersion)
	}
	evidence, err := r.evidence.Lookup(ctx, *item.UpstreamRequestID)
	if err != nil {
		if recordErr := r.store.RecordConvergenceEvidence(ctx, item.ID, "PROVIDER_LOOKUP", "PROVIDER_QUERY", "REVIEW", digestString([]byte(*item.UpstreamRequestID)), map[string]any{"lookup_succeeded": false, "usage_present": false, "rejected": false}); recordErr != nil {
			return recordErr
		}
		return err
	}
	decision := "RECORDED"
	if evidence.Usage == nil && !evidence.Rejected {
		decision = "REVIEW"
	}
	if err := r.store.RecordConvergenceEvidence(ctx, item.ID, "PROVIDER_LOOKUP", "PROVIDER_QUERY", decision, digestString([]byte(*item.UpstreamRequestID)), map[string]any{"lookup_succeeded": true, "usage_present": evidence.Usage != nil, "rejected": evidence.Rejected, "usage": evidence.Usage}); err != nil {
		return err
	}
	if evidence.Usage != nil {
		return r.commit(ctx, item, *evidence.Usage, nil)
	}
	if evidence.Rejected {
		if err := r.billing.Release(ctx, item.ID, *item.HoldID, "provider evidence confirmed no consumption"); err != nil {
			return err
		}
		return r.store.MarkReleased(ctx, item.ID, "reconciled_upstream_rejected")
	}
	return errors.New("provider has no conclusive usage or rejection evidence yet")
}

func (r *UsageReconciler) commit(ctx context.Context, item ReconciliationItem, usage Usage, expectedActual *int64) error {
	var snapshot struct {
		PricingSnapshot *PriceVersion `json:"pricing_snapshot"`
	}
	_ = json.Unmarshal(item.RequestSnapshot, &snapshot)
	var price PriceVersion
	var err error
	if snapshot.PricingSnapshot != nil {
		price = *snapshot.PricingSnapshot
	} else {
		price, err = r.billing.PriceByID(ctx, *item.PriceVersionID)
		if err != nil {
			return err
		}
	}
	if price.ID != *item.PriceVersionID || price.Model != item.Model {
		return fmt.Errorf("%w: historical price does not match usage request", errManualReconciliationRequired)
	}
	actual, err := Cost(price, usage)
	if err != nil {
		return fmt.Errorf("%w: reconciled usage is invalid", errManualReconciliationRequired)
	}
	if expectedActual != nil && actual != *expectedActual {
		return fmt.Errorf("%w: settlement snapshot does not match historical pricing", errManualReconciliationRequired)
	}
	if store, ok := r.store.(missingReferenceDeadlineStore); ok {
		if fenced, err := store.PreserveMissingReferenceLateUsage(ctx, item.ID, usage); err != nil {
			return err
		} else if fenced {
			return nil
		}
	}
	if store, ok := r.store.(usageWaiverEvidenceStore); ok {
		if fenced, err := store.PreserveUsageWaiverEvidence(ctx, item.ID, usage); err != nil {
			return err
		} else if fenced {
			return nil
		}
	}
	if err := r.billing.Commit(ctx, item.ID, *item.HoldID, actual, usage); err != nil {
		if definitiveSettlementConflict(err) {
			return fmt.Errorf("%w: billing commit was definitively rejected", errManualReconciliationRequired)
		}
		return err
	}
	settlementSnapshot := map[string]any{"usage": usage, "actual_microcredits": actual, "reconciled": true}
	var previous struct {
		BillingNotice         *billingNotice  `json:"billing_notice"`
		AdditionalReservation json.RawMessage `json:"additional_reservation"`
	}
	if json.Unmarshal(item.UsageSnapshot, &previous) == nil {
		if previous.BillingNotice != nil {
			settlementSnapshot["billing_notice"] = previous.BillingNotice
		}
		// Keep the original ACK evidence when MarkCommitted replaces both
		// durable snapshots. This is evidence only, never a pricing input.
		if len(previous.AdditionalReservation) > 0 && string(previous.AdditionalReservation) != "null" {
			settlementSnapshot["additional_reservation"] = previous.AdditionalReservation
		}
	}
	return r.store.MarkCommitted(ctx, item.ID, actual, settlementSnapshot)
}

func reconciliationBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 5 * time.Second
	for i := 1; i < attempt && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

var _ ReconciliationStore = (*PostgresRepository)(nil)
