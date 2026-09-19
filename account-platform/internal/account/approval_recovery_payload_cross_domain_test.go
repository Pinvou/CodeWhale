package account

import (
	"encoding/json"
	"testing"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
)

// The Account recovery reader validates the Billing positive-result payload
// against a closed shape per command. Both sides are separate packages with
// separate JSON tags, so this test marshals the real Billing result structs and
// proves the Account-side closure accepts exactly what the owner writes. Without
// it the two definitions can drift and recovery would silently degrade to
// "unavailable" for a command that actually succeeded.
func TestApprovalRecoveryPayloadAcceptsRealBillingResults(t *testing.T) {
	digest := func(seed string) string {
		value := ""
		for len(value) < 64 {
			value += seed
		}
		return value[:64]
	}
	ledgerQuote := func(id uuid.UUID) json.RawMessage {
		encoded, _ := json.Marshal(map[string]any{"journal_entry_id": id})
		return encoded
	}
	journalID := uuid.New()
	cases := map[string]struct {
		domain any
		ledger json.RawMessage
	}{
		"LEDGER_REPAIR": {billingdomain.LedgerRepairResult{
			CaseID: uuid.New(), TargetJournalEntryID: uuid.New(), TargetPostingID: uuid.New(),
			WrongLedgerAccountID: uuid.New(), CorrectLedgerAccountID: uuid.New(), EntryType: "REVERSAL",
			AmountMicrocredits: 30, PostingSetDigest: digest("a"), JournalEntryID: journalID, Status: "SUCCEEDED",
		}, ledgerQuote(journalID)},
		"RECEIVABLE_WRITE_OFF": {billingdomain.ReceivableWriteOffResult{
			CaseID: uuid.New(), ReceivableID: uuid.New(), WalletID: uuid.New(), AmountMicrocredits: 500,
			ReceivableVersionBefore: 3, ReceivableVersionAfter: 4, StatusAfter: "WRITTEN_OFF",
			FactsDigest: digest("b"), JournalEntryID: journalID, AllocationID: uuid.New(), Status: "SUCCEEDED",
		}, ledgerQuote(journalID)},
		"PAYMENT_FINANCIAL_REPAIR": {billingdomain.PaymentFinancialRepairResult{
			RepairID: uuid.New(), PaymentOrderID: uuid.New(), PaymentTransactionID: uuid.New(),
			EntitlementPackageID: uuid.New(), JournalEntryID: journalID, ProviderTransactionID: "channel-1",
			CreditMicrocredits: 1000, AppliedToReceivables: 0, WalletVersionAfter: 2,
			FactsDigest: digest("c"), Status: "SUCCEEDED",
		}, ledgerQuote(journalID)},
		"REFUND_EXCEPTION_REPAIR": {billingdomain.RefundExceptionRepairResult{
			CaseID: uuid.New(), RefundID: uuid.New(), WalletID: uuid.New(), PackageID: uuid.New(),
			RevokedMicrocredits: 80, ShortfallMicrocredits: 20, JournalEntryID: journalID,
			ProviderRefundID: "refund-1", FactsDigest: digest("d"), Status: "SUCCEEDED",
		}, ledgerQuote(journalID)},
		"CHARGEBACK_FINANCIAL_REPAIR": {billingdomain.ChargebackFinancialRepairResult{
			CaseID: uuid.New(), ChargebackCaseID: uuid.New(), WalletID: uuid.New(), PackageID: uuid.New(),
			PaymentEvidenceID: uuid.New(), BaseShortfallMicrocredits: 50, ComponentDigest: digest("e"),
			FactsDigest: digest("f"), JournalEntryID: journalID, ReceivableID: uuid.New(),
			ProviderChargebackID: "chargeback-1", Status: "SUCCEEDED",
		}, ledgerQuote(journalID)},
		"RECONCILIATION_FINANCIAL_REPAIR": {billingdomain.ReconciliationFinancialRepairResult{
			CaseID: uuid.New(), RunID: uuid.New(), Channel: "ALIPAY", BusinessDate: "2026-09-20",
			MerchantOrderNo: "merchant-1", AmountFen: 100, Currency: "CNY", ProviderTransactionID: "channel-2",
			PaymentEvidenceID: uuid.New(), ResolutionCode: "PROVIDER_REFUND_CONFIRMED", RecheckCaseCount: 0,
			FactsDigest: digest("1"), LedgerEffect: "NONE", Status: "SUCCEEDED",
		}, json.RawMessage(`{}`)},
		"USAGE_SETTLEMENT_REPAIR": {billingdomain.UsageSettlementRepairResult{
			CaseID: uuid.New(), UsageRequestID: uuid.New(), HoldID: uuid.New(), WalletID: uuid.New(),
			LossReviewID: &journalID, EvidenceKind: "LOSS_REVIEW", ExpectedMicrocredits: 100,
			SettledMicrocredits: 100, OverageMicrocredits: 0, AllocationsDigest: digest("2"),
			FactsDigest: digest("3"), JournalEntryID: journalID, Status: "SUCCEEDED",
		}, ledgerQuote(journalID)},
	}
	for command, test := range cases {
		t.Run(command, func(t *testing.T) {
			domain, err := json.Marshal(test.domain)
			if err != nil {
				t.Fatal(err)
			}
			if !validBillingRecoveryPayload(command, domain, test.ledger) {
				t.Fatalf("%s payload is not accepted by the Account recovery closure: %s", command, domain)
			}
			// A degraded owner answer must never be accepted: the closure is not a
			// lenient "any object" check.
			if validBillingRecoveryPayload(command, json.RawMessage(`{}`), test.ledger) {
				t.Fatalf("%s accepted an empty domain result", command)
			}
			// The reconciliation repair is the one command whose closure carries
			// no ledger effect at all, so an empty ledger reference set is exactly
			// its closed shape and must stay acceptable there.
			if command == "RECONCILIATION_FINANCIAL_REPAIR" {
				if !validBillingRecoveryPayload(command, domain, json.RawMessage(`{}`)) {
					t.Fatalf("%s must accept its zero-accounting-effect ledger closure", command)
				}
			} else if validBillingRecoveryPayload(command, domain, json.RawMessage(`{}`)) {
				t.Fatalf("%s accepted an empty ledger reference set", command)
			}
		})
	}
}
