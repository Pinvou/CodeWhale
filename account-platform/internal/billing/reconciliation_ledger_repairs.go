package billing

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"time"

	"github.com/google/uuid"
)

// reconciliationLedgerRepairPostings verifies immutable evidence before deriving
// an in-memory attribution. The supplied raw evidence is never altered.
func reconciliationLedgerRepairPostings(payload reconciliationAuditRecord) ([]reconciliationAuditRecord, bool) {
	original := auditRecords(payload["postings"])
	var evidence reconciliationAuditRecord
	if len(payload["ledger_repairs"]) == 0 || string(payload["ledger_repairs"]) == "null" {
		return original, true
	}
	if json.Unmarshal(payload["ledger_repairs"], &evidence) != nil {
		return nil, false
	}
	cases, reversals, legs := auditRecords(evidence["cases"]), auditRecords(evidence["journals"]), auditRecords(evidence["postings"])
	if len(cases) == 0 && len(reversals) == 0 && len(legs) == 0 {
		return original, true
	}
	journals := auditRecords(payload["journals"])
	if len(journals) != 1 || len(cases) == 0 || len(reversals) != len(cases) || len(legs) != 2*len(cases) {
		return nil, false
	}
	journalID, ok := reconciliationRepairID(auditString(journals[0], "id"))
	if !ok {
		return nil, false
	}
	createdAt, err := time.Parse(time.RFC3339Nano, auditString(journals[0], "created_at"))
	if err != nil {
		return nil, false
	}
	target := ledgerRepairTarget{JournalEntryID: journalID, EntryType: auditString(journals[0], "entry_type"), BusinessReference: auditString(journals[0], "business_reference"), CreatedAt: createdAt}
	positions := make(map[string]int, len(original))
	for i, post := range original {
		id, valid := reconciliationRepairID(auditString(post, "id"))
		if !valid {
			return nil, false
		}
		accountID, valid := reconciliationRepairID(auditString(post, "ledger_account_id"))
		if !valid || auditString(post, "journal_entry_id") != journalID.String() {
			return nil, false
		}
		if _, exists := positions[id.String()]; exists {
			return nil, false
		}
		positions[id.String()] = i
		amount := auditInteger(post, "delta_microcredits")
		if amount == nil || !amount.IsInt64() || amount.Sign() == 0 || amount.Int64() == math.MinInt64 {
			return nil, false
		}
		var account reconciliationAuditRecord
		if json.Unmarshal(post["account"], &account) != nil || auditString(account, "id") != accountID.String() {
			return nil, false
		}
		var wallet, lot *uuid.UUID
		if raw := auditString(account, "wallet_id"); raw != "" {
			value, valid := reconciliationRepairID(raw)
			if !valid {
				return nil, false
			}
			wallet = &value
		}
		if raw := auditString(post, "credit_lot_id"); raw != "" {
			value, valid := reconciliationRepairID(raw)
			if !valid {
				return nil, false
			}
			lot = &value
		}
		target.Postings = append(target.Postings, ledgerRepairPosting{ID: id, LedgerAccountID: accountID, AccountType: auditString(account, "account_type"), WalletID: wallet, Delta: amount.Int64(), CreditLotID: lot})
	}
	digest := ledgerRepairPostingSetDigest(target)
	reversalByID := map[string]reconciliationAuditRecord{}
	for _, journal := range reversals {
		id, valid := reconciliationRepairID(auditString(journal, "id"))
		if !valid || reversalByID[id.String()] != nil {
			return nil, false
		}
		reversalByID[id.String()] = journal
	}
	legByJournal := map[string][]reconciliationAuditRecord{}
	seenLegs := map[string]bool{}
	for _, leg := range legs {
		id, valid := reconciliationRepairID(auditString(leg, "id"))
		if !valid || seenLegs[id.String()] {
			return nil, false
		}
		seenLegs[id.String()] = true
		journal := auditString(leg, "journal_entry_id")
		if reversalByID[journal] == nil {
			return nil, false
		}
		legByJournal[journal] = append(legByJournal[journal], leg)
	}
	effective := append([]reconciliationAuditRecord(nil), original...)
	seenTargets, seenCases, seenReversals := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range cases {
		caseID, valid := reconciliationRepairID(auditString(c, "id"))
		if !valid || seenCases[caseID.String()] {
			return nil, false
		}
		seenCases[caseID.String()] = true
		for _, key := range []string{"approval_request_id", "authorization_event_id"} {
			if _, valid = reconciliationRepairID(auditString(c, key)); !valid {
				return nil, false
			}
		}
		epoch := auditInteger(c, "approval_epoch")
		if epoch == nil || epoch.Sign() <= 0 {
			return nil, false
		}
		for _, key := range []string{"execution_key_digest", "facts_digest", "posting_set_digest"} {
			if !reconciliationRepairDigest(auditString(c, key)) {
				return nil, false
			}
		}
		postID := auditString(c, "target_posting_id")
		index, exists := positions[postID]
		if !exists || seenTargets[postID] {
			return nil, false
		}
		seenTargets[postID] = true
		post := target.Postings[index]
		if auditString(c, "state") != "EXECUTED" || auditString(c, "target_journal_entry_id") != journalID.String() || auditString(c, "entry_type") != target.EntryType || auditString(c, "posting_set_digest") != digest || auditString(c, "wrong_ledger_account_id") != post.LedgerAccountID.String() || post.WalletID != nil || post.CreditLotID != nil {
			return nil, false
		}
		amount := auditInteger(c, "amount_microcredits")
		if amount == nil || !amount.IsInt64() || amount.Int64() != absInt64(post.Delta) {
			return nil, false
		}
		var wrong, correct reconciliationAuditRecord
		if json.Unmarshal(c["wrong_account"], &wrong) != nil || json.Unmarshal(c["correct_account"], &correct) != nil || !reconciliationRepairSystemAccount(wrong) || !reconciliationRepairSystemAccount(correct) {
			return nil, false
		}
		if auditString(wrong, "id") != post.LedgerAccountID.String() || auditString(correct, "id") != auditString(c, "correct_ledger_account_id") || auditString(wrong, "id") == auditString(correct, "id") {
			return nil, false
		}
		var originalAccount reconciliationAuditRecord
		if json.Unmarshal(original[index]["account"], &originalAccount) != nil || !reconciliationRepairSameAccount(wrong, originalAccount) {
			return nil, false
		}
		reversalID := auditString(c, "journal_entry_id")
		reversal := reversalByID[reversalID]
		if reversal == nil || seenReversals[reversalID] || auditString(reversal, "entry_type") != "REVERSAL" || auditString(reversal, "business_reference") != "ledger-repair:"+postID {
			return nil, false
		}
		seenReversals[reversalID] = true
		pair := legByJournal[reversalID]
		if len(pair) != 2 {
			return nil, false
		}
		foundWrong, foundCorrect := false, false
		for _, leg := range pair {
			var a reconciliationAuditRecord
			if json.Unmarshal(leg["account"], &a) != nil || auditString(leg, "credit_lot_id") != "" || auditString(leg, "ledger_account_id") != auditString(a, "id") {
				return nil, false
			}
			delta := auditInteger(leg, "delta_microcredits")
			if delta == nil || !delta.IsInt64() {
				return nil, false
			}
			switch {
			case reconciliationRepairSameAccount(a, wrong) && delta.Int64() == -post.Delta && !foundWrong:
				foundWrong = true
			case reconciliationRepairSameAccount(a, correct) && delta.Int64() == post.Delta && !foundCorrect:
				foundCorrect = true
			default:
				return nil, false
			}
		}
		if !foundWrong || !foundCorrect {
			return nil, false
		}
		clone := make(reconciliationAuditRecord, len(original[index]))
		for key, value := range original[index] {
			clone[key] = value
		}
		clone["account"] = c["correct_account"]
		clone["ledger_account_id"] = c["correct_ledger_account_id"]
		effective[index] = clone
	}
	return effective, true
}

func reconciliationRepairID(value string) (uuid.UUID, bool) {
	id, err := uuid.Parse(value)
	return id, err == nil && id != uuid.Nil && id.String() == value
}

func reconciliationRepairDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == value
}

func reconciliationRepairSystemAccount(a reconciliationAuditRecord) bool {
	id, ok := reconciliationRepairID(auditString(a, "id"))
	if !ok || auditString(a, "wallet_id") != "" {
		return false
	}
	for _, expected := range systemLedgerAccountExpectations {
		if id == expected.ID && auditString(a, "code") == expected.Code && auditString(a, "account_type") == expected.AccountType {
			return true
		}
	}
	return false
}

func reconciliationRepairSameAccount(a, b reconciliationAuditRecord) bool {
	return auditString(a, "id") == auditString(b, "id") && auditString(a, "code") == auditString(b, "code") && auditString(a, "account_type") == auditString(b, "account_type") && auditString(a, "wallet_id") == auditString(b, "wallet_id")
}

// journalFilter is a fixed SQL fragment owned by the caller, never user input.
// Both sides of the association are searched so malformed links and orphan
// reversal references remain visible to the verifier rather than disappearing.
func reconciliationLedgerRepairEvidenceSQL(journalFilter string) string {
	return `'ledger_repairs',(WITH source_journals AS (
 SELECT root.id FROM journal_entries root WHERE ` + journalFilter + `
), source_postings AS (
 SELECT p.id,p.journal_entry_id FROM ledger_postings p JOIN source_journals j ON j.id=p.journal_entry_id
), relevant_cases AS (
 SELECT c.* FROM ledger_repair_cases c WHERE c.target_journal_entry_id IN (SELECT id FROM source_journals) OR c.target_posting_id IN (SELECT id FROM source_postings)
), relevant_journals AS (
 SELECT j.* FROM journal_entries j WHERE j.id IN (SELECT journal_entry_id FROM relevant_cases) OR j.business_reference IN (SELECT 'ledger-repair:'||id::text FROM source_postings)
)
SELECT jsonb_build_object(
 'cases',COALESCE((SELECT jsonb_agg(to_jsonb(c)||jsonb_build_object('wrong_account',to_jsonb(w),'correct_account',to_jsonb(a),'approval_claim',to_jsonb(cl))) FROM relevant_cases c LEFT JOIN ledger_accounts w ON w.id=c.wrong_ledger_account_id LEFT JOIN ledger_accounts a ON a.id=c.correct_ledger_account_id LEFT JOIN billing_approval_proof_claims cl ON cl.approval_request_id=c.approval_request_id),'[]'),
 'activation_history',COALESCE((SELECT jsonb_agg(jsonb_build_object('policy_version',h.policy_version,'snapshot_schema_version',h.snapshot_schema_version,'state',h.state,'version',h.version) ORDER BY h.version) FROM billing_approval_command_activation_history h WHERE h.command_type='LEDGER_REPAIR' AND h.policy_version=1 AND h.snapshot_schema_version=1 AND h.state='ACTIVE' AND EXISTS(SELECT 1 FROM relevant_cases)),'[]'),
 'journals',COALESCE((SELECT jsonb_agg(to_jsonb(j)) FROM relevant_journals j),'[]'),
 'postings',COALESCE((SELECT jsonb_agg(to_jsonb(p)||jsonb_build_object('account',to_jsonb(a))) FROM ledger_postings p JOIN relevant_journals j ON j.id=p.journal_entry_id LEFT JOIN ledger_accounts a ON a.id=p.ledger_account_id),'[]')))`
}

// Old no-repair incidents keep the exact old digest. When evidence exists,
// include immutable case, account, reversal and source-posting identities.
func reconciliationLedgerRepairDigestFacts(payload reconciliationAuditRecord) []string {
	var evidence reconciliationAuditRecord
	if json.Unmarshal(payload["ledger_repairs"], &evidence) != nil {
		return nil
	}
	if len(auditRecords(evidence["cases"]))+len(auditRecords(evidence["journals"]))+len(auditRecords(evidence["postings"])) == 0 {
		return nil
	}
	selected := []string{}
	fields := map[string][]string{
		"cases":    {"id", "approval_request_id", "authorization_event_id", "approval_epoch", "execution_key_digest", "target_journal_entry_id", "target_posting_id", "wrong_ledger_account_id", "correct_ledger_account_id", "entry_type", "amount_microcredits", "posting_set_digest", "facts_digest", "journal_entry_id", "state"},
		"journals": {"id", "entry_type", "business_reference"},
		"postings": {"id", "journal_entry_id", "ledger_account_id", "delta_microcredits", "credit_lot_id"},
	}
	for section, keys := range fields {
		for _, record := range auditRecords(evidence[section]) {
			entry := map[string]json.RawMessage{}
			for _, key := range keys {
				entry[key] = record[key]
			}
			accounts := []string{}
			if section == "cases" {
				accounts = []string{"wrong_account", "correct_account"}
			}
			if section == "postings" {
				accounts = []string{"account"}
			}
			for _, name := range accounts {
				var account reconciliationAuditRecord
				_ = json.Unmarshal(record[name], &account)
				for _, key := range []string{"id", "code", "account_type", "wallet_id"} {
					entry[name+"_"+key] = account[key]
				}
			}
			raw, _ := json.Marshal(entry)
			selected = append(selected, "ledger-repair-"+section+":"+string(raw))
		}
	}
	for _, journal := range auditRecords(payload["journals"]) {
		raw, _ := json.Marshal(map[string]json.RawMessage{"id": journal["id"], "created_at": journal["created_at"]})
		selected = append(selected, "ledger-repair-source-journal:"+string(raw))
	}
	for _, post := range auditRecords(payload["postings"]) {
		raw, _ := json.Marshal(map[string]json.RawMessage{"id": post["id"], "journal_entry_id": post["journal_entry_id"]})
		selected = append(selected, "ledger-repair-source-posting:"+string(raw))
	}
	return selected
}
