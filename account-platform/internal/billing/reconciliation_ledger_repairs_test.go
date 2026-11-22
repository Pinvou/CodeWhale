package billing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ledgerRepairReconciliationFixture(t *testing.T) (reconciliationAuditRequest, reconciliationAuditRecord) {
	t.Helper()
	request, payload := correctionAuditFixture(t)
	journals := auditRecords(payload["journals"])
	journalID := auditString(journals[0], "id")
	created := time.Date(2026, 10, 1, 0, 0, 0, 123456000, time.UTC)
	journals[0]["created_at"], _ = json.Marshal(created)
	payload["journals"], _ = json.Marshal(journals)
	posts := auditRecords(payload["postings"])
	target := ledgerRepairTarget{JournalEntryID: uuid.MustParse(journalID), EntryType: "RECHARGE", BusinessReference: request.References[0], CreatedAt: created}
	wrong := map[string]any{"id": adjustmentAccount.String(), "wallet_id": nil, "code": "platform:adjustment", "account_type": "ADJUSTMENT"}
	correct := map[string]any{"id": paymentClearingAccount.String(), "wallet_id": nil, "code": "platform:payment-clearing", "account_type": "PAYMENT_CLEARING"}
	var targetID string
	for i, post := range posts {
		id := uuid.New()
		post["id"], _ = json.Marshal(id.String())
		var account reconciliationAuditRecord
		_ = json.Unmarshal(post["account"], &account)
		if i == len(posts)-1 {
			targetID = id.String()
			post["account"], _ = json.Marshal(wrong)
			post["ledger_account_id"], _ = json.Marshal(adjustmentAccount.String())
			_ = json.Unmarshal(post["account"], &account)
		}
		accountID := uuid.MustParse(auditString(post, "ledger_account_id"))
		amount := auditInteger(post, "delta_microcredits")
		var wallet, lot *uuid.UUID
		if raw := auditString(account, "wallet_id"); raw != "" {
			value := uuid.MustParse(raw)
			wallet = &value
		}
		if raw := auditString(post, "credit_lot_id"); raw != "" {
			value := uuid.MustParse(raw)
			lot = &value
		}
		target.Postings = append(target.Postings, ledgerRepairPosting{ID: id, LedgerAccountID: accountID, AccountType: auditString(account, "account_type"), WalletID: wallet, Delta: amount.Int64(), CreditLotID: lot})
	}
	payload["postings"], _ = json.Marshal(posts)
	reversal := uuid.NewString()
	c := map[string]any{"id": uuid.NewString(), "approval_request_id": uuid.NewString(), "authorization_event_id": uuid.NewString(), "approval_epoch": 1, "execution_key_digest": strings.Repeat("a", 64), "facts_digest": strings.Repeat("b", 64), "posting_set_digest": ledgerRepairPostingSetDigest(target), "target_journal_entry_id": journalID, "target_posting_id": targetID, "wrong_ledger_account_id": adjustmentAccount.String(), "correct_ledger_account_id": paymentClearingAccount.String(), "entry_type": "RECHARGE", "amount_microcredits": 100, "journal_entry_id": reversal, "state": "EXECUTED", "wrong_account": wrong, "correct_account": correct}
	j := map[string]any{"id": reversal, "entry_type": "REVERSAL", "business_reference": "ledger-repair:" + targetID}
	legs := []any{map[string]any{"id": uuid.NewString(), "journal_entry_id": reversal, "ledger_account_id": adjustmentAccount.String(), "delta_microcredits": 100, "account": wrong}, map[string]any{"id": uuid.NewString(), "journal_entry_id": reversal, "ledger_account_id": paymentClearingAccount.String(), "delta_microcredits": -100, "account": correct}}
	payload["ledger_repairs"], _ = json.Marshal(map[string]any{"cases": []any{c}, "journals": []any{j}, "postings": legs})
	return request, payload
}

func TestReconciliationLedgerRepairEvidence(t *testing.T) {
	request, payload := ledgerRepairReconciliationFixture(t)
	before := append([]byte(nil), payload["postings"]...)
	valid := ReconciliationInternalFact{EntryType: "PAYMENT"}
	auditReconciliationFact(&valid, request, payload)
	if !valid.LedgerValid {
		t.Fatal("exact correction did not resolve original attribution", valid.IntegrityCaseType)
	}
	if string(payload["postings"]) != string(before) {
		t.Fatal("original evidence mutated")
	}
	originalDigest := valid.IntegrityDigest
	if originalDigest == "" {
		t.Fatal("missing digest")
	}
	mutations := []struct {
		section, field, value string
		index                 int
	}{
		{"cases", "target_journal_entry_id", `"00000000-0000-4000-8000-000000000001"`, 0},
		{"cases", "target_posting_id", `"00000000-0000-4000-8000-000000000002"`, 0},
		{"cases", "amount_microcredits", `99`, 0},
		{"cases", "wrong_ledger_account_id", `"00000000-0000-4000-8000-000000000005"`, 0},
		{"cases", "correct_ledger_account_id", `"00000000-0000-4000-8000-000000000006"`, 0},
		{"cases", "journal_entry_id", `"00000000-0000-4000-8000-000000000007"`, 0},
		{"cases", "execution_key_digest", `"bad"`, 0},
		{"cases", "posting_set_digest", `"` + strings.Repeat("a", 64) + `"`, 0},
		{"cases", "state", `"FAILED"`, 0},
		{"journals", "entry_type", `"RECHARGE"`, 0},
		{"journals", "business_reference", `"ledger-repair:wrong"`, 0},
		{"postings", "delta_microcredits", `99`, 0},
		{"postings", "credit_lot_id", `"00000000-0000-4000-8000-000000000003"`, 0},
		{"postings", "ledger_account_id", `"00000000-0000-4000-8000-000000000004"`, 0},
	}
	for _, m := range mutations {
		t.Run(m.section+"-"+m.field, func(t *testing.T) {
			var evidence reconciliationAuditRecord
			_ = json.Unmarshal(payload["ledger_repairs"], &evidence)
			records := auditRecords(evidence[m.section])
			records[m.index][m.field] = json.RawMessage(m.value)
			evidence[m.section], _ = json.Marshal(records)
			clone := make(reconciliationAuditRecord, len(payload))
			for k, v := range payload {
				clone[k] = v
			}
			clone["ledger_repairs"], _ = json.Marshal(evidence)
			fact := ReconciliationInternalFact{EntryType: "PAYMENT"}
			auditReconciliationFact(&fact, request, clone)
			if fact.LedgerValid {
				t.Fatal("invalid evidence accepted")
			}
			if fact.IntegrityDigest == originalDigest {
				t.Fatal("changed immutable evidence did not change digest")
			}
		})
	}
	for _, scenario := range []string{"orphan-journal", "missing-leg", "duplicate-leg", "duplicate-case", "duplicate-journal", "forged-account-code"} {
		t.Run(scenario, func(t *testing.T) {
			var evidence reconciliationAuditRecord
			if err := json.Unmarshal(payload["ledger_repairs"], &evidence); err != nil {
				t.Fatal(err)
			}
			cases, journals, postings := auditRecords(evidence["cases"]), auditRecords(evidence["journals"]), auditRecords(evidence["postings"])
			switch scenario {
			case "orphan-journal":
				cases = nil
			case "missing-leg":
				postings = postings[:1]
			case "duplicate-leg":
				postings[1] = postings[0]
			case "duplicate-case":
				cases = append(cases, cases[0])
			case "duplicate-journal":
				journals = append(journals, journals[0])
			case "forged-account-code":
				var account reconciliationAuditRecord
				if err := json.Unmarshal(cases[0]["correct_account"], &account); err != nil {
					t.Fatal(err)
				}
				account["code"] = json.RawMessage(`"platform:forged"`)
				cases[0]["correct_account"], _ = json.Marshal(account)
			}
			evidence["cases"], _ = json.Marshal(cases)
			evidence["journals"], _ = json.Marshal(journals)
			evidence["postings"], _ = json.Marshal(postings)
			clone := make(reconciliationAuditRecord, len(payload))
			for key, value := range payload {
				clone[key] = value
			}
			clone["ledger_repairs"], _ = json.Marshal(evidence)
			fact := ReconciliationInternalFact{EntryType: "PAYMENT"}
			auditReconciliationFact(&fact, request, clone)
			if fact.LedgerValid {
				t.Fatal("incomplete, forged or duplicate evidence accepted")
			}
			if fact.IntegrityDigest == originalDigest {
				t.Fatal("changed evidence kept original digest")
			}
		})
	}
	t.Run("missing-case", func(t *testing.T) {
		clone := make(reconciliationAuditRecord, len(payload))
		for k, v := range payload {
			clone[k] = v
		}
		delete(clone, "ledger_repairs")
		fact := ReconciliationInternalFact{EntryType: "PAYMENT"}
		auditReconciliationFact(&fact, request, clone)
		if fact.LedgerValid {
			t.Fatal("wrong attribution accepted without receipt")
		}
	})
	t.Run("unrelated-wallet-corruption", func(t *testing.T) {
		clone := make(reconciliationAuditRecord, len(payload))
		for k, v := range payload {
			clone[k] = v
		}
		posts := auditRecords(clone["postings"])
		posts[0]["delta_microcredits"] = json.RawMessage(`999`)
		clone["postings"], _ = json.Marshal(posts)
		fact := ReconciliationInternalFact{EntryType: "PAYMENT"}
		auditReconciliationFact(&fact, request, clone)
		if fact.LedgerValid {
			t.Fatal("repair hid unrelated financial corruption")
		}
	})
}

func TestReconciliationNoLedgerRepairDigestCompatibility(t *testing.T) {
	_, payload := correctionAuditFixture(t)
	before := reconciliationIntegrityDigest(payload)
	payload["ledger_repairs"] = json.RawMessage(`{"cases":[],"journals":[],"postings":[]}`)
	if got := reconciliationIntegrityDigest(payload); got != before {
		t.Fatal("empty repair evidence changed historical digest", before, got)
	}
}
