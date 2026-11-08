package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// An unknown payment date is not a reconciliation business date. Its manual
// investigation follows the frozen provider generation and order, while each
// run still owns its original, date-scoped immutable observation and fact digest.
func reconciliationCaseIdentity(run ReconciliationRun, difference ReconciliationDifference) string {
	if !unknownDateOrphanDifference(difference) {
		return reconciliationDifferenceDigest(run, difference)
	}
	raw, _ := json.Marshal([]any{"unknown-date-orphan-order-v1", run.ProviderID, run.ProviderVersion, run.Channel, *difference.PaymentOrderID})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func unknownDateOrphanDifference(difference ReconciliationDifference) bool {
	if difference.CaseType != "PAYMENT_TRANSACTION_MISSING" || difference.Provider != nil || difference.PaymentOrderID == nil {
		return false
	}
	var snapshot struct {
		DateVerification bool `json:"payment_date_verification_required"`
		Order            struct {
			PaidAt json.RawMessage `json:"paid_at"`
		} `json:"payment_order"`
	}
	if json.Unmarshal(difference.InternalSnapshot, &snapshot) != nil || !snapshot.DateVerification || string(snapshot.Order.PaidAt) != "null" {
		return false
	}
	return isOrphanPaymentOrderFact(ReconciliationInternalFact{EntryType: difference.InternalEntryType, ProviderReferenceID: difference.InternalReference, PaymentOrderID: *difference.PaymentOrderID, InternalSnapshot: difference.InternalSnapshot})
}

// Old writers included the scan date in their key. Preserve their original rows
// and immutable evidence; select the earliest matching investigation. Historical
// duplicate cases are retained, never silently deleted or financially disposed.
func unknownDateOrphanCases(ctx context.Context, tx pgx.Tx, run ReconciliationRun, differences []ReconciliationDifference) (map[string]reconciliationUnknownDateCase, error) {
	byOrder := make(map[uuid.UUID]ReconciliationDifference)
	orders := make([]uuid.UUID, 0, len(differences))
	for _, difference := range differences {
		if unknownDateOrphanDifference(difference) {
			byOrder[*difference.PaymentOrderID] = difference
			orders = append(orders, *difference.PaymentOrderID)
		}
	}
	found := make(map[string]reconciliationUnknownDateCase)
	if len(orders) == 0 {
		return found, nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (c.payment_order_id) c.id,c.payment_order_id,c.version
FROM reconciliation_cases c JOIN reconciliation_runs run ON run.id=c.reconciliation_run_id
WHERE run.provider_id=$1 AND run.provider_version=$2 AND run.channel=$3
AND c.payment_order_id=ANY($4::uuid[]) AND c.case_type='PAYMENT_TRANSACTION_MISSING'
AND c.internal_snapshot->'payment_date_verification_required'='true'::jsonb
AND c.internal_snapshot->'payment_order'->'paid_at'='null'::jsonb
ORDER BY c.payment_order_id,c.created_at,c.id`, run.ProviderID, run.ProviderVersion, run.Channel, orders)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var candidate reconciliationUnknownDateCase
		var order uuid.UUID
		if err := rows.Scan(&candidate.ID, &order, &candidate.Version); err != nil {
			return nil, err
		}
		difference := byOrder[order]
		key := reconciliationCaseIdentity(run, difference)
		found[key] = candidate
	}
	return found, rows.Err()
}

type reconciliationUnknownDateCase struct {
	ID      uuid.UUID
	Version int64
}

// Case identity is intentionally broader than an IGNORE acceptance. Normalize
// only the observation date in this fingerprint, retaining the original factual
// comparison dimensions. A changed fact reopens the same investigation.
func unknownDateOrphanFingerprint(run ReconciliationRun, difference ReconciliationDifference) string {
	run.BusinessDate = ""
	return reconciliationDifferenceDigest(run, difference)
}

func unknownDateOrphanOriginalDifference(item AdminReconciliationCase) ReconciliationDifference {
	difference := ReconciliationDifference{CaseType: item.CaseType, PaymentOrderID: item.PaymentOrderID, MerchantOrderNo: item.MerchantOrderNo, InternalSnapshot: item.InternalSnapshot, InternalEntryType: "PAYMENT"}
	if item.PaymentOrderID != nil {
		difference.InternalReference = "ORPHAN_PAYMENT_ORDER:" + item.PaymentOrderID.String()
	}
	var snapshot struct {
		Order struct {
			Amount   int64  `json:"amount_fen"`
			Currency string `json:"currency"`
		} `json:"payment_order"`
	}
	if json.Unmarshal(item.InternalSnapshot, &snapshot) == nil {
		difference.InternalAmountFen, difference.InternalCurrency = snapshot.Order.Amount, snapshot.Order.Currency
	}
	return difference
}

func unknownDateOrphanRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (ReconciliationRun, error) {
	var run ReconciliationRun
	err := tx.QueryRow(ctx, `SELECT provider_id,provider_version,channel FROM reconciliation_runs WHERE id=$1`, runID).Scan(&run.ProviderID, &run.ProviderVersion, &run.Channel)
	return run, err
}

// Add acceptance evidence to the existing private immutable command receipt.
// Its ordinary AdminReconciliationCase JSON fields and replay remain compatible.
func recordUnknownDateOrphanAcceptance(ctx context.Context, tx pgx.Tx, item AdminReconciliationCase, observedID uuid.UUID, after []byte) ([]byte, error) {
	difference := unknownDateOrphanOriginalDifference(item)
	if !unknownDateOrphanDifference(difference) {
		if observedID != uuid.Nil {
			return nil, ErrConflict
		}
		return after, nil
	}
	run, err := unknownDateOrphanRun(ctx, tx, item.RunID)
	if err != nil {
		return nil, err
	}
	var observation struct {
		ID               uuid.UUID       `json:"observation_id"`
		InternalSnapshot json.RawMessage `json:"internal_snapshot"`
	}
	if len(item.LatestObservation) > 0 {
		if err := json.Unmarshal(item.LatestObservation, &observation); err != nil || observation.ID == uuid.Nil {
			return nil, ErrConflict
		}
		latest := item
		latest.InternalSnapshot = observation.InternalSnapshot
		latestDifference := unknownDateOrphanOriginalDifference(latest)
		if !unknownDateOrphanDifference(latestDifference) {
			return nil, ErrConflict
		}
		// A legacy client may still accept the unchanged original fact. It must
		// never silently accept a changed snapshot it cannot display or bind.
		if observedID == uuid.Nil {
			if unknownDateOrphanFingerprint(run, latestDifference) != unknownDateOrphanFingerprint(run, difference) {
				return nil, ErrConflict
			}
		} else if observedID != observation.ID {
			return nil, ErrConflict
		}
		difference = latestDifference
	} else if observedID != uuid.Nil {
		return nil, ErrConflict
	}
	var receipt map[string]json.RawMessage
	if err := json.Unmarshal(after, &receipt); err != nil {
		return nil, err
	}
	receipt["unknown_date_accepted_fact_digest"], _ = json.Marshal(unknownDateOrphanFingerprint(run, difference))
	return json.Marshal(receipt)
}

// The original case snapshots remain immutable. Its read projection separately
// exposes the latest associated observation, so IGNORE can name the exact facts
// the administrator saw. Replayed command receipts retain their original view.
const unknownDateOrphanLatestObservationSQL = `CASE WHEN case_type='PAYMENT_TRANSACTION_MISSING'
AND internal_snapshot->'payment_date_verification_required'='true'::jsonb
AND internal_snapshot->'payment_order'->'paid_at'='null'::jsonb
THEN (SELECT jsonb_build_object('observation_id',observation.id,'reconciliation_run_id',observed_run.id,
'business_date',observed_run.business_date,'observed_at',observation.created_at,'internal_snapshot',fact.internal_snapshot)
FROM reconciliation_observation_progress progress
JOIN reconciliation_observations observation ON observation.id=progress.observation_id
JOIN reconciliation_runs observed_run ON observed_run.id=observation.run_id
JOIN reconciliation_observation_facts fact ON fact.observation_id=observation.id
AND fact.payment_order_id=reconciliation_cases.payment_order_id AND fact.entry_type='PAYMENT'
AND fact.provider_reference_id='ORPHAN_PAYMENT_ORDER:'||reconciliation_cases.payment_order_id::text
WHERE progress.case_id=reconciliation_cases.id
ORDER BY observation.created_at DESC,observation.id DESC LIMIT 1) END`

func changedIgnoredUnknownDateOrphan(ctx context.Context, tx pgx.Tx, caseID uuid.UUID, run ReconciliationRun, difference ReconciliationDifference) (bool, error) {
	var accepted *string
	err := tx.QueryRow(ctx, `SELECT after_snapshot->>'unknown_date_accepted_fact_digest' FROM reconciliation_case_commands WHERE reconciliation_case_id=$1 AND action='IGNORE' ORDER BY created_at DESC,id DESC LIMIT 1`, caseID).Scan(&accepted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if accepted == nil {
		// Legacy IGNORE accepted the original immutable case snapshot.
		var original AdminReconciliationCase
		if err := scanReconciliationCase(tx.QueryRow(ctx, reconciliationCaseSelect+` WHERE id=$1`, caseID), &original); err != nil {
			return false, err
		}
		fingerprint := unknownDateOrphanFingerprint(run, unknownDateOrphanOriginalDifference(original))
		accepted = &fingerprint
	}
	return *accepted != unknownDateOrphanFingerprint(run, difference), nil
}
