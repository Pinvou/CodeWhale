package billing

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"time"

	"github.com/google/uuid"
)

// A correction changes the entitlement split, never the original cash funding.
// Retain both views and prove the immutable receipt and its paired wallet journal
// before accepting a new lot as part of a payment's lineage.
type reconciliationCorrectionAmounts struct {
	funding, effective map[string]*big.Int
	kinds              map[string]string
	byKind             map[string]*big.Int
	pieces             map[string][]*big.Int
	total              *big.Int
}

func auditCorrectionAmounts(request reconciliationAuditRequest, payload reconciliationAuditRecord, wallet, packageID string) (reconciliationCorrectionAmounts, bool) {
	a := reconciliationCorrectionAmounts{funding: map[string]*big.Int{}, effective: map[string]*big.Int{}, kinds: map[string]string{}, byKind: map[string]*big.Int{}, pieces: map[string][]*big.Int{}, total: new(big.Int)}
	receipts := auditRecords(payload["corrections"])
	if len(receipts) != 1 {
		return a, false
	}
	r := receipts[0]
	for _, key := range []string{"id", "approval_request_id", "authorization_event_id", "package_id", "wallet_id", "payment_order_id", "payment_transaction_id", "source_component_id", "source_lot_id", "target_component_id", "target_lot_id", "journal_entry_id"} {
		id, err := uuid.Parse(auditString(r, key))
		if err != nil || id == uuid.Nil {
			return a, false
		}
	}
	for _, key := range []string{"execution_key_digest", "source_evidence_digest", "facts_digest"} {
		value := auditString(r, key)
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != value {
			return a, false
		}
	}
	amount, epoch := auditInteger(r, "amount_microcredits"), auditInteger(r, "approval_epoch")
	sourceKind, targetKind := auditString(r, "source_type"), auditString(r, "target_type")
	if amount == nil || amount.Sign() <= 0 || epoch == nil || epoch.Sign() <= 0 || auditString(r, "reason_code") == "" ||
		auditString(r, "wallet_id") != wallet || auditString(r, "package_id") != packageID || auditString(r, "payment_order_id") != request.Order || auditString(r, "payment_transaction_id") != request.Transaction ||
		(sourceKind != "BASE" && sourceKind != "BONUS") || (targetKind != "BASE" && targetKind != "BONUS") || sourceKind == targetKind ||
		auditString(r, "source_component_id") == auditString(r, "target_component_id") || auditString(r, "source_lot_id") == auditString(r, "target_lot_id") {
		return a, false
	}
	var source, target reconciliationAuditRecord
	originalKinds := map[string]bool{}
	ids := map[string]bool{}
	for _, c := range auditRecords(payload["components"]) {
		var lot reconciliationAuditRecord
		if json.Unmarshal(c["lot"], &lot) != nil {
			return a, false
		}
		id, lotID, kind := auditString(c, "id"), auditString(c, "credit_lot_id"), auditString(c, "component_type")
		original, out := auditInteger(c, "original_microcredits"), auditInteger(c, "reclassified_out_microcredits")
		lotOriginal := auditInteger(lot, "original_microcredits")
		if id == "" || ids[id] || lotID == "" || a.kinds[lotID] != "" || original == nil || out == nil || lotOriginal == nil || original.Sign() < 0 || out.Sign() < 0 || out.Cmp(original) > 0 || original.Cmp(lotOriginal) != 0 ||
			auditString(c, "package_id") != packageID || auditString(lot, "id") != lotID || auditString(lot, "wallet_id") != wallet || auditString(lot, "source_type") != "RECHARGE" || (kind != "BASE" && kind != "BONUS") {
			return a, false
		}
		ids[id] = true
		a.kinds[lotID] = kind
		effective := new(big.Int).Sub(original, out)
		a.effective[lotID] = effective
		a.total.Add(a.total, effective)
		if a.byKind[kind] == nil {
			a.byKind[kind] = new(big.Int)
		}
		a.byKind[kind].Add(a.byKind[kind], effective)
		a.pieces[kind] = append(a.pieces[kind], new(big.Int).Set(effective))
		if id == auditString(r, "target_component_id") {
			if lotID != auditString(r, "target_lot_id") || kind != targetKind || original.Cmp(amount) != 0 || out.Sign() != 0 {
				return a, false
			}
			target = c
			continue
		}
		if originalKinds[kind] || !auditOriginalFundingReference(auditString(lot, "source_reference"), request.Transaction, kind) {
			return a, false
		}
		originalKinds[kind] = true
		a.funding[lotID] = new(big.Int).Set(original)
		if id == auditString(r, "source_component_id") {
			if lotID != auditString(r, "source_lot_id") || kind != sourceKind || out.Cmp(amount) != 0 {
				return a, false
			}
			source = c
		} else if out.Sign() != 0 {
			return a, false
		}
	}
	if source == nil || target == nil {
		return a, false
	}
	var targetLot reconciliationAuditRecord
	_ = json.Unmarshal(target["lot"], &targetLot)
	// Execution derives SourceEvidenceRef from the package's payment identity,
	// not the selected lot's optional :bonus suffix.
	if auditString(targetLot, "source_reference") != "payment-transaction:"+request.Transaction+":correction:"+auditString(r, "id") {
		return a, false
	}
	for _, key := range []string{"credited_at", "expires_at"} {
		left, err := time.Parse(time.RFC3339Nano, auditString(source, key))
		right, err2 := time.Parse(time.RFC3339Nano, auditString(target, key))
		if err != nil || err2 != nil || !left.Equal(right) {
			return a, false
		}
	}
	journals := auditRecords(payload["correction_journals"])
	if len(journals) != 1 || auditString(journals[0], "id") != auditString(r, "journal_entry_id") || auditString(journals[0], "entry_type") != "ADJUSTMENT" || auditString(journals[0], "business_reference") != "approval-execution:"+auditString(r, "approval_request_id") {
		return a, false
	}
	posts := auditRecords(payload["correction_postings"])
	if len(posts) != 2 {
		return a, false
	}
	seenLots := map[string]bool{}
	accountID := ""
	for _, post := range posts {
		var account reconciliationAuditRecord
		if json.Unmarshal(post["account"], &account) != nil {
			return a, false
		}
		lot := auditString(post, "credit_lot_id")
		delta := auditInteger(post, "delta_microcredits")
		id := auditString(account, "id")
		if delta == nil || seenLots[lot] || id == "" || auditString(post, "ledger_account_id") != id || auditString(post, "journal_entry_id") != auditString(r, "journal_entry_id") || auditString(account, "wallet_id") != wallet || auditString(account, "code") != "wallet:"+wallet || auditString(account, "account_type") != "WALLET_LIABILITY" || (accountID != "" && accountID != id) {
			return a, false
		}
		accountID, seenLots[lot] = id, true
		expected := new(big.Int).Set(amount)
		if lot == auditString(r, "source_lot_id") {
			expected.Neg(expected)
		} else if lot != auditString(r, "target_lot_id") {
			return a, false
		}
		if delta.Cmp(expected) != 0 {
			return a, false
		}
	}
	return a, true
}

func auditOriginalFundingReference(reference, transaction, kind string) bool {
	for _, prefix := range []string{"payment-transaction:", "repair-payment-transaction:"} {
		if reference == prefix+transaction || kind == "BONUS" && reference == prefix+transaction+":bonus" {
			return true
		}
	}
	return false
}

// Application rounds each component, including a correction-created component.
// Summing first would change the existing cash-to-rights rounding contract.
func auditComponentProportion(pieces []*big.Int, amount, denominator *big.Int, ceiling bool) (*big.Int, bool) {
	result := new(big.Int)
	if amount == nil || denominator == nil || amount.Sign() < 0 || denominator.Sign() <= 0 {
		return result, false
	}
	for _, piece := range pieces {
		if piece == nil || piece.Sign() < 0 {
			return result, false
		}
		value := new(big.Int).Mul(piece, amount)
		if ceiling && value.Sign() > 0 {
			value.Add(value, new(big.Int).Sub(denominator, big.NewInt(1)))
		}
		result.Add(result, value.Quo(value, denominator))
	}
	return result, true
}
