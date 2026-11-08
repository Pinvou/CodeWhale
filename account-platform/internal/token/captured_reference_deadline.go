package token

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const capturedReferenceReleaseKey = "captured-upstream-reference-deadline:v1"

// Caller already holds the usage lock; preserve the existing missing key and
// distinguish only the one captured-reference decision introduced here.
func deadlineLateKeyTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, kind string) (string, error) {
	var captured bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='CUSTOMER_HOLD_RELEASE' AND source_type='RECONCILER' AND decision='REVIEW' AND evidence_key=$2)`, id, capturedReferenceReleaseKey).Scan(&captured)
	if err != nil {
		return "", err
	}
	if captured {
		return "late-" + kind + "-after-captured-reference-decision", nil
	}
	return "late-" + kind + "-after-missing-reference-decision", nil
}

// This is the original generic deadline's release decision, not the personal
// missing-reference policy. In particular it retains REVIEW and nullable price.
type capturedReferenceRelease struct {
	Version              int        `json:"version"`
	UsageID              uuid.UUID  `json:"usage_request_id"`
	UserID               uuid.UUID  `json:"user_id"`
	OrganizationID       *uuid.UUID `json:"organization_id"`
	HoldID               uuid.UUID  `json:"hold_id"`
	WalletID             uuid.UUID  `json:"wallet_id"`
	PriceID              *uuid.UUID `json:"price_version_id"`
	Maximum              int64      `json:"maximum_microcredits"`
	StartedAt            time.Time  `json:"started_at"`
	Provider             string     `json:"provider_id"`
	Adapter              string     `json:"adapter_version"`
	ReferenceDigest      string     `json:"upstream_reference_digest"`
	Failure              string     `json:"failure_code"`
	ReleasePending       bool       `json:"release_pending"`
	CustomerHoldReleased bool       `json:"customer_hold_released"`
}

func (v capturedReferenceRelease) snapshot() map[string]any {
	return map[string]any{"version": v.Version, "usage_request_id": v.UsageID, "user_id": v.UserID, "organization_id": v.OrganizationID,
		"hold_id": v.HoldID, "wallet_id": v.WalletID, "price_version_id": v.PriceID, "maximum_microcredits": v.Maximum, "started_at": v.StartedAt,
		"provider_id": v.Provider, "adapter_version": v.Adapter, "upstream_reference_digest": v.ReferenceDigest, "failure_code": v.Failure,
		"release_pending": v.ReleasePending, "customer_hold_released": v.CustomerHoldReleased}
}

func capturedAttemptTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (string, string, string, error) {
	var provider, adapter, reference, digest string
	err := tx.QueryRow(ctx, `SELECT provider_id,adapter_version,upstream_request_id,upstream_reference_digest FROM usage_attempts WHERE usage_request_id=$1`, id).Scan(&provider, &adapter, &reference, &digest)
	if err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(adapter) == "" || strings.TrimSpace(reference) == "" || digestString([]byte(reference)) != digest {
		return "", "", "", ErrForbidden
	}
	return provider, adapter, digest, nil
}

func restoreCapturedReferenceTx(ctx context.Context, tx pgx.Tx, u UsageRequest) (*capturedReferenceRelease, error) {
	rows, err := tx.Query(ctx, `SELECT sanitized_snapshot,payload_digest FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='CUSTOMER_HOLD_RELEASE' AND source_type='RECONCILER' AND decision='REVIEW' AND evidence_key=$2`, u.ID, capturedReferenceReleaseKey)
	if err != nil {
		return nil, err
	}
	var raw []byte
	var digest string
	count := 0
	for rows.Next() {
		count++
		if err = rows.Scan(&raw, &digest); err != nil {
			rows.Close()
			return nil, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	if count != 1 {
		return nil, ErrForbidden
	}
	var v capturedReferenceRelease
	if json.Unmarshal(raw, &v) != nil {
		return nil, ErrForbidden
	}
	encoded, err := json.Marshal(v.snapshot())
	if err != nil {
		return nil, err
	}
	if !expiryJSONEqual(encoded, raw) || replayDigest(encoded) != digest || v.Version != 1 || !v.ReleasePending || v.CustomerHoldReleased || v.Failure == "" ||
		v.UsageID != u.ID || v.UserID != u.UserID || !sameOptionalUUID(v.OrganizationID, u.OrganizationID) || u.HoldID == nil || v.HoldID != *u.HoldID || v.WalletID != u.WalletID ||
		!sameOptionalUUID(v.PriceID, u.PriceVersionID) || v.Maximum != u.MaximumMicrocredits || !v.StartedAt.Equal(u.StartedAt) {
		return nil, ErrForbidden
	}
	p, a, d, err := capturedAttemptTx(ctx, tx, u.ID)
	if err != nil {
		return nil, err
	}
	if p != v.Provider || a != v.Adapter || d != v.ReferenceDigest {
		return nil, ErrForbidden
	}
	return &v, nil
}

// Called with the usage lock. Diagnostic metadata copied by migration 000015
// is not a final usage fact. A known or contradictory fact blocks release only;
// this guard never chooses numbers or authorizes a customer debit.
func capturedKnownUsageTx(ctx context.Context, tx pgx.Tx, u UsageRequest, provider, adapter, referenceDigest string) (bool, error) {
	var source string
	var observation []byte
	if err := tx.QueryRow(ctx, `SELECT COALESCE(trusted_usage_source,''),usage_snapshot FROM usage_requests WHERE id=$1`, u.ID).Scan(&source, &observation); err != nil {
		return false, err
	}
	if u.ActualMicrocredits != nil || u.Status == "SETTLEMENT_PENDING" || source == "PROVIDER_FINAL_USAGE" || source == "MIGRATED_SETTLEMENT" {
		return true, nil
	}
	var response replayResponseObservation
	if json.Unmarshal(observation, &response) == nil && response.ResponseDisposition == responseDispositionRejectedBeforeGeneration {
		return true, nil
	}
	var rejected bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_convergence_evidence WHERE usage_request_id=$1 AND source_type='GATEWAY' AND provider_id=$3 AND adapter_version=$4 AND upstream_reference_digest=$5 AND ((evidence_type='RESULT_OBSERVED' AND decision='RECORDED' AND sanitized_snapshot->>'response_disposition'=$2) OR (evidence_type='UNCERTAINTY' AND decision='REVIEW' AND sanitized_snapshot->'sanitized_observation'->>'response_disposition'=$2)))`, u.ID, responseDispositionRejectedBeforeGeneration, provider, adapter, referenceDigest).Scan(&rejected); err != nil {
		return false, err
	}
	if rejected {
		return true, nil
	}
	// Non-empty migrated metadata alone is deliberately ignored above. Numeric
	// final evidence needs its original source; arbitrary RESULT_OBSERVED is not it.
	rows, err := tx.Query(ctx, `SELECT evidence_key,provider_id,adapter_version,upstream_reference_digest,payload_digest,sanitized_snapshot FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='PROVIDER_LOOKUP' AND source_type='PROVIDER_QUERY' AND decision='RECORDED'`, u.ID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, digest string
		var p, a, d *string
		var snapshot []byte
		if err := rows.Scan(&key, &p, &a, &d, &digest, &snapshot); err != nil {
			return false, err
		}
		var fact struct {
			Succeeded bool   `json:"lookup_succeeded"`
			Present   bool   `json:"usage_present"`
			Rejected  bool   `json:"rejected"`
			Usage     *Usage `json:"usage"`
		}
		if json.Unmarshal(snapshot, &fact) != nil {
			return true, nil
		}
		if !fact.Succeeded || !fact.Present && !fact.Rejected {
			continue
		}
		numbers, valid := confirmedUsageNumbers(snapshot)
		encoded, _ := json.Marshal(map[string]any{"lookup_succeeded": fact.Succeeded, "usage_present": fact.Present, "rejected": fact.Rejected, "usage": fact.Usage})
		if !valid || fact.Rejected || fact.Usage == nil || numbers != *fact.Usage || !expiryJSONEqual(encoded, snapshot) || replayDigest(encoded) != digest || key != referenceDigest || p == nil || a == nil || d == nil || *p != provider || *a != adapter || *d != referenceDigest {
			return true, nil
		}
		return true, nil
	}
	return false, rows.Err()
}

type capturedReferenceDeadlineStore interface {
	PrepareCapturedReferenceRelease(context.Context, uuid.UUID, string, bool) (*capturedReferenceRelease, bool, error)
	FinishCapturedReferenceRelease(context.Context, capturedReferenceRelease, capturedReleaseReceipt) error
	PauseCapturedReferenceRelease(context.Context, capturedReferenceRelease) error
}

// create=false only resumes an original decision after explicit human requeue.
// The stopped latch is closed and committed before returning a single RPC right.
func (r *PostgresRepository) PrepareCapturedReferenceRelease(ctx context.Context, id uuid.UUID, failure string, create bool) (*capturedReferenceRelease, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	var u UsageRequest
	if err = scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, id), &u); err != nil {
		return nil, false, err
	}
	v, err := restoreCapturedReferenceTx(ctx, tx, u)
	if err != nil {
		return nil, true, err
	}
	var stopped *time.Time
	var loss bool
	if err = tx.QueryRow(ctx, `SELECT reconciliation_stopped_at,EXISTS(SELECT 1 FROM usage_platform_loss_reviews WHERE usage_request_id=$1) FROM usage_requests WHERE id=$1`, id).Scan(&stopped, &loss); err != nil {
		return nil, v != nil, err
	}
	if v != nil {
		if stopped != nil || loss {
			return nil, true, tx.Commit(ctx)
		}
	} else {
		if !create {
			return nil, false, tx.Commit(ctx)
		}
		if failure != "USAGE_EVIDENCE_UNAVAILABLE" && failure != "USAGE_EVIDENCE_TIMEOUT" {
			return nil, true, ErrForbidden
		}
		if stopped != nil || loss || u.Status != "REVIEW" || (u.ExecutionStatus != "REVIEW" && u.ExecutionStatus != "SETTLEMENT_PENDING") || u.HoldID == nil || u.PriceVersionID == nil || u.MaximumMicrocredits < 0 || u.ErrorClass != nil && *u.ErrorClass == "cancel_hold_recovery_unconfirmed" {
			return nil, true, ErrForbidden
		}
		if pending, e := missingReferenceReleasePendingTx(ctx, tx, id); e != nil {
			return nil, true, e
		} else if pending {
			return nil, true, ErrForbidden
		}
		if waiver, e := tokenWaiverCommandTx(ctx, tx, id); e != nil {
			return nil, true, e
		} else if waiver != nil {
			return nil, true, ErrForbidden
		}
		p, a, d, e := capturedAttemptTx(ctx, tx, id)
		if e != nil {
			return nil, true, e
		}
		known, e := capturedKnownUsageTx(ctx, tx, u, p, a, d)
		if e != nil {
			return nil, true, e
		}
		if known {
			return nil, true, ErrForbidden
		}
		var now time.Time
		if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
			return nil, true, err
		}
		if now.Before(u.StartedAt.Add(24 * time.Hour)) {
			return nil, true, ErrForbidden
		}
		v = &capturedReferenceRelease{Version: 1, UsageID: id, UserID: u.UserID, OrganizationID: u.OrganizationID, HoldID: *u.HoldID, WalletID: u.WalletID, PriceID: u.PriceVersionID, Maximum: u.MaximumMicrocredits, StartedAt: u.StartedAt, Provider: p, Adapter: a, ReferenceDigest: d, Failure: failure, ReleasePending: true}
		if err = appendUsageConvergenceEvidenceTx(ctx, tx, id, "CUSTOMER_HOLD_RELEASE", "RECONCILER", "REVIEW", capturedReferenceReleaseKey, v.snapshot(), now); err != nil {
			return nil, true, err
		}
	}
	if (u.Status != "REVIEW" && u.Status != "SETTLEMENT_PENDING") || u.ErrorClass != nil && *u.ErrorClass == "cancel_hold_recovery_unconfirmed" {
		return nil, true, ErrForbidden
	}
	// A prior stopped decision is never cleared by a reader. Only the existing
	// governed human retry opens this latch; another old claim cannot reopen it.
	if _, err = tx.Exec(ctx, `UPDATE usage_requests SET reconciliation_stopped_at=transaction_timestamp(),reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error='CAPTURED_REFERENCE_RELEASE_REQUIRES_MANUAL_RECOVERY',updated_at=transaction_timestamp() WHERE id=$1`, id); err != nil {
		return nil, true, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, true, err
	}
	return v, true, nil
}

type capturedExtensionProof struct {
	OperationID  uuid.UUID               `json:"operation_id"`
	LeaseSeconds int                     `json:"lease_seconds"`
	Additional   int64                   `json:"additional_microcredits"`
	Receipt      confirmedReleaseReceipt `json:"receipt"`
	ExpiresAt    time.Time               `json:"expires_at"`
}

type capturedReleaseReceipt struct {
	confirmedReleaseReceipt
	Extension *capturedExtensionProof
}

func capturedReceiptMatches(r capturedReleaseReceipt, v capturedReferenceRelease) bool {
	if v.Maximum < 0 || r.Status != "RELEASED" || r.ID != v.HoldID || r.UsageID != v.UsageID || r.WalletID != v.WalletID || !sameOptionalUUID(r.PriceID, v.PriceID) || r.Maximum < v.Maximum {
		return false
	}
	if r.Maximum == v.Maximum {
		return r.Extension == nil
	}
	p := r.Extension
	return p != nil && p.OperationID == finalUsageReservationOperationID(v.UsageID, v.HoldID) && p.LeaseSeconds == 900 && p.Additional > 0 && p.Additional == r.Maximum-v.Maximum && !p.ExpiresAt.IsZero() && p.Receipt.Status == "ACTIVE" && p.Receipt.ID == v.HoldID && p.Receipt.UsageID == v.UsageID && p.Receipt.WalletID == v.WalletID && sameOptionalUUID(p.Receipt.PriceID, v.PriceID) && p.Receipt.Maximum == r.Maximum && p.Receipt.Maximum-p.Additional == v.Maximum
}

func (r *PostgresRepository) PauseCapturedReferenceRelease(ctx context.Context, v capturedReferenceRelease) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var u UsageRequest
	if err = scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, v.UsageID), &u); err != nil {
		return err
	}
	prior, err := restoreCapturedReferenceTx(ctx, tx, u)
	if err != nil {
		return err
	}
	if prior == nil || !expiryJSONEqual(mustCapturedJSON(prior.snapshot()), mustCapturedJSON(v.snapshot())) {
		return ErrForbidden
	}
	var loss bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_platform_loss_reviews WHERE usage_request_id=$1)`, v.UsageID).Scan(&loss); err != nil {
		return err
	}
	if loss {
		return tx.Commit(ctx)
	}
	if _, err = r.stopReconciliationTx(ctx, tx, v.UsageID, "HOLD_RELEASE_UNAVAILABLE:"+v.Failure, r.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func mustCapturedJSON(v map[string]any) []byte { raw, _ := json.Marshal(v); return raw }

func (r *PostgresRepository) FinishCapturedReferenceRelease(ctx context.Context, v capturedReferenceRelease, receipt capturedReleaseReceipt) error {
	if !capturedReceiptMatches(receipt, v) {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var u UsageRequest
	if err = scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, v.UsageID), &u); err != nil {
		return err
	}
	prior, err := restoreCapturedReferenceTx(ctx, tx, u)
	if err != nil {
		return err
	}
	if prior == nil || !expiryJSONEqual(mustCapturedJSON(prior.snapshot()), mustCapturedJSON(v.snapshot())) {
		return ErrForbidden
	}
	confirmed := v.snapshot()
	confirmed["release_pending"] = false
	confirmed["customer_hold_released"] = true
	confirmed["billing_receipt"] = receipt.confirmedReleaseReceipt
	if receipt.Extension != nil {
		confirmed["extension_receipt"] = receipt.Extension
	}
	confirmed["late_usage_customer_debit_prohibited"] = true
	raw, err := json.Marshal(confirmed)
	if err != nil {
		return err
	}
	var previous []byte
	var cause string
	var prohibited bool
	err = tx.QueryRow(ctx, `SELECT cause_code,provider_cost_evidence,customer_debit_prohibited FROM usage_platform_loss_reviews WHERE usage_request_id=$1`, u.ID).Scan(&cause, &previous, &prohibited)
	if err == nil {
		if cause != "TRUSTED_USAGE_DEADLINE_EXCEEDED" || !prohibited || !expiryJSONEqual(previous, raw) {
			return ErrForbidden
		}
		var exact bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='CUSTOMER_HOLD_RELEASE' AND source_type='BILLING' AND decision='RELEASE' AND evidence_key=$2 AND sanitized_snapshot=$3::jsonb) AND reconciliation_stopped_at IS NOT NULL FROM usage_requests WHERE id=$1`, u.ID, capturedReferenceReleaseKey, raw).Scan(&exact); err != nil {
			return err
		}
		if !exact {
			return ErrForbidden
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if u.Status != "REVIEW" && u.Status != "SETTLEMENT_PENDING" {
		return ErrForbidden
	}
	now := r.now()
	if _, err = tx.Exec(ctx, `INSERT INTO usage_platform_loss_reviews(id,usage_request_id,cause_code,provider_cost_evidence,created_at) VALUES($1,$2,'TRUSTED_USAGE_DEADLINE_EXCEEDED',$3,$4)`, uuid.Must(uuid.NewV7()), u.ID, raw, now); err != nil {
		return err
	}
	if err = appendUsageConvergenceEvidenceTx(ctx, tx, u.ID, "CUSTOMER_HOLD_RELEASE", "BILLING", "RELEASE", capturedReferenceReleaseKey, confirmed, now); err != nil {
		return err
	}
	changed, err := r.stopReconciliationTx(ctx, tx, u.ID, "TRUSTED_USAGE_DEADLINE_EXCEEDED:"+v.Failure, now)
	if err != nil {
		return err
	}
	if !changed {
		return ErrForbidden
	}
	return tx.Commit(ctx)
}

func (r *UsageReconciler) reconcileCapturedReferenceDeadline(ctx context.Context, item ReconciliationItem, failure string, create bool) (bool, error) {
	store, ok := r.store.(capturedReferenceDeadlineStore)
	if !ok {
		return false, nil
	}
	v, handled, err := store.PrepareCapturedReferenceRelease(ctx, item.ID, failure, create)
	if errors.Is(err, ErrForbidden) {
		if stopErr := r.stop(ctx, item, "MANUAL_RECONCILIATION_REQUIRED"); stopErr != nil {
			return true, stopErr
		}
	}
	if err != nil || v == nil {
		return handled, err
	}
	billing, ok := r.billing.(interface {
		ReleaseCapturedReference(context.Context, capturedReferenceRelease) (capturedReleaseReceipt, error)
	})
	if !ok {
		return true, store.PauseCapturedReferenceRelease(ctx, *v)
	}
	receipt, err := billing.ReleaseCapturedReference(ctx, *v)
	if err != nil {
		if pauseErr := store.PauseCapturedReferenceRelease(ctx, *v); pauseErr != nil {
			return true, pauseErr
		}
		return true, err
	}
	return true, store.FinishCapturedReferenceRelease(ctx, *v, receipt)
}
