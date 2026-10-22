package token

// This policy owns only acquired personal calls whose upstream reference was
// never captured. It does not turn a generic REVIEW label into release proof.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const missingReferenceDeadlineCause = "MISSING_UPSTREAM_REFERENCE_DEADLINE_EXCEEDED"
const missingReferenceReleaseKey = "missing-upstream-reference-deadline:v1"

type missingReferenceRelease struct {
	Version              int       `json:"version"`
	UsageID              uuid.UUID `json:"usage_request_id"`
	UserID               uuid.UUID `json:"user_id"`
	HoldID               uuid.UUID `json:"hold_id"`
	WalletID             uuid.UUID `json:"wallet_id"`
	PriceID              uuid.UUID `json:"price_version_id"`
	Maximum              int64     `json:"maximum_microcredits"`
	StartedAt            time.Time `json:"started_at"`
	RequestFingerprint   string    `json:"request_fingerprint"`
	SnapshotDigest       string    `json:"request_snapshot_digest"`
	DispatchIntegrity    string    `json:"dispatch_intent_integrity"`
	AcquiredAt           time.Time `json:"worker_acquired_at"`
	Owner                string    `json:"worker_lease_owner"`
	Cause                string    `json:"cause_code"`
	ReleasePending       bool      `json:"release_pending"`
	CustomerHoldReleased bool      `json:"customer_hold_released"`
}

// The snapshot is retained only locally for the checked Billing request. The
// evidence stores its digest and the original identity, never credentials or
// response content. All acknowledgement retries use this same usage/hold pair.
type missingReferenceReleaseRequest struct {
	Intent  missingReferenceRelease
	Pricing HoldPricingContext
}

type missingReferenceDeadlineStore interface {
	RecoverStoppedMissingReferences(context.Context, int) error
	PrepareMissingReferenceRelease(context.Context, uuid.UUID) (*missingReferenceReleaseRequest, bool, error)
	FinishMissingReferenceRelease(context.Context, missingReferenceReleaseRequest, confirmedReleaseReceipt) error
	PauseMissingReferenceRelease(context.Context, missingReferenceReleaseRequest, error) error
	PreserveMissingReferenceLateUsage(context.Context, uuid.UUID, Usage) (bool, error)
}

type confirmedReleaseReceipt struct {
	ID       uuid.UUID  `json:"hold_id"`
	UsageID  uuid.UUID  `json:"usage_request_id"`
	WalletID uuid.UUID  `json:"wallet_id"`
	PriceID  *uuid.UUID `json:"price_version_id"`
	Maximum  int64      `json:"max_microcredits"`
	Status   string     `json:"status"`
}

type missingReferenceDeadlineBilling interface {
	ReleaseMissingReference(context.Context, missingReferenceReleaseRequest) (confirmedReleaseReceipt, error)
}

func (value missingReferenceRelease) snapshot() map[string]any {
	return map[string]any{"version": value.Version, "usage_request_id": value.UsageID, "user_id": value.UserID,
		"hold_id": value.HoldID, "wallet_id": value.WalletID, "price_version_id": value.PriceID,
		"maximum_microcredits": value.Maximum, "started_at": value.StartedAt,
		"request_fingerprint": value.RequestFingerprint, "request_snapshot_digest": value.SnapshotDigest,
		"dispatch_intent_integrity": value.DispatchIntegrity, "worker_acquired_at": value.AcquiredAt,
		"worker_lease_owner": value.Owner, "cause_code": value.Cause,
		"release_pending": value.ReleasePending, "customer_hold_released": value.CustomerHoldReleased}
}

// The caller must hold the usage row. Every publisher of a replacement state
// checks this same immutable record under that row lock, rather than trusting
// an error_class string which MarkReview/admin retries may overwrite.
func missingReferenceReleasePendingTx(ctx context.Context, tx pgx.Tx, usageID uuid.UUID) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_convergence_evidence
 WHERE usage_request_id=$1 AND evidence_type='CUSTOMER_HOLD_RELEASE'
 AND source_type='RECONCILER' AND decision='REVIEW' AND evidence_key=$2)`, usageID, missingReferenceReleaseKey).Scan(&pending)
	return pending, err
}

func lockMissingReferenceReleaseTx(ctx context.Context, tx pgx.Tx, usageID uuid.UUID) (bool, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&id); err != nil {
		return false, err
	}
	return missingReferenceReleasePendingTx(ctx, tx, usageID)
}

func missingReferenceFrozenPricing(usage UsageRequest) (HoldPricingContext, error) {
	if usage.OrganizationID != nil || usage.CanonicalVersion != 1 || usage.IdempotencyNamespace != "model-inference" ||
		usage.UserID == uuid.Nil || usage.WalletID == uuid.Nil || usage.TokenAccountID == uuid.Nil || usage.CredentialID == uuid.Nil ||
		usage.HoldID == nil || *usage.HoldID == uuid.Nil || usage.PriceVersionID == nil || *usage.PriceVersionID == uuid.Nil ||
		usage.ModelID == nil || *usage.ModelID == uuid.Nil || usage.Model != usage.ModelID.String() ||
		usage.CapabilityVersionID == nil || *usage.CapabilityVersionID == uuid.Nil || usage.ProtocolDefinitionID == nil || *usage.ProtocolDefinitionID == uuid.Nil ||
		usage.CatalogVersion == nil || *usage.CatalogVersion <= 0 || usage.RouteGeneration == nil || *usage.RouteGeneration <= 0 ||
		usage.SessionID == "" || !expiryDigest(usage.IdempotencyKeyDigest) || !expiryDigest(usage.RequestFingerprint) || usage.MaximumMicrocredits < 0 {
		return HoldPricingContext{}, ErrForbidden
	}
	// JSON missing/null fields cannot borrow Go zero values as proof.
	if !expiryJSONFields(usage.RequestSnapshot, []string{"request_sha256", "model_id", "upstream_model_ref", "catalog_version", "model_capability_version_id", "protocol_definition_id", "route_generation", "protocol_code", "protocol_version", "stream", "estimated_input_tokens", "maximum_output_tokens", "estimator", "context_type", "can_purchase_organization_entitlements", "wallet_id", "token_account_id", "credential_id", "formula_version_id", "price_version_id", "pricing_snapshot", "provider_id", "adapter_version", "wallet_owner_type"}) {
		return HoldPricingContext{}, ErrForbidden
	}
	// The personal producer records organization_id explicitly as null. A
	// missing field must not borrow the pointer's Go zero value as proof.
	var fields map[string]json.RawMessage
	if json.Unmarshal(usage.RequestSnapshot, &fields) != nil || strings.TrimSpace(string(fields["organization_id"])) != "null" {
		return HoldPricingContext{}, ErrForbidden
	}
	var snapshot struct {
		RequestSHA      string       `json:"request_sha256"`
		ModelID         uuid.UUID    `json:"model_id"`
		UpstreamModel   string       `json:"upstream_model_ref"`
		CatalogVersion  int64        `json:"catalog_version"`
		CapabilityID    uuid.UUID    `json:"model_capability_version_id"`
		ProtocolID      uuid.UUID    `json:"protocol_definition_id"`
		RouteGeneration int64        `json:"route_generation"`
		Protocol        string       `json:"protocol_code"`
		ProtocolVersion int          `json:"protocol_version"`
		Context         string       `json:"context_type"`
		OrganizationID  *uuid.UUID   `json:"organization_id"`
		WalletID        uuid.UUID    `json:"wallet_id"`
		TokenAccountID  uuid.UUID    `json:"token_account_id"`
		CredentialID    uuid.UUID    `json:"credential_id"`
		PriceID         uuid.UUID    `json:"price_version_id"`
		FormulaID       uuid.UUID    `json:"formula_version_id"`
		Price           PriceVersion `json:"pricing_snapshot"`
		EstimatedInput  int64        `json:"estimated_input_tokens"`
		MaximumOutput   int64        `json:"maximum_output_tokens"`
		Estimator       struct {
			Code    string `json:"code"`
			Version int    `json:"version"`
		} `json:"estimator"`
		Provider    string `json:"provider_id"`
		Adapter     string `json:"adapter_version"`
		WalletOwner string `json:"wallet_owner_type"`
	}
	if json.Unmarshal(usage.RequestSnapshot, &snapshot) != nil || !expiryDigest(snapshot.RequestSHA) ||
		snapshot.ModelID != *usage.ModelID || snapshot.CatalogVersion != *usage.CatalogVersion || snapshot.CapabilityID != *usage.CapabilityVersionID ||
		snapshot.ProtocolID != *usage.ProtocolDefinitionID || snapshot.RouteGeneration != *usage.RouteGeneration ||
		snapshot.Context != "PERSONAL" || snapshot.OrganizationID != nil || snapshot.Protocol != "CHAT" || snapshot.ProtocolVersion != 1 ||
		snapshot.WalletID != usage.WalletID || snapshot.TokenAccountID != usage.TokenAccountID || snapshot.CredentialID != usage.CredentialID ||
		snapshot.PriceID != *usage.PriceVersionID || snapshot.Price.ID != snapshot.PriceID || snapshot.Price.Model != usage.Model ||
		snapshot.FormulaID != snapshot.Price.FormulaVersionID || snapshot.FormulaID == uuid.Nil ||
		snapshot.WalletOwner != "USER" || strings.TrimSpace(snapshot.Provider) == "" || strings.TrimSpace(snapshot.Adapter) == "" ||
		strings.TrimSpace(snapshot.UpstreamModel) == "" || snapshot.Estimator.Code != "UTF8_BYTES_DIV4_CEIL" || snapshot.Estimator.Version != 1 ||
		snapshot.EstimatedInput < 0 || snapshot.MaximumOutput <= 0 || ValidatePriceFormula(snapshot.Price) != nil {
		return HoldPricingContext{}, ErrForbidden
	}
	price, err := json.Marshal(snapshot.Price)
	if err != nil {
		return HoldPricingContext{}, err
	}
	if !expiryJSONFields(price, []string{"price_version_id", "model", "input_per_million", "output_per_million", "cached_input_per_million", "scope_type", "protocol_code", "formula_version_id", "formula_snapshot", "credit_unit", "effective_from", "resolved_at", "units"}) {
		return HoldPricingContext{}, ErrForbidden
	}
	maximum, err := Cost(snapshot.Price, Usage{InputTokens: snapshot.EstimatedInput, OutputTokens: snapshot.MaximumOutput})
	if err != nil || maximum != usage.MaximumMicrocredits {
		return HoldPricingContext{}, ErrForbidden
	}
	return HoldPricingContext{ModelID: usage.Model, ProtocolCode: "CHAT", FormulaVersionID: snapshot.FormulaID, PricingSnapshot: price, RequestSnapshot: append([]byte(nil), usage.RequestSnapshot...)}, nil
}

// This deliberately does not decode an envelope: AcquireDispatch destroyed it
// after committing DISPATCH_STARTED. Its retained immutable integrity/owner/
// acquired-time record proves acquisition, not non-dispatch or provider success.
func missingReferenceEligibilityTx(ctx context.Context, tx pgx.Tx, usage UsageRequest, now time.Time) (missingReferenceReleaseRequest, bool, error) {
	pricing, err := missingReferenceFrozenPricing(usage)
	if err != nil {
		return missingReferenceReleaseRequest{}, false, err
	}
	if usage.Status != "REVIEW" || (usage.ExecutionStatus != "REVIEW" && usage.ExecutionStatus != "SETTLEMENT_PENDING") || usage.ActualMicrocredits != nil ||
		usage.FinalUsageObservedAt != nil || usage.StartedAt.IsZero() || usage.StartedAt.After(now) ||
		(usage.CancelStatus != "NOT_REQUESTED" && usage.CancelStatus != "CANCEL_REQUESTED" && usage.CancelStatus != "UNSUPPORTED" && usage.CancelStatus != "RESULT_UNKNOWN") {
		return missingReferenceReleaseRequest{}, false, ErrForbidden
	}
	var acquired, leaseUntil *time.Time
	var owner *string
	var upstreamKey *string
	var idempotent bool
	err = tx.QueryRow(ctx, `SELECT worker_acquired_at,worker_lease_until,worker_lease_owner,upstream_idempotency_key,adapter_supports_idempotency
 FROM usage_dispatch_intents WHERE usage_request_id=$1`, usage.ID).Scan(&acquired, &leaseUntil, &owner, &upstreamKey, &idempotent)
	if errors.Is(err, pgx.ErrNoRows) {
		return missingReferenceReleaseRequest{}, false, ErrForbidden
	}
	if err != nil {
		return missingReferenceReleaseRequest{}, false, err
	}
	if acquired == nil || leaseUntil == nil || owner == nil || strings.TrimSpace(*owner) == "" || upstreamKey == nil ||
		*upstreamKey != "pinvou-"+usage.ID.String() || idempotent || acquired.Before(usage.StartedAt) || leaseUntil.After(now) {
		return missingReferenceReleaseRequest{}, false, ErrForbidden
	}
	var maxExecution int
	var blocked, safeCause bool
	err = tx.QueryRow(ctx, `SELECT maximum_execution_seconds,
 trusted_usage_snapshot IS NOT NULL OR trusted_usage_source IS NOT NULL OR provider_cancel_confirmed_at IS NOT NULL OR provider_cancel_sent_at IS NOT NULL
 OR EXISTS(SELECT 1 FROM usage_attempts WHERE usage_request_id=$1)
 OR EXISTS(SELECT 1 FROM usage_platform_loss_reviews WHERE usage_request_id=$1)
 OR EXISTS(SELECT 1 FROM usage_replay_results WHERE usage_request_id=$1 AND final_usage IS NOT NULL)
 OR EXISTS(SELECT 1 FROM usage_convergence_evidence WHERE usage_request_id=$1 AND (
  evidence_type IN ('UPSTREAM_REFERENCE','SETTLEMENT','CUSTOMER_HOLD_RELEASE','PROVIDER_LOOKUP')
  OR (evidence_type='UNCERTAINTY' AND evidence_key NOT IN ('upstream_transport_unknown','dispatch_result_unknown','upstream_body_incomplete','upstream_error_unknown_usage','stream_usage_missing','stream_terminal_missing','usage_missing'))
  OR (evidence_type='UNCERTAINTY' AND (sanitized_snapshot->'sanitized_observation'->'usage' IS NOT NULL
   OR sanitized_snapshot->'sanitized_observation'->'actual_microcredits' IS NOT NULL
   OR sanitized_snapshot->'sanitized_observation'->>'usage_found'='true'))
  OR (evidence_type='RESULT_OBSERVED' AND (decision<>'RECORDED' OR sanitized_snapshot->'usage' IS NOT NULL
   OR sanitized_snapshot->>'final_usage_present'='true' OR sanitized_snapshot->>'settlement_snapshot_present'='true'
   OR sanitized_snapshot->>'response_disposition' IN ('TRUSTED_USAGE','REJECTED_BEFORE_GENERATION'))))) AS blocked,
 COALESCE(EXISTS(SELECT 1 FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='UNCERTAINTY' AND source_type='GATEWAY' AND decision='REVIEW'
 AND evidence_key IN ('upstream_transport_unknown','dispatch_result_unknown','upstream_body_incomplete','upstream_error_unknown_usage','stream_usage_missing','stream_terminal_missing','usage_missing'))
 OR (usage_snapshot->>'response_disposition'='USAGE_EVIDENCE_REQUIRED' AND EXISTS(
 SELECT 1 FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='RESULT_OBSERVED' AND source_type='GATEWAY' AND decision='RECORDED'
 AND sanitized_snapshot->>'final_usage_present'='false' AND sanitized_snapshot->>'settlement_snapshot_present'='false'
 AND sanitized_snapshot->>'response_payload_digest' ~ '^[0-9a-f]{64}$'
 AND sanitized_snapshot->>'http_status' ~ '^[1-5][0-9]{2}$')),false)
 FROM usage_requests WHERE id=$1`, usage.ID).Scan(&maxExecution, &blocked, &safeCause)
	if err != nil {
		return missingReferenceReleaseRequest{}, false, err
	}
	// Hard server execution deadline, not the old ten-minute lease alone.
	if blocked || !safeCause || maxExecution != 7200 || acquired.Add(2*time.Hour).After(now) {
		return missingReferenceReleaseRequest{}, false, ErrForbidden
	}
	var integrity string
	var count int
	err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(min(evidence_key),'') FROM usage_convergence_evidence
 WHERE usage_request_id=$1 AND evidence_type='DISPATCH_STARTED' AND source_type='GATEWAY' AND decision='RECORDED'
 AND observed_at=$2 AND sanitized_snapshot->>'worker_lease_owner'=$3
 AND sanitized_snapshot->>'automatic_redispatch'='false'
 AND sanitized_snapshot->>'dispatch_intent_integrity'=evidence_key
 AND (SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='DISPATCH_STARTED')=1`, usage.ID, acquired, *owner).Scan(&count, &integrity)
	if err != nil {
		return missingReferenceReleaseRequest{}, false, err
	}
	if count != 1 || !expiryDigest(integrity) {
		return missingReferenceReleaseRequest{}, false, ErrForbidden
	}
	var observation map[string]json.RawMessage
	if len(usage.UsageSnapshot) > 0 && string(usage.UsageSnapshot) != "null" {
		if json.Unmarshal(usage.UsageSnapshot, &observation) != nil || observation["usage"] != nil || observation["actual_microcredits"] != nil || observation["billing_notice"] != nil {
			return missingReferenceReleaseRequest{}, false, ErrForbidden
		}
		if raw, exists := observation["response_disposition"]; exists {
			var disposition string
			if json.Unmarshal(raw, &disposition) != nil || disposition != responseDispositionUsageEvidenceRequired {
				return missingReferenceReleaseRequest{}, false, ErrForbidden
			}
		}
		if raw, exists := observation["usage_found"]; exists {
			var found bool
			if json.Unmarshal(raw, &found) != nil || found {
				return missingReferenceReleaseRequest{}, false, ErrForbidden
			}
		}
	}
	intent := missingReferenceRelease{Version: 1, UsageID: usage.ID, UserID: usage.UserID, HoldID: *usage.HoldID, WalletID: usage.WalletID,
		PriceID: *usage.PriceVersionID, Maximum: usage.MaximumMicrocredits, StartedAt: usage.StartedAt,
		RequestFingerprint: usage.RequestFingerprint, SnapshotDigest: replayDigest(usage.RequestSnapshot),
		DispatchIntegrity: integrity, AcquiredAt: *acquired, Owner: *owner, Cause: missingReferenceDeadlineCause, ReleasePending: true}
	return missingReferenceReleaseRequest{Intent: intent, Pricing: pricing}, !now.Before(usage.StartedAt.Add(24 * time.Hour)), nil
}

func restoreMissingReferenceIntentTx(ctx context.Context, tx pgx.Tx, usage UsageRequest) (*missingReferenceReleaseRequest, error) {
	rows, err := tx.Query(ctx, `SELECT sanitized_snapshot,observed_at,payload_digest FROM usage_convergence_evidence WHERE usage_request_id=$1
 AND evidence_type='CUSTOMER_HOLD_RELEASE' AND source_type='RECONCILER' AND decision='REVIEW' AND evidence_key=$2`, usage.ID, missingReferenceReleaseKey)
	if err != nil {
		return nil, err
	}
	var snapshots [][]byte
	var observedTimes []time.Time
	var payloadDigests []string
	for rows.Next() {
		var raw []byte
		var observed time.Time
		var digest string
		if err := rows.Scan(&raw, &observed, &digest); err != nil {
			rows.Close()
			return nil, err
		}
		snapshots = append(snapshots, raw)
		observedTimes = append(observedTimes, observed)
		payloadDigests = append(payloadDigests, digest)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(snapshots) == 0 {
		return nil, nil
	}
	if len(snapshots) != 1 {
		return nil, ErrForbidden
	}
	if !expiryJSONFields(snapshots[0], []string{"version", "usage_request_id", "user_id", "hold_id", "wallet_id", "price_version_id", "maximum_microcredits", "started_at", "request_fingerprint", "request_snapshot_digest", "dispatch_intent_integrity", "worker_acquired_at", "worker_lease_owner", "cause_code", "release_pending", "customer_hold_released"}) {
		return nil, ErrForbidden
	}
	var intent missingReferenceRelease
	if json.Unmarshal(snapshots[0], &intent) != nil || intent.Version != 1 || !intent.ReleasePending || intent.CustomerHoldReleased || intent.Cause != missingReferenceDeadlineCause ||
		intent.UsageID != usage.ID || intent.UserID != usage.UserID || usage.HoldID == nil || intent.HoldID != *usage.HoldID || intent.WalletID != usage.WalletID ||
		usage.PriceVersionID == nil || intent.PriceID != *usage.PriceVersionID || intent.Maximum != usage.MaximumMicrocredits || !intent.StartedAt.Equal(usage.StartedAt) ||
		intent.RequestFingerprint != usage.RequestFingerprint || intent.SnapshotDigest != replayDigest(usage.RequestSnapshot) || !expiryDigest(intent.DispatchIntegrity) ||
		intent.AcquiredAt.IsZero() || strings.TrimSpace(intent.Owner) == "" || usage.Status == "COMMITTED" {
		return nil, ErrForbidden
	}
	encoded, err := json.Marshal(intent.snapshot())
	if err != nil {
		return nil, err
	}
	if !expiryJSONEqual(encoded, snapshots[0]) || replayDigest(encoded) != payloadDigests[0] ||
		observedTimes[0].Before(intent.StartedAt.Add(24*time.Hour)) || observedTimes[0].Before(intent.AcquiredAt.Add(2*time.Hour)) {
		return nil, ErrForbidden
	}
	pricing, err := missingReferenceFrozenPricing(usage)
	if err != nil {
		return nil, err
	}
	return &missingReferenceReleaseRequest{Intent: intent, Pricing: pricing}, nil
}

func (r *PostgresRepository) PrepareMissingReferenceRelease(ctx context.Context, usageID uuid.UUID) (*missingReferenceReleaseRequest, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	var usage UsageRequest
	if err := scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID), &usage); err != nil {
		return nil, false, err
	}
	if pending, err := restoreMissingReferenceIntentTx(ctx, tx, usage); err != nil {
		return nil, false, err
	} else if pending != nil {
		return pending, true, tx.Commit(ctx)
	}
	// Both coordinators decide under this original usage-row lock. Preserve an
	// already recorded deadline intent above; otherwise an admitted release-only
	// waiver owns recovery instead of waiting for a new automatic deadline.
	if waiver, err := tokenWaiverCommandTx(ctx, tx, usageID); err != nil {
		return nil, false, err
	} else if waiver != nil {
		return nil, false, tx.Commit(ctx)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return nil, false, err
	}
	request, due, err := missingReferenceEligibilityTx(ctx, tx, usage, now)
	if errors.Is(err, ErrForbidden) {
		return nil, false, tx.Commit(ctx)
	}
	if err != nil {
		return nil, false, err
	}
	if !due {
		_, err = tx.Exec(ctx, `UPDATE usage_requests SET reconciliation_next_at=$2,reconciliation_lease_until=NULL,reconciliation_last_error='MISSING_UPSTREAM_REFERENCE_WAITING_DEADLINE',updated_at=$3 WHERE id=$1`, usage.ID, usage.StartedAt.Add(24*time.Hour), now)
		if err != nil {
			return nil, false, err
		}
		return nil, true, tx.Commit(ctx)
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usage.ID, "CUSTOMER_HOLD_RELEASE", "RECONCILER", "REVIEW", missingReferenceReleaseKey, request.Intent.snapshot(), now); err != nil {
		return nil, false, err
	}
	// Never say RELEASED before receiving Billing's exact committed receipt.
	_, err = tx.Exec(ctx, `UPDATE usage_requests SET reconciliation_next_at=$2,reconciliation_stopped_at=NULL,reconciliation_last_error='MISSING_UPSTREAM_REFERENCE_RELEASE_PENDING',updated_at=$2 WHERE id=$1`, usage.ID, now)
	if err != nil {
		return nil, false, err
	}
	return &request, true, tx.Commit(ctx)
}

func (r *PostgresRepository) RecoverStoppedMissingReferences(ctx context.Context, limit int) error {
	if limit < 1 || limit > 100 {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id FROM usage_requests WHERE status='REVIEW' AND execution_status IN ('REVIEW','SETTLEMENT_PENDING')
 AND reconciliation_stopped_at IS NOT NULL AND reconciliation_last_error='MANUAL_RECONCILIATION_REQUIRED'
 AND (reconciliation_next_at IS NULL OR reconciliation_next_at<=transaction_timestamp())
 AND NOT EXISTS(SELECT 1 FROM usage_attempts WHERE usage_request_id=usage_requests.id)
 AND EXISTS(SELECT 1 FROM usage_dispatch_intents WHERE usage_request_id=usage_requests.id AND worker_acquired_at IS NOT NULL)
 ORDER BY reconciliation_stopped_at,id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return err
	}
	for _, id := range ids {
		var usage UsageRequest
		if err := scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1`, id), &usage); err != nil {
			return err
		}
		_, _, err := missingReferenceEligibilityTx(ctx, tx, usage, now)
		if errors.Is(err, ErrForbidden) {
			// A coarse SQL candidate is not financial proof. Keep the exact stop
			// and reason; back off this scan only, so invalid early rows cannot
			// permanently hide eligible later rows. No release/requeue is granted.
			if _, err := tx.Exec(ctx, `UPDATE usage_requests SET reconciliation_next_at=$2 WHERE id=$1 AND reconciliation_stopped_at IS NOT NULL`, id, now.Add(reconciliationBackoff(100))); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE usage_requests SET reconciliation_stopped_at=NULL,reconciliation_next_at=$2,reconciliation_lease_until=NULL,reconciliation_last_error='MISSING_UPSTREAM_REFERENCE_WAITING_DEADLINE',updated_at=$2 WHERE id=$1`, id, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func releaseReceiptMatches(receipt confirmedReleaseReceipt, intent missingReferenceRelease) bool {
	return receipt.Status == "RELEASED" && receipt.ID == intent.HoldID && receipt.UsageID == intent.UsageID &&
		receipt.WalletID == intent.WalletID && receipt.PriceID != nil && *receipt.PriceID == intent.PriceID && receipt.Maximum == intent.Maximum
}

func (r *PostgresRepository) FinishMissingReferenceRelease(ctx context.Context, request missingReferenceReleaseRequest, receipt confirmedReleaseReceipt) error {
	if !releaseReceiptMatches(receipt, request.Intent) {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var usage UsageRequest
	if err := scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, request.Intent.UsageID), &usage); err != nil {
		return err
	}
	pending, err := restoreMissingReferenceIntentTx(ctx, tx, usage)
	if err != nil {
		return err
	}
	if pending == nil || !sameMissingReferenceIntent(pending.Intent, request.Intent) {
		return ErrForbidden
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return err
	}
	confirmed := request.Intent.snapshot()
	confirmed["release_pending"] = false
	confirmed["customer_hold_released"] = true
	confirmed["late_usage_customer_debit_prohibited"] = true
	confirmed["billing_receipt_status"] = receipt.Status
	confirmed["provider_success_inferred"] = false
	confirmed["upstream_usage_unknown"] = true
	if usage.Status == "RELEASED" {
		var exact bool
		encoded, err := json.Marshal(confirmed)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='CUSTOMER_HOLD_RELEASE' AND source_type='BILLING' AND decision='RELEASE' AND evidence_key=$2 AND sanitized_snapshot=$3::jsonb) AND EXISTS(SELECT 1 FROM usage_platform_loss_reviews WHERE usage_request_id=$1 AND cause_code=$4 AND provider_cost_evidence=$3::jsonb AND customer_debit_prohibited)`, usage.ID, missingReferenceReleaseKey, encoded, missingReferenceDeadlineCause).Scan(&exact); err != nil {
			return err
		}
		if !exact {
			return ErrForbidden
		}
		return tx.Commit(ctx)
	}
	if usage.Status != "REVIEW" || (usage.ExecutionStatus != "REVIEW" && usage.ExecutionStatus != "SETTLEMENT_PENDING") {
		return ErrForbidden
	}
	encoded, err := json.Marshal(confirmed)
	if err != nil {
		return err
	}
	// No DO UPDATE: an unrelated historical loss fact is never replaced.
	_, err = tx.Exec(ctx, `INSERT INTO usage_platform_loss_reviews(id,usage_request_id,cause_code,provider_cost_evidence,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(usage_request_id) DO NOTHING`, uuid.Must(uuid.NewV7()), usage.ID, missingReferenceDeadlineCause, encoded, now)
	if err != nil {
		return err
	}
	var exact bool
	if err := tx.QueryRow(ctx, `SELECT cause_code=$2 AND provider_cost_evidence=$3::jsonb AND customer_debit_prohibited FROM usage_platform_loss_reviews WHERE usage_request_id=$1`, usage.ID, missingReferenceDeadlineCause, encoded).Scan(&exact); err != nil {
		return err
	}
	if !exact {
		return ErrForbidden
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usage.ID, "CUSTOMER_HOLD_RELEASE", "BILLING", "RELEASE", missingReferenceReleaseKey, confirmed, now); err != nil {
		return err
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usage.ID, "RECONCILIATION_STOPPED", "RECONCILER", "REVIEW", missingReferenceReleaseKey, map[string]any{"reason_code": missingReferenceDeadlineCause, "automatic_high_risk_retry": false, "customer_hold_released": true, "late_usage_customer_debit_prohibited": true}, now); err != nil {
		return err
	}
	if err := transitionExecutionTx(ctx, tx, usage.ID, []string{"REVIEW", "SETTLEMENT_PENDING"}, "FAILED", missingReferenceDeadlineCause, now); err != nil {
		return err
	}
	if err := addReplayTombstone(ctx, tx, usage.ID, "FAILED", now); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE usage_requests SET status='RELEASED',settlement_status='RELEASED',error_class=$2,terminal_at=$3,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=$2,reconciliation_stopped_at=$3,updated_at=$3 WHERE id=$1 AND status='REVIEW'`, usage.ID, missingReferenceDeadlineCause, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrForbidden
	}
	// Delivery/cancel status and existing numeric provider evidence are retained.
	return tx.Commit(ctx)
}

func (r *PostgresRepository) PreserveMissingReferenceLateUsage(ctx context.Context, usageID uuid.UUID, usage Usage) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	pending, err := lockMissingReferenceReleaseTx(ctx, tx, usageID)
	if err != nil {
		return false, err
	}
	if !pending {
		return false, tx.Commit(ctx)
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 {
		return true, ErrForbidden
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usageID, "RESULT_OBSERVED", "GATEWAY", "REVIEW", "late-usage-after-missing-reference-decision", map[string]any{"usage": usage, "content_persisted": false, "customer_debit_prohibited": true}, r.now()); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}

func (r *PostgresRepository) PauseMissingReferenceRelease(ctx context.Context, request missingReferenceReleaseRequest, failure error) error {
	var response *BillingHTTPError
	if !errors.As(failure, &response) || !definitiveSettlementConflict(failure) {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var usage UsageRequest
	if err := scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, request.Intent.UsageID), &usage); err != nil {
		return err
	}
	if usage.Status == "RELEASED" {
		return tx.Commit(ctx)
	}
	pending, err := restoreMissingReferenceIntentTx(ctx, tx, usage)
	if err != nil {
		return err
	}
	if pending == nil || !sameMissingReferenceIntent(pending.Intent, request.Intent) {
		return ErrForbidden
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usage.ID, "UNCERTAINTY", "RECONCILER", "REVIEW", "missing-reference-release-definite-conflict", map[string]any{"billing_http_status": response.Status, "customer_hold_released": false, "automatic_retry": false}, r.now()); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE usage_requests SET reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_stopped_at=$2,reconciliation_last_error='MISSING_UPSTREAM_REFERENCE_RELEASE_CONFLICT',updated_at=$2 WHERE id=$1 AND status='REVIEW'`, usage.ID, r.now())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func sameMissingReferenceIntent(left, right missingReferenceRelease) bool {
	// Compare timestamps by instant, not time.Time's location pointer.
	started, acquired := left.StartedAt.Equal(right.StartedAt), left.AcquiredAt.Equal(right.AcquiredAt)
	left.StartedAt, right.StartedAt = time.Time{}, time.Time{}
	left.AcquiredAt, right.AcquiredAt = time.Time{}, time.Time{}
	return started && acquired && left == right
}

// A handled decision never falls through to generic manual stop/deadline code.
// Unknown HTTP acknowledgement retries the persisted intent, not a new hold.
func (r *UsageReconciler) reconcileMissingReferenceDeadline(ctx context.Context, item ReconciliationItem) (bool, error) {
	store, ok := r.store.(missingReferenceDeadlineStore)
	if !ok {
		return false, nil
	}
	request, handled, err := store.PrepareMissingReferenceRelease(ctx, item.ID)
	if err != nil {
		return true, err
	}
	if !handled || request == nil {
		return handled, nil
	}
	billing, ok := r.billing.(missingReferenceDeadlineBilling)
	if !ok {
		return true, fmt.Errorf("checked deadline release is unavailable")
	}
	receipt, err := billing.ReleaseMissingReference(ctx, *request)
	if err != nil {
		if definitiveSettlementConflict(err) {
			return true, store.PauseMissingReferenceRelease(ctx, *request, err)
		}
		return true, err
	}
	return true, store.FinishMissingReferenceRelease(ctx, *request, receipt)
}
