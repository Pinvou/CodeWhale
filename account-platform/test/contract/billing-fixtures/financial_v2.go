package main

import (
	"encoding/json"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
)

// These are real production response types serialized by encoding/json. They
// verify the financial wire above JavaScript's exact integer boundary; they do
// not simulate database persistence, authorization or payment effects.
func financialV2Fixtures() map[string]any {
	id := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	amount := int64(9007199254740993)
	return map[string]any{
		"PaymentOrderDetail":      billing.AdminPaymentOrder{ID: id, MerchantOrderNo: "fixture-order", ProviderID: "development_simulator", ProviderVersion: "development-v1", PayerUserID: id, WalletID: id, AmountFen: amount, CreditMicrocredits: amount, PaymentSummary: json.RawMessage(`{"paid_count":1,"paid_amount_fen":"9007199254740993","late_paid_count":0,"review_count":0}`), RefundSummary: json.RawMessage(`{"succeeded_amount_fen":"9007199254740993","pending_count":0,"review_count":0}`), ChargebackSummary: json.RawMessage(`{"open_count":1,"lost_amount_fen":"9007199254740993"}`), AttentionSummary: "NORMAL", SummaryVersion: 1, Transactions: []billing.PaymentTransaction{}, Evidence: []billing.PaymentEvidence{}, Packages: []billing.EntitlementPackage{}},
		"UsageHold":               billing.AdminHold{ID: id, UsageRequestID: id, WalletID: id, Status: "REVIEW", AnomalyState: "REVIEW_REQUIRED", MaxMicrocredits: amount, ActualMicrocredits: &amount, ExpiresAt: now, CreatedAt: now, UpdatedAt: now},
		"HoldComponentAllocation": billing.AdminHoldComponentAllocation{PackageID: id, ComponentID: id, ComponentType: "BASE", CreditLotID: id, ReservedMicrocredits: amount, SettledMicrocredits: amount, ReleasedMicrocredits: amount, Status: "ACTIVE"},
		"HoldLeaseEvent":          billing.AdminHoldLeaseEvent{ID: id, EventType: "EXTENDED", ExpiresAt: now, PreviousMaxMicrocredits: amount, MaxMicrocredits: amount, AdditionalMicrocredits: amount, CreatedAt: now},
		"Receivable":              billing.Receivable{ID: id, WalletID: id, SourceType: "USAGE_OVERAGE", SourceReference: id.String(), OriginalMicrocredits: amount, SettledMicrocredits: amount, OutstandingMicrocredits: amount, Status: "OPEN", Version: 1, CreatedAt: now, UpdatedAt: now},
		"ReceivableAllocation":    billing.ReceivableAllocation{ID: id, ReceivableID: id, WalletID: id, SourceType: "CASH_RECHARGE_BASE", SourceReference: id.String(), AmountMicrocredits: amount, JournalEntryID: id, CreatedAt: now},
		"PaymentTransaction":      billing.PaymentTransaction{ID: id, PaymentOrderID: id, ProviderID: "development_simulator", ProviderVersion: "development-v1", ProviderTransactionID: "fixture-transaction", Channel: "ALIPAY", Status: "PAID", FulfillmentStatus: "CREDITED", AmountFen: amount, Currency: "CNY", ProviderStatus: "PAID", VerifiedAt: now},
		"PaymentEvidence":         billing.PaymentEvidence{ID: id, PaymentOrderID: id, EvidenceType: "QUERY", ProviderID: "development_simulator", ProviderVersion: "development-v1", EvidenceKey: "fixture-evidence", AmountFen: &amount, ReceivedAt: now, PayloadDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PayloadSnapshot: []byte(`{"fixture":true}`), MergeResult: "MERGED"},
	}
}
