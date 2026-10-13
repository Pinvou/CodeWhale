package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// The audit compares microcredit postings to frozen financial quantities, never
// to supplier fen. Current remaining balances are deliberately not initial credit.
type reconciliationAuditRequest struct {
	Index       int      `json:"index"`
	Order       string   `json:"order_id"`
	Transaction string   `json:"transaction_id"`
	Source      string   `json:"source_id"`
	References  []string `json:"journal_refs"`
	IsForced    bool     `json:"is_forced"`
}
type reconciliationAuditRecord map[string]json.RawMessage

func auditString(r reconciliationAuditRecord, k string) string {
	var s string
	_ = json.Unmarshal(r[k], &s)
	return s
}
func auditInteger(r reconciliationAuditRecord, k string) *big.Int {
	v := new(big.Int)
	if _, ok := v.SetString(string(r[k]), 10); !ok {
		return nil
	}
	return v
}
func auditRecords(raw json.RawMessage) []reconciliationAuditRecord {
	var r []reconciliationAuditRecord
	_ = json.Unmarshal(raw, &r)
	return r
}

func auditFourWayReconciliationFacts(ctx context.Context, q reconciliationQuerier, facts []ReconciliationInternalFact) error {
	requests := make([]reconciliationAuditRequest, 0, len(facts))
	for i, f := range facts {
		var internal reconciliationAuditRecord
		if err := json.Unmarshal(f.InternalSnapshot, &internal); err != nil {
			return fmt.Errorf("decode reconciliation internal facts: %w", err)
		}
		request := reconciliationAuditRequest{Index: i, Order: f.PaymentOrderID.String()}
		if f.EntryType == "PAYMENT" {
			var transaction reconciliationAuditRecord
			if err := json.Unmarshal(internal["payment_transaction"], &transaction); err != nil {
				return err
			}
			request.Transaction = auditString(transaction, "id")
			request.References = []string{"payment-transaction:" + request.Transaction, "repair-payment-transaction:" + request.Transaction}
		} else {
			var source reconciliationAuditRecord
			if err := json.Unmarshal(f.RefundSnapshot, &source); err != nil {
				return err
			}
			request.Transaction = auditString(source, "payment_transaction_id")
			request.Source = auditString(source, "id")
			switch {
			case f.EntryType == "CHARGEBACK":
				request.References = []string{"payment-transaction:" + request.Transaction + ":chargeback:" + auditString(source, "provider_chargeback_id")}
			case auditString(source, "provider_refund_id") != "":
				request.IsForced = true
				request.References = []string{"forced-refund:" + request.Source}
			default:
				request.References = []string{"refund:" + request.Source}
			}
		}
		requests = append(requests, request)
	}
	if len(requests) == 0 {
		return nil
	}
	raw, err := json.Marshal(requests)
	if err != nil {
		return err
	}
	rows, err := q.Query(ctx, `SELECT input.index,jsonb_build_object(
 'order',(SELECT to_jsonb(o) FROM payment_orders o WHERE o.id=input.order_id::uuid),
 'transaction',(SELECT to_jsonb(t) FROM payment_transactions t WHERE t.id=input.transaction_id::uuid),
 'packages',COALESCE((SELECT jsonb_agg(to_jsonb(p)) FROM entitlement_packages p WHERE p.source_payment_transaction_id=input.transaction_id::uuid),'[]'),
 'components',COALESCE((SELECT jsonb_agg(to_jsonb(c)||jsonb_build_object('lot',to_jsonb(l))) FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id LEFT JOIN credit_lots l ON l.id=c.credit_lot_id WHERE p.source_payment_transaction_id=input.transaction_id::uuid),'[]'),
 'corrections',COALESCE((SELECT jsonb_agg(to_jsonb(c)) FROM entitlement_component_corrections c WHERE c.payment_transaction_id=input.transaction_id::uuid),'[]'),
 'correction_journals',COALESCE((SELECT jsonb_agg(to_jsonb(j)) FROM journal_entries j JOIN entitlement_component_corrections c ON c.journal_entry_id=j.id WHERE c.payment_transaction_id=input.transaction_id::uuid),'[]'),
 'correction_postings',COALESCE((SELECT jsonb_agg(to_jsonb(p)||jsonb_build_object('account',to_jsonb(a))) FROM ledger_postings p JOIN entitlement_component_corrections c ON c.journal_entry_id=p.journal_entry_id LEFT JOIN ledger_accounts a ON a.id=p.ledger_account_id WHERE c.payment_transaction_id=input.transaction_id::uuid),'[]'),
 'journals',COALESCE((SELECT jsonb_agg(to_jsonb(j)) FROM journal_entries j WHERE j.business_reference=ANY(input.journal_refs)),'[]'),
 'postings',COALESCE((SELECT jsonb_agg(to_jsonb(post)||jsonb_build_object('account',to_jsonb(a))) FROM ledger_postings post JOIN journal_entries j ON j.id=post.journal_entry_id LEFT JOIN ledger_accounts a ON a.id=post.ledger_account_id WHERE j.business_reference=ANY(input.journal_refs)),'[]'),
 'allocations',COALESCE((SELECT jsonb_agg(to_jsonb(a)||jsonb_build_object('receivable_wallet_id',r.wallet_id)) FROM receivable_allocations a JOIN receivables r ON r.id=a.receivable_id JOIN journal_entries j ON j.id=a.journal_entry_id WHERE j.business_reference=ANY(input.journal_refs)),'[]'),
 'receivables',COALESCE((SELECT jsonb_agg(to_jsonb(r)) FROM receivables r WHERE r.source_reference=ANY(input.journal_refs)),'[]'),
 'reservations',COALESCE((SELECT jsonb_agg(to_jsonb(r)) FROM refund_component_reservations r WHERE r.refund_id=NULLIF(input.source_id,'')::uuid),'[]'),
 'forced_history',COALESCE((SELECT jsonb_agg(jsonb_build_object(
  'source',to_jsonb(s),
  'dismissed',EXISTS(SELECT 1 FROM payment_anomaly_cases anomaly JOIN payment_anomaly_commands command ON command.payment_anomaly_case_id=anomaly.id AND command.action='DISMISS' WHERE anomaly.id=s.manual_review_case_id AND anomaly.forced_refund_case_id=s.id AND anomaly.status='RESOLVED' AND anomaly.resolution_code='DISMISS' AND anomaly.financial_action_status='NONE'),
  'journals',COALESCE((SELECT jsonb_agg(to_jsonb(j)) FROM journal_entries j WHERE j.business_reference='forced-refund:'||s.id::text),'[]'),
  'postings',COALESCE((SELECT jsonb_agg(to_jsonb(post)||jsonb_build_object('account',to_jsonb(a))) FROM ledger_postings post JOIN journal_entries j ON j.id=post.journal_entry_id LEFT JOIN ledger_accounts a ON a.id=post.ledger_account_id WHERE j.business_reference='forced-refund:'||s.id::text),'[]'),
  'receivables',COALESCE((SELECT jsonb_agg(to_jsonb(r)) FROM receivables r WHERE r.source_reference='forced-refund:'||s.id::text),'[]')))
 FROM forced_refund_cases s WHERE input.is_forced AND s.payment_transaction_id=input.transaction_id::uuid),'[]'),
 'forced_mixed',input.is_forced AND (EXISTS(SELECT 1 FROM refunds r WHERE r.payment_transaction_id=input.transaction_id::uuid AND (r.status='SUCCEEDED' OR EXISTS(SELECT 1 FROM journal_entries j WHERE j.entry_type='REFUND' AND j.business_reference='refund:'||r.id::text)))
 OR EXISTS(SELECT 1 FROM chargeback_cases c WHERE c.payment_transaction_id=input.transaction_id::uuid AND (c.status='CONFIRMED' OR EXISTS(SELECT 1 FROM journal_entries j WHERE j.entry_type='CHARGEBACK' AND j.business_reference='payment-transaction:'||input.transaction_id||':chargeback:'||c.provider_chargeback_id)))))
 FROM jsonb_to_recordset($1::jsonb) AS input(index int,order_id text,transaction_id text,source_id text,journal_refs text[],is_forced bool)`, raw)
	if err != nil {
		return fmt.Errorf("query reconciliation integrity: %w", err)
	}
	defer rows.Close()
	count := 0
	seen := make([]bool, len(facts))
	for rows.Next() {
		var index int
		var data []byte
		if err := rows.Scan(&index, &data); err != nil {
			return err
		}
		if index < 0 || index >= len(facts) || seen[index] {
			return fmt.Errorf("invalid reconciliation audit row index")
		}
		seen[index] = true
		var payload reconciliationAuditRecord
		if err := json.Unmarshal(data, &payload); err != nil {
			return err
		}
		auditReconciliationFact(&facts[index], requests[index], payload)
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != len(facts) {
		return fmt.Errorf("incomplete reconciliation integrity query")
	}
	return nil
}

func auditReconciliationFact(f *ReconciliationInternalFact, request reconciliationAuditRequest, payload reconciliationAuditRecord) {
	auditReconciliationFactWithForcedGroup(f, request, payload, true)
}

func auditReconciliationFactWithForcedGroup(f *ReconciliationInternalFact, request reconciliationAuditRequest, payload reconciliationAuditRecord, verifyGroup bool) {
	if f.EntryType != "PAYMENT" {
		payload["source"] = f.RefundSnapshot
	}
	journals := auditRecords(payload["journals"])
	f.LedgerPresent = len(journals) > 0
	f.LedgerValid = false
	f.IntegrityCaseType = ""
	// Include postings/accounts and immutable source quantities in retained evidence.
	f.LedgerSnapshot, _ = json.Marshal(payload)
	f.IntegrityDigest = reconciliationIntegrityDigest(payload)
	if !f.LedgerPresent {
		return
	}
	invalid := func() { f.IntegrityCaseType = f.EntryType + "_LEDGER_INVALID" }
	invalid()
	if len(journals) != 1 {
		return
	}
	var order reconciliationAuditRecord
	_ = json.Unmarshal(payload["order"], &order)
	wallet := auditString(order, "wallet_id")
	if wallet == "" {
		return
	}
	var transaction reconciliationAuditRecord
	_ = json.Unmarshal(payload["transaction"], &transaction)
	if auditString(transaction, "payment_order_id") != request.Order || !bytesEqualJSONInteger(transaction["amount_fen"], order["amount_fen"]) || auditString(transaction, "currency") != auditString(order, "currency") {
		return
	}
	packages := auditRecords(payload["packages"])
	components := auditRecords(payload["components"])
	lotAmounts := map[string]*big.Int{}
	lotKinds := map[string]string{}
	kindAmounts := map[string]*big.Int{}
	packageValid := len(packages) == 1 && auditString(packages[0], "wallet_id") == wallet && auditString(packages[0], "payment_order_id") == request.Order && auditString(packages[0], "source_payment_transaction_id") == request.Transaction && auditString(packages[0], "source_type") == "PAYMENT"
	if !packageValid {
		if f.EntryType == "PAYMENT" {
			f.IntegrityCaseType = "PAYMENT_PACKAGE_INVALID"
		}
		return
	}
	if auditString(order, "beneficiary_organization_id") == "" {
		packageValid = auditString(packages[0], "user_id") == auditString(order, "requester_id")
	} else {
		packageValid = auditString(packages[0], "user_id") == ""
	}
	original := new(big.Int)
	pieces := map[string][]*big.Int{}
	corrected := len(auditRecords(payload["corrections"])) > 0
	for _, c := range components {
		if out := auditInteger(c, "reclassified_out_microcredits"); out != nil && out.Sign() != 0 {
			corrected = true
		}
	}
	if corrected {
		amounts, valid := auditCorrectionAmounts(request, payload, wallet, auditString(packages[0], "id"))
		packageValid = packageValid && valid
		original.Set(amounts.total)
		lotKinds, kindAmounts, pieces = amounts.kinds, amounts.byKind, amounts.pieces
		lotAmounts = amounts.effective
		if f.EntryType == "PAYMENT" {
			lotAmounts = amounts.funding
		}
	} else {
		for _, c := range components {
			n := auditInteger(c, "original_microcredits")
			var lot reconciliationAuditRecord
			_ = json.Unmarshal(c["lot"], &lot)
			lotID := auditString(c, "credit_lot_id")
			if n == nil || n.Sign() < 0 || lotID == "" || lotAmounts[lotID] != nil || auditString(lot, "wallet_id") != wallet || auditString(c, "package_id") != auditString(packages[0], "id") || auditInteger(lot, "original_microcredits") == nil || n.Cmp(auditInteger(lot, "original_microcredits")) != 0 {
				packageValid = false
				continue
			}
			lotAmounts[lotID] = new(big.Int).Set(n)
			lotKinds[lotID] = auditString(c, "component_type")
			kind := lotKinds[lotID]
			reference := auditString(lot, "source_reference")
			validReference := reference == "payment-transaction:"+request.Transaction || reference == "repair-payment-transaction:"+request.Transaction
			if kind == "BONUS" {
				validReference = validReference || reference == "payment-transaction:"+request.Transaction+":bonus" || reference == "repair-payment-transaction:"+request.Transaction+":bonus"
			}
			if (kind != "BASE" && kind != "BONUS") || kindAmounts[kind] != nil || !validReference || auditString(lot, "source_type") != "RECHARGE" {
				packageValid = false
			}
			kindAmounts[kind] = new(big.Int).Set(n)
			pieces[kind] = append(pieces[kind], new(big.Int).Set(n))
			original.Add(original, n)
		}
	}
	credit := auditInteger(order, "credit_microcredits")
	var product Product
	if err := json.Unmarshal(order["product_snapshot"], &product); err != nil {
		packageValid = false
	}
	if credit != nil && credit.IsInt64() {
		base, bonus, err := productComponentAmounts(credit.Int64(), product.BasePoints, product.BonusPoints)
		if err != nil {
			packageValid = false
		} else {
			for kind, n := range map[string]int64{"BASE": base, "BONUS": bonus} {
				actual := kindAmounts[kind]
				if actual == nil {
					actual = new(big.Int)
				}
				if actual.Cmp(big.NewInt(n)) != 0 {
					packageValid = false
				}
			}
		}
	}
	if credit == nil || len(components) == 0 || original.Cmp(credit) != 0 || !packageValid || auditInteger(packages[0], "points_total") == nil || auditInteger(packages[0], "points_total").Cmp(credit) != 0 || !bytesEqualJSONInteger(packages[0]["amount_fen"], order["amount_fen"]) {
		if f.EntryType == "PAYMENT" {
			f.IntegrityCaseType = "PAYMENT_PACKAGE_INVALID"
		}
		return
	}
	total := new(big.Int)
	walletExpected := new(big.Int)
	receivableExpected := new(big.Int)
	clearing := paymentClearingAccount.String()
	clearingType := "PAYMENT_CLEARING"
	journalType := "RECHARGE"
	if f.EntryType == "PAYMENT" {
		total.Set(credit)
		walletExpected.Set(credit)
		if strings.HasPrefix(auditString(journals[0], "business_reference"), "repair-") {
			journalType = "REPAIR_CREDIT"
		}
		for _, allocation := range auditRecords(payload["allocations"]) {
			n := auditInteger(allocation, "amount_microcredits")
			if n == nil || n.Sign() <= 0 || auditString(allocation, "wallet_id") != wallet || auditString(allocation, "receivable_wallet_id") != wallet || auditString(allocation, "source_type") != "CASH_RECHARGE_BASE" || auditString(allocation, "source_reference") != auditString(journals[0], "business_reference") {
				return
			}
			receivableExpected.Add(receivableExpected, n)
		}
		walletExpected.Sub(walletExpected, receivableExpected)
		for lot, n := range lotAmounts {
			if lotKinds[lot] == "BASE" {
				n.Sub(n, receivableExpected)
				if n.Sign() < 0 {
					return
				}
			}
		}
	} else {
		clearing = refundClearingAccount.String()
		clearingType = "REFUND_CLEARING"
		journalType = "REFUND"
		var source reconciliationAuditRecord
		_ = json.Unmarshal(f.RefundSnapshot, &source)
		if auditString(source, "payment_order_id") != request.Order || auditString(source, "payment_transaction_id") != request.Transaction || auditString(source, "entitlement_package_id") != auditString(packages[0], "id") {
			return
		}
		if strings.HasPrefix(request.References[0], "refund:") {
			total = auditInteger(source, "credit_microcredits")
			if total == nil || total.Cmp(credit) != 0 || !bytesEqualJSONInteger(source["amount_fen"], transaction["amount_fen"]) {
				return
			}
			for kind, key := range map[string]string{"BASE": "base_microcredits", "BONUS": "bonus_microcredits"} {
				expected := kindAmounts[kind]
				if expected == nil {
					expected = new(big.Int)
				}
				actual := auditInteger(source, key)
				if actual == nil || actual.Cmp(expected) != 0 {
					return
				}
			}
			originals := lotAmounts
			lotAmounts = map[string]*big.Int{}
			for _, reservation := range auditRecords(payload["reservations"]) {
				n := new(big.Int)
				for _, k := range []string{"available_microcredits", "active_grant_microcredits", "suspended_grant_microcredits"} {
					v := auditInteger(reservation, k)
					if v == nil || v.Sign() < 0 {
						return
					}
					n.Add(n, v)
				}
				lot := auditString(reservation, "credit_lot_id")
				if lotKinds[lot] == "" || auditString(reservation, "package_id") != auditString(packages[0], "id") || originals[lot] == nil || n.Cmp(originals[lot]) != 0 || lotAmounts[lot] != nil {
					return
				}
				lotAmounts[lot] = n
				walletExpected.Add(walletExpected, n)
			}
			if walletExpected.Cmp(total) != 0 {
				return
			}
		} else {
			journalType = "FORCED_REFUND"
			if auditString(source, "wallet_id") != wallet {
				return
			}
			if f.EntryType == "CHARGEBACK" {
				journalType = "CHARGEBACK"
				clearing = chargebackClearingAccount.String()
				clearingType = "CHARGEBACK_CLEARING"
			}
			base, bonus := auditInteger(source, "revoked_base_microcredits"), auditInteger(source, "revoked_bonus_microcredits")
			if base == nil || bonus == nil {
				return
			}
			walletExpected.Add(base, bonus)
			for _, r := range auditRecords(payload["receivables"]) {
				n := auditInteger(r, "original_microcredits")
				if n == nil || n.Sign() <= 0 || auditString(r, "id") != auditString(source, "receivable_id") || auditString(r, "source_type") != journalType || auditString(r, "source_reference") != request.References[0] || auditString(r, "wallet_id") != wallet || auditString(r, "payment_order_id") != request.Order || auditString(r, "payment_transaction_id") != request.Transaction || auditString(r, "entitlement_package_id") != auditString(packages[0], "id") {
					return
				}
				receivableExpected.Add(receivableExpected, n)
			}
			if (receivableExpected.Sign() > 0) != (auditString(source, "receivable_id") != "") {
				return
			}
			originalBase, originalBonus := kindAmounts["BASE"], kindAmounts["BONUS"]
			if originalBase == nil {
				originalBase = new(big.Int)
			}
			if originalBonus == nil {
				originalBonus = new(big.Int)
			}
			targetBase, targetBonus := new(big.Int).Set(originalBase), new(big.Int).Set(originalBonus)
			if f.EntryType != "CHARGEBACK" {
				amount, transactionAmount := auditInteger(source, "amount_fen"), auditInteger(transaction, "amount_fen")
				if amount == nil || transactionAmount == nil || !amount.IsInt64() || !transactionAmount.IsInt64() || amount.Sign() <= 0 || amount.Cmp(transactionAmount) > 0 || !originalBase.IsInt64() || !originalBonus.IsInt64() {
					return
				}
				baseAmount, ok := auditComponentProportion(pieces["BASE"], amount, transactionAmount, true)
				if !ok {
					return
				}
				bonusAmount, ok := auditComponentProportion(pieces["BONUS"], amount, transactionAmount, true)
				if !ok {
					return
				}
				targetBase.Set(baseAmount)
				targetBonus.Set(bonusAmount)
				if verifyGroup && !auditForcedRefundGroupComponents(request, payload, pieces, transactionAmount) {
					return
				}
				// Receipts have no durable cross-process execution ordinal. Validate
				// the group's exact cumulative target, then each receipt's possible
				// floor/ceiling increment; never infer ordering from wall clocks.
				lower, ok := auditComponentProportion(pieces["BASE"], amount, transactionAmount, false)
				if !ok {
					return
				}
				actualBase := new(big.Int).Add(base, receivableExpected)
				if actualBase.Cmp(lower) < 0 || actualBase.Cmp(targetBase) > 0 {
					return
				}
				targetBase.Set(actualBase)
			}
			// BASE shortfall is an actual receivable; consumed BONUS is never debt.
			if base.Sign() < 0 || bonus.Sign() < 0 || new(big.Int).Add(base, receivableExpected).Cmp(targetBase) != 0 || bonus.Cmp(targetBonus) > 0 {
				return
			}
			total.Add(walletExpected, receivableExpected)
			// Partial revocation varies per lot; validate source lot and component-kind totals below.
			lotAmounts = nil
		}
		walletExpected.Neg(walletExpected)
		receivableExpected.Neg(receivableExpected)
	}
	if auditString(journals[0], "entry_type") != journalType {
		return
	}
	observedWallet, observedReceivable, observedClearing := new(big.Int), new(big.Int), new(big.Int)
	observedLots := map[string]*big.Int{}
	for _, post := range auditRecords(payload["postings"]) {
		n := auditInteger(post, "delta_microcredits")
		var account reconciliationAuditRecord
		_ = json.Unmarshal(post["account"], &account)
		if n == nil || n.Sign() == 0 {
			return
		}
		accountID := auditString(account, "id")
		lot := auditString(post, "credit_lot_id")
		switch {
		case auditString(account, "account_type") == "WALLET_LIABILITY" && auditString(account, "wallet_id") == wallet:
			if auditString(account, "code") != "wallet:"+wallet {
				return
			}
			if (f.EntryType == "PAYMENT" && n.Sign() < 0) || (f.EntryType != "PAYMENT" && n.Sign() > 0) {
				return
			}
			if lotKinds[lot] == "" {
				return
			}
			observedWallet.Add(observedWallet, n)
			if observedLots[lot] == nil {
				observedLots[lot] = new(big.Int)
			}
			observedLots[lot].Add(observedLots[lot], n)
		case accountID == receivableAccount.String() && auditString(account, "account_type") == "RECEIVABLE_ASSET" && auditString(account, "wallet_id") == "" && lot == "":
			if auditString(account, "code") != "platform:receivables" || n.Sign() != receivableExpected.Sign() {
				return
			}
			observedReceivable.Add(observedReceivable, n)
		case accountID == clearing && auditString(account, "account_type") == clearingType && auditString(account, "wallet_id") == "" && lot == "":
			expectedCode := map[string]string{"PAYMENT_CLEARING": "platform:payment-clearing", "REFUND_CLEARING": "platform:refund-clearing", "CHARGEBACK_CLEARING": "platform:chargeback-clearing"}[clearingType]
			if auditString(account, "code") != expectedCode || (f.EntryType == "PAYMENT" && n.Sign() > 0) || (f.EntryType != "PAYMENT" && n.Sign() < 0) {
				return
			}
			observedClearing.Add(observedClearing, n)
		default:
			return
		}
	}
	expectedClearing := new(big.Int).Set(total)
	if f.EntryType == "PAYMENT" {
		expectedClearing.Neg(expectedClearing)
	}
	if observedWallet.Cmp(walletExpected) != 0 || observedReceivable.Cmp(receivableExpected) != 0 || observedClearing.Cmp(expectedClearing) != 0 {
		return
	}
	if lotAmounts != nil {
		for lot, n := range lotAmounts {
			expected := new(big.Int).Set(n)
			if f.EntryType != "PAYMENT" {
				expected.Neg(expected)
			}
			observed := observedLots[lot]
			if observed == nil {
				observed = new(big.Int)
			}
			if observed.Cmp(expected) != 0 {
				return
			}
		}
	} else {
		var source reconciliationAuditRecord
		_ = json.Unmarshal(f.RefundSnapshot, &source)
		for _, kind := range []string{"BASE", "BONUS"} {
			observed := new(big.Int)
			for lot, n := range observedLots {
				if lotKinds[lot] == kind {
					observed.Sub(observed, n)
				}
			}
			expected := auditInteger(source, "revoked_"+strings.ToLower(kind)+"_microcredits")
			if expected == nil || observed.Cmp(expected) != 0 {
				return
			}
		}
	}
	f.LedgerValid = true
	f.IntegrityCaseType = ""
}

// This proves cumulative disposition and receipt accounting, not historical
// execution order. Application timestamps and UUIDs are not a cross-process
// ordinal. Unknown legacy allocations remain incidents, never new fund actions.
func auditForcedRefundGroup(request reconciliationAuditRequest, payload reconciliationAuditRecord, originalBase, originalBonus, transactionAmount *big.Int) bool {
	return auditForcedRefundGroupComponents(request, payload, map[string][]*big.Int{"BASE": {originalBase}, "BONUS": {originalBonus}}, transactionAmount)
}

func auditForcedRefundGroupComponents(request reconciliationAuditRequest, payload reconciliationAuditRecord, pieces map[string][]*big.Int, transactionAmount *big.Int) bool {
	var mixed bool
	if json.Unmarshal(payload["forced_mixed"], &mixed) != nil || mixed {
		return false
	}
	cash, base, bonus := new(big.Int), new(big.Int), new(big.Int)
	seen := map[string]bool{}
	found := false
	for _, history := range auditRecords(payload["forced_history"]) {
		var source reconciliationAuditRecord
		if json.Unmarshal(history["source"], &source) != nil {
			return false
		}
		id := auditString(source, "id")
		if id == "" || seen[id] || auditString(source, "payment_order_id") != request.Order || auditString(source, "payment_transaction_id") != request.Transaction {
			return false
		}
		seen[id] = true
		version := auditInteger(source, "version")
		if version == nil || version.Sign() <= 0 {
			return false
		}
		journals := auditRecords(history["journals"])
		if len(journals) == 0 {
			if len(auditRecords(history["receivables"])) != 0 || len(auditRecords(history["postings"])) != 0 {
				return false
			}
			var dismissed bool
			_ = json.Unmarshal(history["dismissed"], &dismissed)
			if auditString(source, "receivable_id") != "" || !bytesEqualJSONInteger(source["revoked_base_microcredits"], json.RawMessage(`0`)) || !bytesEqualJSONInteger(source["revoked_bonus_microcredits"], json.RawMessage(`0`)) {
				return false
			}
			if !(version.Cmp(big.NewInt(1)) == 0 && auditString(source, "status") == "REVIEW_REQUIRED") && !(auditString(source, "status") == "RESOLVED" && auditString(source, "resolution_code") == "DISMISS" && dismissed) {
				return false
			}
			continue
		}
		if version.Cmp(big.NewInt(1)) <= 0 || !(auditString(source, "status") == "REVIEW_REQUIRED" || auditString(source, "status") == "RESOLVED" && auditString(source, "resolution_code") == "CONFIRM_FORCED_REFUND") {
			return false
		}
		peerPayload := reconciliationAuditRecord{}
		for key, value := range payload {
			if key != "forced_history" {
				peerPayload[key] = value
			}
		}
		for _, key := range []string{"journals", "postings", "receivables"} {
			peerPayload[key] = history[key]
		}
		peerRequest := request
		peerRequest.Source, peerRequest.References = id, []string{"forced-refund:" + id}
		peer := ReconciliationInternalFact{EntryType: "REFUND", RefundSnapshot: history["source"]}
		auditReconciliationFactWithForcedGroup(&peer, peerRequest, peerPayload, false)
		if !peer.LedgerValid {
			return false
		}
		amount, revokedBase, revokedBonus := auditInteger(source, "amount_fen"), auditInteger(source, "revoked_base_microcredits"), auditInteger(source, "revoked_bonus_microcredits")
		if amount == nil || revokedBase == nil || revokedBonus == nil {
			return false
		}
		cash.Add(cash, amount)
		base.Add(base, revokedBase)
		bonus.Add(bonus, revokedBonus)
		for _, debt := range auditRecords(history["receivables"]) {
			n := auditInteger(debt, "original_microcredits")
			if n == nil {
				return false
			}
			base.Add(base, n)
		}
		if id == request.Source {
			var current reconciliationAuditRecord
			_ = json.Unmarshal(payload["source"], &current)
			for _, key := range []string{"id", "payment_order_id", "payment_transaction_id", "entitlement_package_id", "wallet_id", "amount_fen", "revoked_base_microcredits", "revoked_bonus_microcredits", "receivable_id", "provider_refund_id"} {
				if string(current[key]) != string(source[key]) {
					return false
				}
			}
			found = true
		}
	}
	if !found || cash.Sign() <= 0 || cash.Cmp(transactionAmount) > 0 || !cash.IsInt64() || !transactionAmount.IsInt64() {
		return false
	}
	baseTarget, ok := auditComponentProportion(pieces["BASE"], cash, transactionAmount, true)
	if !ok {
		return false
	}
	bonusTarget, ok := auditComponentProportion(pieces["BONUS"], cash, transactionAmount, true)
	return ok && base.Cmp(baseTarget) == 0 && bonus.Cmp(bonusTarget) <= 0
}

func bytesEqualJSONInteger(a, b json.RawMessage) bool {
	left, ok := new(big.Int).SetString(string(a), 10)
	right, ok2 := new(big.Int).SetString(string(b), 10)
	return ok && ok2 && left.Cmp(right) == 0
}

// Identity includes only financial quantities and source relations. Consumption,
// review versions and clock changes cannot manufacture another incident.
func reconciliationIntegrityDigest(payload reconciliationAuditRecord) string {
	selected := []string{}
	for _, history := range auditRecords(payload["forced_history"]) {
		peer := reconciliationAuditRecord{}
		for key, value := range payload {
			if key != "forced_history" {
				peer[key] = value
			}
		}
		for _, key := range []string{"source", "journals", "postings", "receivables"} {
			peer[key] = history[key]
		}
		var source reconciliationAuditRecord
		_ = json.Unmarshal(history["source"], &source)
		version := auditInteger(source, "version")
		var dismissed bool
		_ = json.Unmarshal(history["dismissed"], &dismissed)
		known := version != nil && ((len(auditRecords(history["journals"])) > 0 && version.Cmp(big.NewInt(1)) > 0 && (auditString(source, "status") == "REVIEW_REQUIRED" || auditString(source, "status") == "RESOLVED" && auditString(source, "resolution_code") == "CONFIRM_FORCED_REFUND")) || (len(auditRecords(history["journals"])) == 0 && (version.Cmp(big.NewInt(1)) == 0 && auditString(source, "status") == "REVIEW_REQUIRED" || auditString(source, "status") == "RESOLVED" && auditString(source, "resolution_code") == "DISMISS" && dismissed)))
		if len(auditRecords(history["journals"])) == 0 && known && auditString(source, "receivable_id") == "" && bytesEqualJSONInteger(source["revoked_base_microcredits"], json.RawMessage(`0`)) && bytesEqualJSONInteger(source["revoked_bonus_microcredits"], json.RawMessage(`0`)) && len(auditRecords(history["receivables"])) == 0 {
			// Recorded but undisposed evidence is not another financial incident.
			continue
		}
		selected = append(selected, fmt.Sprintf("forced-history:%t:%s", known, reconciliationIntegrityDigest(peer)))
	}
	if len(auditRecords(payload["forced_history"])) > 0 {
		selected = append(selected, "forced-mixed:"+string(payload["forced_mixed"]))
	}
	for section, keys := range map[string][]string{"order": {"id", "wallet_id", "requester_id", "beneficiary_organization_id", "amount_fen", "currency", "credit_microcredits", "product_snapshot"}, "transaction": {"id", "payment_order_id", "amount_fen", "currency"}, "source": {"id", "payment_order_id", "payment_transaction_id", "entitlement_package_id", "wallet_id", "amount_fen", "credit_microcredits", "base_microcredits", "bonus_microcredits", "revoked_base_microcredits", "revoked_bonus_microcredits", "provider_refund_id", "provider_chargeback_id", "merchant_refund_no", "channel_refund_id", "receivable_id"}} {
		var record reconciliationAuditRecord
		_ = json.Unmarshal(payload[section], &record)
		entry := map[string]json.RawMessage{}
		for _, key := range keys {
			entry[key] = record[key]
		}
		raw, _ := json.Marshal(entry)
		selected = append(selected, section+":"+string(raw))
	}
	fields := map[string][]string{
		"journals":            {"id", "entry_type", "business_reference"},
		"postings":            {"journal_entry_id", "ledger_account_id", "delta_microcredits", "credit_lot_id"},
		"packages":            {"id", "user_id", "source_type", "wallet_id", "payment_order_id", "source_payment_transaction_id", "amount_fen", "points_total"},
		"components":          {"id", "package_id", "component_type", "credit_lot_id", "original_microcredits"},
		"allocations":         {"receivable_id", "wallet_id", "source_type", "source_reference", "amount_microcredits", "journal_entry_id", "receivable_wallet_id"},
		"receivables":         {"id", "wallet_id", "source_type", "source_reference", "original_microcredits", "payment_order_id", "payment_transaction_id", "entitlement_package_id"},
		"reservations":        {"refund_id", "component_id", "package_id", "component_type", "credit_lot_id", "available_microcredits", "active_grant_microcredits", "suspended_grant_microcredits"},
		"corrections":         {"id", "approval_request_id", "authorization_event_id", "approval_epoch", "execution_key_digest", "package_id", "wallet_id", "payment_order_id", "payment_transaction_id", "source_component_id", "source_lot_id", "target_component_id", "target_lot_id", "source_type", "target_type", "amount_microcredits", "reason_code", "source_evidence_digest", "facts_digest", "journal_entry_id"},
		"correction_journals": {"id", "entry_type", "business_reference"},
		"correction_postings": {"journal_entry_id", "ledger_account_id", "delta_microcredits", "credit_lot_id"},
	}
	for section, keys := range fields {
		for _, record := range auditRecords(payload[section]) {
			entry := map[string]json.RawMessage{}
			for _, key := range keys {
				entry[key] = record[key]
			}
			if section == "postings" || section == "correction_postings" {
				var a reconciliationAuditRecord
				_ = json.Unmarshal(record["account"], &a)
				for _, key := range []string{"id", "wallet_id", "account_type", "code"} {
					entry["account_"+key] = a[key]
				}
			}
			if section == "components" {
				// Keep old, uncorrected incident identities stable.
				if out := auditInteger(record, "reclassified_out_microcredits"); out != nil && out.Sign() != 0 {
					entry["reclassified_out_microcredits"] = record["reclassified_out_microcredits"]
				}
				var l reconciliationAuditRecord
				_ = json.Unmarshal(record["lot"], &l)
				for _, key := range []string{"id", "wallet_id", "source_type", "source_reference", "original_microcredits"} {
					entry["lot_"+key] = l[key]
				}
			}
			raw, _ := json.Marshal(entry)
			selected = append(selected, section+":"+string(raw))
		}
	}
	sort.Strings(selected)
	raw, _ := json.Marshal(selected)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
