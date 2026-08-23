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
	ClaimReconciliation(context.Context, int, time.Duration) ([]ReconciliationItem, error)
	RescheduleReconciliation(context.Context, uuid.UUID, string, time.Time) error
	StopReconciliation(context.Context, uuid.UUID, string) error
	MarkCommitted(context.Context, uuid.UUID, int64, map[string]any) error
	MarkReleased(context.Context, uuid.UUID, string) error
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
	command, err := r.pool.Exec(ctx, `UPDATE usage_requests
SET reconciliation_next_at=$2,reconciliation_lease_until=NULL,reconciliation_last_error=$3,updated_at=$4
WHERE id=$1 AND status IN ('SETTLEMENT_PENDING','REVIEW') AND reconciliation_stopped_at IS NULL`, usageID, nextAt.UTC(), truncateReconciliationError(lastError), r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return r.ensureUsageState(ctx, usageID, "COMMITTED", "RELEASED")
	}
	return nil
}

func (r *PostgresRepository) StopReconciliation(ctx context.Context, usageID uuid.UUID, reason string) error {
	now := r.now()
	command, err := r.pool.Exec(ctx, `UPDATE usage_requests
SET status='REVIEW',error_class='automatic_reconciliation_exhausted',reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=$2,reconciliation_stopped_at=$3,updated_at=$3
WHERE id=$1 AND status IN ('SETTLEMENT_PENDING','REVIEW')`, usageID, truncateReconciliationError(reason), now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return r.ensureUsageState(ctx, usageID, "COMMITTED", "RELEASED")
	}
	return nil
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
	return NewNewAPIUsageEvidenceProvider("NEW_API", "new-api-v1", rawURL, token, userID, policy)
}

func NewNewAPIUsageEvidenceProvider(providerID, adapterVersion, rawURL, token string, userID int64, policy ProviderURLPolicy) (*NewAPIUsageEvidenceClient, error) {
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
	client := observability.NewHTTPClient(10 * time.Second)
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
	items, err := r.store.ClaimReconciliation(ctx, r.batchSize, r.lease)
	if err != nil {
		r.logger.Error("claim failed", "error", err)
		return
	}
	for _, item := range items {
		if err := r.reconcile(ctx, item); err != nil {
			r.logger.Warn("request reconciliation deferred", "usage_request_id", item.ID, "attempt", item.Attempts, "error", err)
			if errors.Is(err, errManualReconciliationRequired) || r.now().Sub(item.StartedAt) >= r.maxAge {
				_ = r.store.StopReconciliation(ctx, item.ID, "automatic reconciliation exhausted: "+err.Error())
				continue
			}
			_ = r.store.RescheduleReconciliation(ctx, item.ID, err.Error(), r.now().Add(reconciliationBackoff(item.Attempts)))
		}
	}
}

func (r *UsageReconciler) reconcile(ctx context.Context, item ReconciliationItem) error {
	if item.HoldID == nil || item.PriceVersionID == nil {
		return fmt.Errorf("%w: usage request is missing hold or price version", errManualReconciliationRequired)
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
	if r.evidence == nil {
		return errors.New("usage evidence provider is unavailable")
	}
	if item.ProviderID == nil || item.AdapterVersion == nil || item.UpstreamRequestID == nil || strings.TrimSpace(*item.UpstreamRequestID) == "" {
		return fmt.Errorf("%w: upstream request evidence was not captured", errManualReconciliationRequired)
	}
	if !strings.EqualFold(*item.ProviderID, r.evidence.ProviderID()) || *item.AdapterVersion != r.evidence.AdapterVersion() {
		return fmt.Errorf("%w: no evidence adapter matches historical provider %s/%s", errManualReconciliationRequired, *item.ProviderID, *item.AdapterVersion)
	}
	evidence, err := r.evidence.Lookup(ctx, *item.UpstreamRequestID)
	if err != nil {
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
	if err != nil || actual > item.MaximumMicrocredits {
		return fmt.Errorf("%w: reconciled usage exceeds the reserved amount", errManualReconciliationRequired)
	}
	if expectedActual != nil && actual != *expectedActual {
		return fmt.Errorf("%w: settlement snapshot does not match historical pricing", errManualReconciliationRequired)
	}
	if err := r.billing.Commit(ctx, item.ID, *item.HoldID, actual, usage); err != nil {
		return err
	}
	return r.store.MarkCommitted(ctx, item.ID, actual, map[string]any{"usage": usage, "reconciled": true})
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
