package billing

import "testing"

// TestLedgerRepairContractShapeSetMatchesDesign pins the frozen §5 table of
// ledger-repair-design.md as a *shape set* with per-leg conditions: every entry
// type admits the shapes in which a conditional leg is absent, and only shapes
// outside the set (unknown account type, wrong direction on a mandatory leg,
// missing mandatory leg, or a present leg with a zero delta) are refused.
//
// The design names the platform-only shapes explicitly — the GSA-25 write-off
// (`ADJUSTMENT −A` + `RECEIVABLE_ASSET +A`) and the GSA-31 compensation
// (`CHARGEBACK_CLEARING +R+B` + `RECEIVABLE_ASSET −B`, no wallet leg) — and
// states that a conditional leg must never be treated as mandatory. Treating
// them as mandatory makes a legal journal look like an attribution error, so
// this test is the regression guard for that deviation.
func TestLedgerRepairContractShapeSetMatchesDesign(t *testing.T) {
	legal := []struct {
		name      string
		entryType string
		signature map[string]int64
	}{
		{"recharge complete", "RECHARGE", map[string]int64{"PAYMENT_CLEARING": -100, "WALLET_LIABILITY": 40, "RECEIVABLE_ASSET": 60}},
		{"recharge without wallet leg (fully applied)", "RECHARGE", map[string]int64{"PAYMENT_CLEARING": -100, "RECEIVABLE_ASSET": 100}},
		{"recharge without receivables", "RECHARGE", map[string]int64{"PAYMENT_CLEARING": -100, "WALLET_LIABILITY": 100}},
		{"usage complete", "USAGE", map[string]int64{"USAGE_REVENUE": 30, "WALLET_LIABILITY": -20, "RECEIVABLE_ASSET": -10}},
		{"usage without wallet leg (fully overage)", "USAGE", map[string]int64{"USAGE_REVENUE": 30, "RECEIVABLE_ASSET": -30}},
		{"adjustment wallet pair", "ADJUSTMENT", map[string]int64{"ADJUSTMENT": -25, "WALLET_LIABILITY": 25}},
		{"adjustment write-off shape (GSA-25)", "ADJUSTMENT", map[string]int64{"ADJUSTMENT": -25, "RECEIVABLE_ASSET": 25}},
		{"refund complete", "REFUND", map[string]int64{"REFUND_CLEARING": 30, "WALLET_LIABILITY": -20, "RECEIVABLE_ASSET": -10}},
		{"refund without wallet leg", "REFUND", map[string]int64{"REFUND_CLEARING": 30, "RECEIVABLE_ASSET": -30}},
		{"expiry complete", "EXPIRY", map[string]int64{"ADJUSTMENT": 15, "WALLET_LIABILITY": -15}},
		{"chargeback complete", "CHARGEBACK", map[string]int64{"CHARGEBACK_CLEARING": 30, "WALLET_LIABILITY": -20, "RECEIVABLE_ASSET": -10}},
		{"chargeback platform-only shape (GSA-31)", "CHARGEBACK", map[string]int64{"CHARGEBACK_CLEARING": 30, "RECEIVABLE_ASSET": -30}},
		{"repair credit complete", "REPAIR_CREDIT", map[string]int64{"PAYMENT_CLEARING": -30, "WALLET_LIABILITY": 20, "RECEIVABLE_ASSET": 10}},
		{"repair credit without wallet leg", "REPAIR_CREDIT", map[string]int64{"PAYMENT_CLEARING": -30, "RECEIVABLE_ASSET": 30}},
		{"forced refund complete", "FORCED_REFUND", map[string]int64{"REFUND_CLEARING": 30, "WALLET_LIABILITY": -20, "RECEIVABLE_ASSET": -10}},
		{"forced refund without wallet leg", "FORCED_REFUND", map[string]int64{"REFUND_CLEARING": 30, "RECEIVABLE_ASSET": -30}},
	}
	for _, item := range legal {
		contract, ok := ledgerRepairContracts[item.entryType]
		if !ok {
			t.Fatalf("%s: entry type %s has no frozen contract", item.name, item.entryType)
		}
		if !ledgerRepairSignatureMatches(contract, item.signature) {
			t.Fatalf("%s: legal shape of %s was refused: %v", item.name, item.entryType, item.signature)
		}
	}

	illegal := []struct {
		name      string
		entryType string
		signature map[string]int64
	}{
		{"unknown account type", "USAGE", map[string]int64{"USAGE_REVENUE": 30, "WALLET_LIABILITY": -20, "ADJUSTMENT": -10}},
		{"missing mandatory leg", "RECHARGE", map[string]int64{"WALLET_LIABILITY": 100}},
		{"missing mandatory leg (expiry)", "EXPIRY", map[string]int64{"WALLET_LIABILITY": -15}},
		{"wrong direction on mandatory leg", "REFUND", map[string]int64{"REFUND_CLEARING": -30, "WALLET_LIABILITY": -30}},
		{"wrong direction on optional leg", "CHARGEBACK", map[string]int64{"CHARGEBACK_CLEARING": 30, "RECEIVABLE_ASSET": 30}},
		{"present leg with zero delta", "USAGE", map[string]int64{"USAGE_REVENUE": 30, "WALLET_LIABILITY": 0}},
		{"write-off shape in the wrong direction", "ADJUSTMENT", map[string]int64{"ADJUSTMENT": 25, "RECEIVABLE_ASSET": 25}},
	}
	for _, item := range illegal {
		contract := ledgerRepairContracts[item.entryType]
		if ledgerRepairSignatureMatches(contract, item.signature) {
			t.Fatalf("%s: illegal shape of %s was accepted: %v", item.name, item.entryType, item.signature)
		}
	}

	// The design freezes the eight entry types whose writers exist and keeps
	// REVERSAL (this command's own output) and RECEIVABLE out of the table.
	if len(ledgerRepairContracts) != 8 {
		t.Fatalf("frozen contract table must hold exactly the 8 written entry types, got %d", len(ledgerRepairContracts))
	}
	for _, absent := range []string{"REVERSAL", "RECEIVABLE"} {
		if _, ok := ledgerRepairContracts[absent]; ok {
			t.Fatalf("%s must not be a repairable entry type", absent)
		}
	}
	// A conditional leg is optional and a mandatory leg is not: this is the
	// exact property the design demands ("不能把条件腿当必需腿").
	for entryType, contract := range ledgerRepairContracts {
		mandatory := 0
		for _, leg := range contract {
			if !leg.Optional {
				mandatory++
			}
		}
		if mandatory == 0 {
			t.Fatalf("%s has no mandatory leg", entryType)
		}
	}
}
