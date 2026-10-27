package billing

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func ledgerRepairAuthorityFixture(t *testing.T) (reconciliationAuditRecord, ledgerRepairProofIdentity, ledgerRepairAuthorizationProof) {
	t.Helper()
	_, payload := ledgerRepairReconciliationFixture(t)
	var evidence reconciliationAuditRecord
	_ = json.Unmarshal(payload["ledger_repairs"], &evidence)
	cases := auditRecords(evidence["cases"])
	c := cases[0]
	request := uuid.MustParse(auditString(c, "approval_request_id"))
	event := uuid.MustParse(auditString(c, "authorization_event_id"))
	id := ledgerRepairProofIdentity{ApprovalRequestID: request, AuthorizationEventID: event, ApprovalEpoch: 1}
	journals := auditRecords(payload["journals"])
	journal := journals[0]
	targetID := uuid.MustParse(auditString(c, "target_posting_id"))
	journalID := uuid.MustParse(auditString(c, "target_journal_entry_id"))
	var post reconciliationAuditRecord
	for _, candidate := range auditRecords(payload["postings"]) {
		if auditString(candidate, "id") == targetID.String() {
			post = candidate
		}
	}
	if post == nil {
		t.Fatal("fixture target missing")
	}
	input := LedgerRepairCommandInput{TargetJournalEntryID: journalID, TargetPostingID: targetID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: paymentClearingAccount, AmountMicrocredits: 100}
	plan := LedgerRepairPlan{TargetJournalEntryID: journalID, TargetPostingID: targetID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: paymentClearingAccount, AmountMicrocredits: 100, EntryType: "RECHARGE", WrongLedgerAccountType: "ADJUSTMENT", CorrectLedgerAccountType: "PAYMENT_CLEARING", PostingSetDigest: auditString(c, "posting_set_digest")}
	critical, financial := ledgerRepairProofProjection(plan)
	normalized, _ := json.Marshal(map[string]any{"target_journal_entry_id": journalID, "target_posting_id": targetID, "wrong_ledger_account_id": adjustmentAccount, "correct_ledger_account_id": paymentClearingAccount, "amount_microcredits": 100, "reason_code": "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT"})
	frozen := approvalencoding.LegacyProofInput{CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "LEDGER_POSTING", TargetResourceID: targetID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(frozen)
	if err != nil {
		t.Fatal(err)
	}
	key := ledgerRepairExecutionKeyDigest("controlled-immutable-key")
	var wire struct {
		NormalizedCommand        json.RawMessage `json:"normalized_command"`
		CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
		FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
	}
	if err := json.Unmarshal(artifacts.ProofInputJSON, &wire); err != nil {
		t.Fatal(err)
	}
	proof := ledgerRepairAuthorizationProof{ledgerRepairProofIdentity: id, Status: "VERIFIED", CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, ExecutionKeyDigest: key, CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, DomainRevision: billingProofRevision(ledgerRepairCommand, 2, frozen, "RECHARGE:ADJUSTMENT->PAYMENT_CLEARING"), NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot}
	// Version 2 authorized the claim; a later disable/re-enable advanced the
	// execution to version 4. Both are valid historical immutable states.
	target := ledgerRepairTarget{JournalEntryID: journalID, EntryType: "RECHARGE", BusinessReference: auditString(journal, "business_reference")}
	posting := ledgerRepairPosting{ID: targetID, LedgerAccountID: adjustmentAccount, AccountType: "ADJUSTMENT", Delta: auditInteger(post, "delta_microcredits").Int64()}
	correct := ledgerRepairAccount{ID: paymentClearingAccount, Code: "platform:payment-clearing", AccountType: "PAYMENT_CLEARING"}
	factsDigest := ledgerRepairFactsDigest(target, posting, correct, input, 1, 1, 4, plan.PostingSetDigest)
	c["execution_key_digest"], _ = json.Marshal(key)
	c["facts_digest"], _ = json.Marshal(factsDigest)
	c["approval_claim"], _ = json.Marshal(map[string]any{"approval_request_id": request, "authorization_event_id": event, "approval_epoch": 1, "command_type": ledgerRepairCommand, "command_digest": artifacts.CommandDigest, "snapshot_digest": artifacts.SnapshotDigest, "domain_revision": proof.DomainRevision})
	evidence["cases"], _ = json.Marshal(cases)
	evidence["activation_history"], _ = json.Marshal([]any{map[string]any{"policy_version": 1, "snapshot_schema_version": 1, "state": "ACTIVE", "version": 2}, map[string]any{"policy_version": 1, "snapshot_schema_version": 1, "state": "ACTIVE", "version": 4}})
	payload["ledger_repairs"], _ = json.Marshal(evidence)
	return payload, id, proof
}

func TestLedgerRepairAuthorityBindsHistoricalFacts(t *testing.T) {
	payload, id, proof := ledgerRepairAuthorityFixture(t)
	proofs := map[ledgerRepairProofIdentity]ledgerRepairAuthorizationProof{id: proof}
	if ok, err := validateLedgerRepairAuthority(payload, proofs); err != nil || !ok {
		t.Fatal("legitimate distinct claim and execution versions rejected", err)
	}
	if ok, err := validateLedgerRepairAuthority(payload, nil); ok || !errors.Is(err, ErrLedgerRepairAuthorityUnavailable) {
		t.Fatal("missing authority was not inconclusive", err)
	}
	for _, field := range []string{"facts_digest", "execution_key_digest", "authorization_event_id"} {
		t.Run(field, func(t *testing.T) {
			var evidence reconciliationAuditRecord
			_ = json.Unmarshal(payload["ledger_repairs"], &evidence)
			cases := auditRecords(evidence["cases"])
			if field == "authorization_event_id" {
				cases[0][field], _ = json.Marshal(uuid.NewString())
			} else {
				cases[0][field], _ = json.Marshal(strings.Repeat("e", 64))
			}
			evidence["cases"], _ = json.Marshal(cases)
			clone := make(reconciliationAuditRecord, len(payload))
			for k, v := range payload {
				clone[k] = v
			}
			clone["ledger_repairs"], _ = json.Marshal(evidence)
			ok, err := validateLedgerRepairAuthority(clone, proofs)
			if ok {
				t.Fatal("well-formed corrupted receipt accepted", field)
			}
			if field != "authorization_event_id" && err != nil {
				t.Fatal("established receipt mismatch was treated as an outage", field, err)
			}
		})
	}
	badProof := proof
	badProof.CriticalResourceVersions = json.RawMessage(`{}`)
	if ok, err := validateLedgerRepairAuthority(payload, map[ledgerRepairProofIdentity]ledgerRepairAuthorizationProof{id: badProof}); ok || !errors.Is(err, ErrLedgerRepairAuthorityUnavailable) {
		t.Fatal("malformed Account proof was not classified as unavailable", err)
	}
	badProof = proof
	badProof.CommandType = "UNTRUSTED"
	if ok, err := validateLedgerRepairAuthority(payload, map[ledgerRepairProofIdentity]ledgerRepairAuthorizationProof{id: badProof}); ok || !errors.Is(err, ErrLedgerRepairAuthorityUnavailable) {
		t.Fatal("unexpected authority command was not classified as unavailable", err)
	}
	badProof = proof
	badProof.Status = "UNAVAILABLE"
	if ok, err := validateLedgerRepairAuthority(payload, map[ledgerRepairProofIdentity]ledgerRepairAuthorizationProof{id: badProof}); ok || !errors.Is(err, ErrLedgerRepairAuthorityUnavailable) {
		t.Fatal("unavailable authority misclassified", err)
	}
}

func TestLedgerRepairAuthorityWireDecimalBoundaries(t *testing.T) {
	_, _, base := ledgerRepairAuthorityFixture(t)
	var command map[string]json.RawMessage
	if json.Unmarshal(base.NormalizedCommand, &command) != nil {
		t.Fatal("fixture")
	}
	var target string
	if json.Unmarshal(command["target_posting_id"], &target) != nil {
		t.Fatal("fixture target")
	}
	for _, bad := range []string{`100`, `"1e2"`, `"0100"`, `"+100"`, `"9223372036854775808"`, `null`} {
		t.Run("reject-"+bad, func(t *testing.T) {
			var changed map[string]json.RawMessage
			_ = json.Unmarshal(base.NormalizedCommand, &changed)
			changed["amount_microcredits"] = json.RawMessage(bad)
			p := base
			p.NormalizedCommand, _ = json.Marshal(changed)
			if _, _, err := decodeLedgerRepairAuthorityProof(p, target); err == nil {
				t.Fatal("invalid decimal wire accepted", bad)
			}
		})
	}
	_, legacy, err := decodeLedgerRepairAuthorityProof(base, target)
	if err != nil {
		t.Fatal(err)
	}
	for _, amount := range []string{"9007199254740993", "9223372036854775807"} {
		t.Run("exact-"+amount, func(t *testing.T) {
			var normalized, financial map[string]json.RawMessage
			_ = json.Unmarshal(legacy.NormalizedCommand, &normalized)
			_ = json.Unmarshal(legacy.FinancialSnapshot, &financial)
			normalized["amount_microcredits"] = json.RawMessage(amount)
			financial["amount_microcredits"] = json.RawMessage(amount)
			input := legacy
			input.NormalizedCommand, _ = json.Marshal(normalized)
			input.FinancialSnapshot, _ = json.Marshal(financial)
			artifacts, err := approvalencoding.ConvertLegacyProofInput(input)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				NormalizedCommand        json.RawMessage `json:"normalized_command"`
				CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
				FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
			}
			if json.Unmarshal(artifacts.ProofInputJSON, &wire) != nil {
				t.Fatal("wire")
			}
			proof := base
			proof.NormalizedCommand, proof.CriticalResourceVersions, proof.FinancialSnapshot = wire.NormalizedCommand, wire.CriticalResourceVersions, wire.FinancialSnapshot
			proof.CommandDigest, proof.SnapshotDigest = artifacts.CommandDigest, artifacts.SnapshotDigest
			_, decoded, err := decodeLedgerRepairAuthorityProof(proof, target)
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]json.RawMessage
			if json.Unmarshal(decoded.NormalizedCommand, &result) != nil || string(result["amount_microcredits"]) != amount {
				t.Fatal("decimal amount changed", string(result["amount_microcredits"]))
			}
		})
	}
}

func TestLedgerRepairAuthorityMixedFactsRemainIndependent(t *testing.T) {
	request, _ := ledgerRepairReconciliationFixture(t)
	payload, id, proof := ledgerRepairAuthorityFixture(t)
	// Derive the request from this exact fixture rather than matching unrelated
	// payment identities from another generated fixture.
	var order reconciliationAuditRecord
	if json.Unmarshal(payload["order"], &order) != nil {
		t.Fatal("order")
	}
	request.Order = auditString(order, "id")
	var tx reconciliationAuditRecord
	if json.Unmarshal(payload["transaction"], &tx) != nil {
		t.Fatal("transaction")
	}
	request.Transaction = auditString(tx, "id")
	journal := auditRecords(payload["journals"])[0]
	request.References = []string{auditString(journal, "business_reference")}
	clean := make(reconciliationAuditRecord, len(payload))
	for k, v := range payload {
		clean[k] = v
	}
	clean["ledger_repairs"] = json.RawMessage(`{"cases":[],"journals":[],"postings":[]}`)
	// Removing the repair from a wrong-attribution fixture makes it invalid but
	// must not rewrite its existing digest or manufacture an authority case.
	unrelated := ReconciliationInternalFact{EntryType: "PAYMENT"}
	auditReconciliationFact(&unrelated, request, clean)
	before := unrelated.IntegrityDigest
	valid := ReconciliationInternalFact{EntryType: "PAYMENT"}
	auditReconciliationFact(&valid, request, payload)
	if !valid.LedgerValid {
		t.Fatal("fixture is not structurally valid")
	}
	facts := []ReconciliationInternalFact{valid, unrelated}
	applyLedgerRepairAuthority(facts, nil)
	if facts[0].LedgerValid || facts[0].IntegrityCaseType != ledgerRepairProofUnavailableCase {
		t.Fatal("unavailable proof was accepted")
	}
	if facts[1].IntegrityDigest != before || facts[1].IntegrityCaseType == ledgerRepairProofUnavailableCase {
		t.Fatal("unrelated no-repair fact changed")
	}
	verified := []ReconciliationInternalFact{valid}
	applyLedgerRepairAuthority(verified, ledgerRepairProofSet{id: proof})
	if !verified[0].LedgerValid || verified[0].IntegrityCaseType != "" || verified[0].IntegrityDigest == facts[0].IntegrityDigest {
		t.Fatal("recovered proof did not change observation identity")
	}
}

func TestLedgerRepairAuthorityUnavailablePreservesExistingInvalidDifference(t *testing.T) {
	request, _ := ledgerRepairReconciliationFixture(t)
	payload, _, _ := ledgerRepairAuthorityFixture(t)
	var order reconciliationAuditRecord
	_ = json.Unmarshal(payload["order"], &order)
	request.Order = auditString(order, "id")
	var transaction reconciliationAuditRecord
	_ = json.Unmarshal(payload["transaction"], &transaction)
	request.Transaction = auditString(transaction, "id")
	request.References = []string{auditString(auditRecords(payload["journals"])[0], "business_reference")}
	order["wallet_id"], _ = json.Marshal(uuid.NewString())
	payload["order"], _ = json.Marshal(order)
	fact := ReconciliationInternalFact{EntryType: "PAYMENT", ProviderReferenceID: "controlled", MerchantOrderNo: "controlled"}
	auditReconciliationFact(&fact, request, payload)
	if fact.IntegrityCaseType != "PAYMENT_PACKAGE_INVALID" {
		t.Fatal("fixture did not expose original financial defect", fact.IntegrityCaseType)
	}
	facts := []ReconciliationInternalFact{fact}
	applyLedgerRepairAuthority(facts, nil)
	differences := CompareFourWayReconciliation(nil, facts)
	seen := map[string]bool{}
	for _, difference := range differences {
		seen[difference.CaseType] = true
	}
	if !seen["PAYMENT_PACKAGE_INVALID"] || !seen[ledgerRepairProofUnavailableCase] {
		t.Fatal("financial defect or separate unavailable proof hidden", seen)
	}
}

func TestLedgerRepairAuthorityInvalidPreservesExistingFinancialDifference(t *testing.T) {
	request, _ := ledgerRepairReconciliationFixture(t)
	payload, id, proof := ledgerRepairAuthorityFixture(t)
	var order, evidence reconciliationAuditRecord
	if err := json.Unmarshal(payload["order"], &order); err != nil {
		t.Fatal(err)
	}
	request.Order = auditString(order, "id")
	var tx reconciliationAuditRecord
	if err := json.Unmarshal(payload["transaction"], &tx); err != nil {
		t.Fatal(err)
	}
	request.Transaction = auditString(tx, "id")
	request.References = []string{auditString(auditRecords(payload["journals"])[0], "business_reference")}
	order["wallet_id"], _ = json.Marshal(uuid.NewString())
	payload["order"], _ = json.Marshal(order)
	if err := json.Unmarshal(payload["ledger_repairs"], &evidence); err != nil {
		t.Fatal(err)
	}
	cases := auditRecords(evidence["cases"])
	cases[0]["facts_digest"], _ = json.Marshal(strings.Repeat("e", 64))
	evidence["cases"], _ = json.Marshal(cases)
	payload["ledger_repairs"], _ = json.Marshal(evidence)
	proofs := ledgerRepairProofSet{id: proof}
	if valid, err := validateLedgerRepairAuthority(payload, proofs); valid || err != nil {
		t.Fatalf("expected verified authority mismatch, valid=%v err=%v", valid, err)
	}
	fact := ReconciliationInternalFact{EntryType: "PAYMENT", ProviderReferenceID: "controlled", MerchantOrderNo: "controlled"}
	auditReconciliationFact(&fact, request, payload)
	if fact.IntegrityCaseType != "PAYMENT_PACKAGE_INVALID" {
		t.Fatal("fixture did not expose original financial defect", fact.IntegrityCaseType)
	}
	facts := []ReconciliationInternalFact{fact}
	applyLedgerRepairAuthority(facts, proofs)
	seen := map[string]int{}
	for _, difference := range CompareFourWayReconciliation(nil, facts) {
		seen[difference.CaseType]++
	}
	if seen["PAYMENT_PACKAGE_INVALID"] != 1 || seen["PAYMENT_LEDGER_INVALID"] != 1 || seen[ledgerRepairProofUnavailableCase] != 0 {
		t.Fatal("financial defect or separate verified mismatch hidden or duplicated", seen)
	}
}

// Exercise classification over two independently identified payloads in the
// same shape as the root/forced_history observations emitted by the query.
// The individual journal fixtures are synthetic and are not an end-to-end
// forced-refund scenario.
func TestLedgerRepairAuthorityCombinedDifferencesAndDeduplication(t *testing.T) {
	invalidPayload, id, proof := ledgerRepairAuthorityFixture(t)
	missingPayload, missingID, _ := ledgerRepairAuthorityFixture(t)
	var evidence reconciliationAuditRecord
	if err := json.Unmarshal(invalidPayload["ledger_repairs"], &evidence); err != nil {
		t.Fatal(err)
	}
	cases := auditRecords(evidence["cases"])
	cases[0]["facts_digest"], _ = json.Marshal(strings.Repeat("e", 64))
	evidence["cases"], _ = json.Marshal(cases)
	invalidPayload["ledger_repairs"], _ = json.Marshal(evidence)
	proofs := ledgerRepairProofSet{id: proof}
	if valid, err := validateLedgerRepairAuthority(invalidPayload, proofs); valid || err != nil {
		t.Fatalf("expected verified mismatch: valid=%v err=%v", valid, err)
	}
	if _, ok := proofs[missingID]; ok {
		t.Fatal("unavailable payload unexpectedly shares proof identity")
	}
	if valid, err := validateLedgerRepairAuthority(missingPayload, proofs); valid || !errors.Is(err, ErrLedgerRepairAuthorityUnavailable) {
		t.Fatalf("expected unavailable authority: valid=%v err=%v", valid, err)
	}
	peer := reconciliationAuditRecord{}
	for _, key := range []string{"journals", "postings", "receivables", "ledger_repairs"} {
		peer[key] = missingPayload[key]
	}
	invalidPayload["forced_history"], _ = json.Marshal([]reconciliationAuditRecord{peer})
	snapshot, err := json.Marshal(invalidPayload)
	if err != nil {
		t.Fatal(err)
	}
	for _, initial := range []string{"PAYMENT_PACKAGE_INVALID", "PAYMENT_LEDGER_INVALID"} {
		t.Run(initial, func(t *testing.T) {
			facts := []ReconciliationInternalFact{{EntryType: "PAYMENT", MerchantOrderNo: "controlled", ProviderReferenceID: "controlled", LedgerPresent: true, LedgerSnapshot: snapshot, IntegrityCaseType: initial}}
			applyLedgerRepairAuthority(facts, proofs)
			seen := map[string]int{}
			for _, difference := range CompareFourWayReconciliation(nil, facts) {
				seen[difference.CaseType]++
			}
			if seen[initial] != 1 || seen["PAYMENT_LEDGER_INVALID"] != 1 || seen[ledgerRepairProofUnavailableCase] != 1 {
				t.Fatal("combined differences missing or duplicated", seen)
			}
		})
	}
}

// Forced-refund evidence is identified within a payment transaction, but
// reconciliation keys omit the order. Distinct orders can therefore have the
// same provider refund reference while carrying different repair evidence.
func TestLedgerRepairAuthorityDuplicateInternalReferenceKeepsOrderEvidence(t *testing.T) {
	validPayload, id, proof := ledgerRepairAuthorityFixture(t)
	missingPayload, missingID, missingProof := ledgerRepairAuthorityFixture(t)
	if id == missingID {
		t.Fatal("fixture repair identities must differ")
	}
	validRaw, _ := json.Marshal(validPayload)
	missingRaw, _ := json.Marshal(missingPayload)
	proofs := ledgerRepairProofSet{id: proof}
	makeFact := func(order string, snapshot json.RawMessage) ReconciliationInternalFact {
		return ReconciliationInternalFact{EntryType: "REFUND", ProviderReferenceID: "same-refund", MerchantOrderNo: order, PaymentOrderID: uuid.New(), AmountFen: 100, Currency: "CNY", EventDateVerified: true, LedgerPresent: true, LedgerValid: true, LedgerSnapshot: snapshot}
	}
	first := makeFact("order-valid", validRaw)
	second := makeFact("order-missing", missingRaw)
	provider := []ProviderBillEntry{{EntryType: "REFUND", ProviderReferenceID: "same-refund", MerchantOrderNo: "order-valid", AmountFen: 100, Currency: "CNY", Status: "SUCCESS"}}
	for _, reverse := range []bool{false, true} {
		for _, available := range []bool{false, true} {
			t.Run(fmt.Sprintf("reverse=%v/available=%v", reverse, available), func(t *testing.T) {
				facts := []ReconciliationInternalFact{first, second}
				caseProofs := proofs
				if available {
					caseProofs = ledgerRepairProofSet{id: proof, missingID: missingProof}
				}
				if reverse {
					facts[0], facts[1] = facts[1], facts[0]
				}
				applyLedgerRepairAuthority(facts, caseProofs)
				seen := map[string]map[string]int{}
				for _, difference := range CompareFourWayReconciliation(provider, facts) {
					if seen[difference.MerchantOrderNo] == nil {
						seen[difference.MerchantOrderNo] = map[string]int{}
					}
					seen[difference.MerchantOrderNo][difference.CaseType]++
				}
				wantUnavailable := 1
				if available {
					wantUnavailable = 0
				}
				if seen["order-missing"][ledgerRepairProofUnavailableCase] != wantUnavailable || seen["order-valid"][ledgerRepairProofUnavailableCase] != 0 {
					t.Fatal("order-specific missing proof lost", seen)
				}
				duplicateOrder := "order-missing"
				if reverse {
					duplicateOrder = "order-valid"
				}
				if seen[duplicateOrder]["DUPLICATE_TRANSACTION"] != 1 {
					t.Fatal("duplicate transaction lost", seen)
				}
				wantUnmatched := 0
				if reverse {
					wantUnmatched = 1
				}
				if seen["order-valid"]["PROVIDER_REFUND_INTERNAL_MISSING"] != wantUnmatched || seen["order-missing"]["INTERNAL_REFUND_PROVIDER_MISSING"] != wantUnmatched {
					t.Fatal("provider matching changed", seen)
				}
			})
		}
	}
	t.Run("duplicate retains related financial integrity observation", func(t *testing.T) {
		duplicate := second
		duplicate.IntegrityCaseType = "REFUND_PACKAGE_INVALID"
		duplicate.LedgerValid = false
		duplicate.EventDateConflict = true
		duplicate.LedgerPresent = false
		facts := []ReconciliationInternalFact{first, duplicate}
		applyLedgerRepairAuthority(facts, proofs)
		seen := map[string]int{}
		for _, d := range CompareFourWayReconciliation(provider, facts) {
			if d.MerchantOrderNo == duplicate.MerchantOrderNo {
				seen[d.CaseType]++
			}
		}
		for _, caseType := range []string{"DUPLICATE_TRANSACTION", "REFUND_PACKAGE_INVALID", ledgerRepairProofUnavailableCase} {
			if seen[caseType] != 1 {
				t.Fatal("duplicate lost or repeated its own observation", caseType, seen)
			}
		}
		if seen["REFUND_DATE_UNVERIFIED"] != 0 || seen["REFUND_LEDGER_MISSING"] != 0 {
			t.Fatal("unrelated duplicate classifications changed", seen)
		}
	})
}

func TestLedgerRepairDuplicateNoRepairRetainsLegacyObservations(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			facts := []ReconciliationInternalFact{
				{EntryType: "REFUND", ProviderReferenceID: "same", MerchantOrderNo: "first", PaymentOrderID: uuid.New(), EventDateConflict: true, IntegrityCaseType: "REFUND_PACKAGE_INVALID", LedgerPresent: false, IntegrityDigest: "first-original", LedgerSnapshot: json.RawMessage(`{"ledger_repairs":{"cases":[]}}`)},
				{EntryType: "REFUND", ProviderReferenceID: "same", MerchantOrderNo: "second", PaymentOrderID: uuid.New(), EventDateConflict: true, IntegrityCaseType: "REFUND_LEDGER_INVALID", LedgerPresent: false, IntegrityDigest: "second-original", LedgerSnapshot: json.RawMessage(`{"ledger_repairs":{"cases":[]}}`)},
			}
			if reverse {
				facts[0], facts[1] = facts[1], facts[0]
			}
			// Baseline: compare the first record normally, then retain only the
			// pre-existing duplicate difference for the second record.
			want := CompareFourWayReconciliation(nil, facts[:1])
			want = append(want, differenceFromFact(facts[1], "DUPLICATE_TRANSACTION", "P0", nil))
			applyLedgerRepairAuthority(facts, nil)
			got := CompareFourWayReconciliation(nil, facts)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("no-repair duplicate observation changed: got=%+v want=%+v", got, want)
			}
		})
	}
}

func TestLedgerRepairDuplicateVerifiedPreservesFinancialInvalidity(t *testing.T) {
	request, _ := ledgerRepairReconciliationFixture(t)
	payload, id, proof := ledgerRepairAuthorityFixture(t)
	var order, transaction reconciliationAuditRecord
	if err := json.Unmarshal(payload["order"], &order); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload["transaction"], &transaction); err != nil {
		t.Fatal(err)
	}
	request.Order = auditString(order, "id")
	request.Transaction = auditString(transaction, "id")
	request.References = []string{auditString(auditRecords(payload["journals"])[0], "business_reference")}
	order["wallet_id"], _ = json.Marshal(uuid.NewString())
	payload["order"], _ = json.Marshal(order)
	proofs := ledgerRepairProofSet{id: proof}
	if valid, err := validateLedgerRepairAuthority(payload, proofs); !valid || err != nil {
		t.Fatalf("expected verified repair authority: valid=%v err=%v", valid, err)
	}
	invalid := ReconciliationInternalFact{EntryType: "REFUND", ProviderReferenceID: "same", MerchantOrderNo: "invalid", PaymentOrderID: uuid.New(), EventDateVerified: true}
	auditReconciliationFact(&invalid, request, payload)
	if invalid.IntegrityCaseType != "REFUND_LEDGER_INVALID" {
		t.Fatalf("expected financial invalidity, got %q", invalid.IntegrityCaseType)
	}
	clean := ReconciliationInternalFact{EntryType: "REFUND", ProviderReferenceID: "same", MerchantOrderNo: "other", PaymentOrderID: uuid.New(), EventDateVerified: true, LedgerPresent: true, LedgerValid: true}
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			facts := []ReconciliationInternalFact{clean, invalid}
			if reverse {
				facts[0], facts[1] = facts[1], facts[0]
			}
			applyLedgerRepairAuthority(facts, proofs)
			seen := map[string]map[string]int{}
			for _, d := range CompareFourWayReconciliation(nil, facts) {
				if seen[d.MerchantOrderNo] == nil {
					seen[d.MerchantOrderNo] = map[string]int{}
				}
				seen[d.MerchantOrderNo][d.CaseType]++
			}
			if seen["invalid"]["REFUND_LEDGER_INVALID"] != 1 || seen["invalid"][ledgerRepairProofUnavailableCase] != 0 || seen["other"]["REFUND_LEDGER_INVALID"] != 0 {
				t.Fatalf("verified repair hid financial invalidity: %v", seen)
			}
		})
	}
}
