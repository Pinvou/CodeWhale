package billing

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const ledgerRepairProofUnavailableCase = "LEDGER_REPAIR_PROOF_UNAVAILABLE"

// Proofs are fetched before the caller starts its serializable transaction.
// Missing entries mean unavailable; they never mean a valid correction.
type ledgerRepairProofSet map[ledgerRepairProofIdentity]ledgerRepairAuthorizationProof

func ledgerRepairAuthorityPayloads(raw json.RawMessage) []reconciliationAuditRecord {
	var payload reconciliationAuditRecord
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	result := []reconciliationAuditRecord{payload}
	for _, peer := range auditRecords(payload["forced_history"]) {
		copy := make(reconciliationAuditRecord, len(payload))
		for key, value := range payload {
			if key != "forced_history" {
				copy[key] = value
			}
		}
		for _, key := range []string{"journals", "postings", "receivables", "ledger_repairs"} {
			copy[key] = peer[key]
		}
		result = append(result, copy)
	}
	return result
}

func ledgerRepairProofIdentities(facts []ReconciliationInternalFact) []ledgerRepairProofIdentity {
	seen := map[ledgerRepairProofIdentity]bool{}
	for _, fact := range facts {
		for _, payload := range ledgerRepairAuthorityPayloads(fact.LedgerSnapshot) {
			var evidence reconciliationAuditRecord
			if json.Unmarshal(payload["ledger_repairs"], &evidence) != nil {
				continue
			}
			for _, c := range auditRecords(evidence["cases"]) {
				request, e1 := uuid.Parse(auditString(c, "approval_request_id"))
				event, e2 := uuid.Parse(auditString(c, "authorization_event_id"))
				epoch := auditInteger(c, "approval_epoch")
				if e1 == nil && e2 == nil && request != uuid.Nil && event != uuid.Nil && epoch != nil && epoch.IsInt64() && epoch.Sign() > 0 {
					seen[ledgerRepairProofIdentity{ApprovalRequestID: request, AuthorizationEventID: event, ApprovalEpoch: epoch.Int64()}] = true
				}
			}
		}
	}
	result := make([]ledgerRepairProofIdentity, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.ApprovalRequestID != b.ApprovalRequestID {
			return a.ApprovalRequestID.String() < b.ApprovalRequestID.String()
		}
		if a.AuthorizationEventID != b.AuthorizationEventID {
			return a.AuthorizationEventID.String() < b.AuthorizationEventID.String()
		}
		return a.ApprovalEpoch < b.ApprovalEpoch
	})
	return result
}

// A failed or incomplete batch leaves only its missing identities unavailable;
// already authenticated batches remain usable for unrelated orders.
func (p *PaymentCoordinator) prefetchLedgerRepairProofs(ctx context.Context, run ReconciliationRun, rows []ProviderBillEntry) (ledgerRepairProofSet, error) {
	date, err := time.Parse("2006-01-02", run.BusinessDate)
	if err != nil {
		return nil, err
	}
	start, end := reconciliationBusinessDateRange(date)
	facts, err := listFourWayFactsForProvider(ctx, p.repository.pool, run.Channel, start, end, run.ProviderID, run.ProviderVersion, rows)
	if err != nil {
		return nil, err
	}
	if err = auditFourWayReconciliationFacts(ctx, p.repository.pool, facts); err != nil {
		return nil, err
	}
	identities := ledgerRepairProofIdentities(facts)
	return p.fetchLedgerRepairProofs(ctx, identities)
}

func (p *PaymentCoordinator) fetchLedgerRepairProofs(ctx context.Context, identities []ledgerRepairProofIdentity) (ledgerRepairProofSet, error) {
	proofs := make(ledgerRepairProofSet, len(identities))
	if len(identities) == 0 || p.ledgerRepairAuthority == nil {
		return proofs, nil
	}
	// The Account authority has a bounded share of the existing command
	// budget. A slow later chunk leaves only unreceived identities unavailable,
	// rather than consuming the caller's entire reconciliation deadline.
	proofCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	returned, _ := p.ledgerRepairAuthority.ReadLedgerRepairProofs(proofCtx, identities)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	for _, item := range returned {
		proofs[item.ledgerRepairProofIdentity] = item
	}
	return proofs, nil
}

func applyLedgerRepairAuthority(facts []ReconciliationInternalFact, proofs ledgerRepairProofSet) {
	for i := range facts {
		fact := &facts[i]
		status := ""
		unavailable := false
		for _, payload := range ledgerRepairAuthorityPayloads(fact.LedgerSnapshot) {
			var evidence reconciliationAuditRecord
			if json.Unmarshal(payload["ledger_repairs"], &evidence) != nil || len(auditRecords(evidence["cases"])) == 0 {
				continue
			}
			for _, c := range auditRecords(evidence["cases"]) {
				request, e1 := uuid.Parse(auditString(c, "approval_request_id"))
				event, e2 := uuid.Parse(auditString(c, "authorization_event_id"))
				epoch := auditInteger(c, "approval_epoch")
				if e1 != nil || e2 != nil || request == uuid.Nil || event == uuid.Nil || epoch == nil || !epoch.IsInt64() || epoch.Sign() <= 0 {
					unavailable = true
					continue
				}
				proof, ok := proofs[ledgerRepairProofIdentity{ApprovalRequestID: request, AuthorizationEventID: event, ApprovalEpoch: epoch.Int64()}]
				if !ok || proof.Status != "VERIFIED" {
					unavailable = true
				}
			}
			valid, err := validateLedgerRepairAuthority(payload, proofs)
			if err != nil {
				unavailable = true
				if status != "INVALID" {
					status = "UNAVAILABLE"
				}
			} else if !valid {
				status = "INVALID"
			} else if status == "" {
				status = "VERIFIED"
			}
		}
		if status == "" {
			continue
		}
		fact.ledgerRepairAuthorityStatus = status
		fact.ledgerRepairProofUnavailable = unavailable
		// No-repair events keep their original digest, including old receipts.
		marker := status
		if unavailable && status == "INVALID" {
			marker += "+UNAVAILABLE"
		}
		fact.IntegrityDigest = digestHex([]byte(fact.IntegrityDigest + "|ledger-repair-authority:" + marker))
		if status == "INVALID" {
			fact.LedgerValid = false
			if fact.IntegrityCaseType == "" {
				fact.IntegrityCaseType = fact.EntryType + "_LEDGER_INVALID"
			}
		} else if status == "UNAVAILABLE" && fact.LedgerValid {
			fact.LedgerValid = false
			fact.IntegrityCaseType = ledgerRepairProofUnavailableCase
		}
	}
}

func reconciliationStoredRows(ctx context.Context, q reconciliationQuerier, runID uuid.UUID) ([]ProviderBillEntry, error) {
	stored, err := q.Query(ctx, `SELECT raw_snapshot FROM reconciliation_provider_rows WHERE reconciliation_run_id=$1 ORDER BY row_number`, runID)
	if err != nil {
		return nil, err
	}
	defer stored.Close()
	var result []ProviderBillEntry
	for stored.Next() {
		var raw []byte
		if err := stored.Scan(&raw); err != nil {
			return nil, err
		}
		var row ProviderBillEntry
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, stored.Err()
}

// Load only repair-bearing journal references owned by this order, regardless
// of the date of the current provider bill. Both repair target columns and an
// orphan reversal reference make the source visible to the structural check.
func ledgerRepairOrderPayloads(ctx context.Context, q reconciliationQuerier, merchant string) ([]reconciliationAuditRecord, error) {
	rows, err := q.Query(ctx, `WITH order_refs(ref) AS (
  SELECT 'payment-transaction:'||t.id::text FROM public.payment_transactions t JOIN public.payment_orders o ON o.id=t.payment_order_id WHERE o.merchant_order_no=$1
  UNION ALL SELECT 'repair-payment-transaction:'||t.id::text FROM public.payment_transactions t JOIN public.payment_orders o ON o.id=t.payment_order_id WHERE o.merchant_order_no=$1
  UNION ALL SELECT 'refund:'||r.id::text FROM public.refunds r JOIN public.payment_orders o ON o.id=r.payment_order_id WHERE o.merchant_order_no=$1
  UNION ALL SELECT 'refund-repair:'||r.id::text FROM public.refunds r JOIN public.payment_orders o ON o.id=r.payment_order_id WHERE o.merchant_order_no=$1
  UNION ALL SELECT 'forced-refund:'||f.id::text FROM public.forced_refund_cases f JOIN public.payment_orders o ON o.id=f.payment_order_id WHERE o.merchant_order_no=$1
  UNION ALL SELECT 'payment-transaction:'||c.payment_transaction_id::text||':chargeback:'||c.provider_chargeback_id FROM public.chargeback_cases c JOIN public.payment_orders o ON o.id=c.payment_order_id WHERE o.merchant_order_no=$1
  UNION ALL SELECT 'chargeback-repair:'||c.id::text FROM public.chargeback_cases c JOIN public.payment_orders o ON o.id=c.payment_order_id WHERE o.merchant_order_no=$1
 ), source_refs AS (
 SELECT DISTINCT journal.business_reference AS ref FROM order_refs refs
 JOIN public.journal_entries journal ON journal.business_reference=refs.ref
 WHERE EXISTS (SELECT 1 FROM public.ledger_repair_cases repair WHERE repair.target_journal_entry_id=journal.id
   OR repair.target_posting_id IN (SELECT id FROM public.ledger_postings WHERE journal_entry_id=journal.id))
 OR EXISTS (SELECT 1 FROM public.ledger_postings p JOIN public.journal_entries reversal
   ON reversal.business_reference='ledger-repair:'||p.id::text WHERE p.journal_entry_id=journal.id)
 ) SELECT jsonb_build_object(
  'journals',COALESCE((SELECT jsonb_agg(to_jsonb(j)) FROM public.journal_entries j WHERE j.business_reference=src.ref),'[]'::jsonb),
  'postings',COALESCE((SELECT jsonb_agg(to_jsonb(p)||jsonb_build_object('account',to_jsonb(a))) FROM public.ledger_postings p JOIN public.journal_entries j ON j.id=p.journal_entry_id LEFT JOIN public.ledger_accounts a ON a.id=p.ledger_account_id WHERE j.business_reference=src.ref),'[]'::jsonb),
  `+reconciliationLedgerRepairEvidenceSQL(`root.business_reference=src.ref`)+`)
 FROM source_refs src ORDER BY src.ref`, merchant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var payloads []reconciliationAuditRecord
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var payload reconciliationAuditRecord
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	return payloads, rows.Err()
}

func (p *PaymentCoordinator) prefetchOrderRepairProofs(ctx context.Context, merchant string) (ledgerRepairProofSet, error) {
	payloads, err := ledgerRepairOrderPayloads(ctx, p.repository.pool, merchant)
	if err != nil {
		return nil, err
	}
	facts := make([]ReconciliationInternalFact, 0, len(payloads))
	for _, payload := range payloads {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		facts = append(facts, ReconciliationInternalFact{LedgerSnapshot: raw})
	}
	return p.fetchLedgerRepairProofs(ctx, ledgerRepairProofIdentities(facts))
}

func validateOrderRepairAuthority(ctx context.Context, q reconciliationQuerier, merchant string, proofs ledgerRepairProofSet) error {
	payloads, err := ledgerRepairOrderPayloads(ctx, q, merchant)
	if err != nil {
		return err
	}
	for _, payload := range payloads {
		if _, valid := reconciliationLedgerRepairPostings(payload); !valid {
			return ErrConflict
		}
		valid, err := validateLedgerRepairAuthority(payload, proofs)
		if err != nil {
			return errors.Join(ErrConflict, err)
		}
		if !valid {
			return ErrConflict
		}
	}
	return nil
}

func (r *PostgresRepository) validateReconciliationIgnoreAuthority(ctx context.Context, tx pgx.Tx, item AdminReconciliationCase, proofs ledgerRepairProofSet) error {
	if item.CaseType == ledgerRepairProofUnavailableCase {
		return ErrConflict
	}
	return validateOrderRepairAuthority(ctx, tx, item.MerchantOrderNo, proofs)
}
