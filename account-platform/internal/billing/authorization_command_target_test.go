package billing

import (
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func TestAuthorizationCommandTargetClosedSet(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct {
		command, target string
		policy          int64
	}{
		{entitlementComponentAdjustmentCommand, "ENTITLEMENT_PACKAGE", 2},
		{"WALLET_ADJUSTMENT", "MANUAL_ADJUSTMENT", 1},
		{"PAYMENT_PROVIDER_RESUME", "PAYMENT_PROVIDER_CONTROL", 1},
		{"PAYMENT_PRODUCT_RESUME", "PAYMENT_PRODUCT_CONTROL", 1},
		{ledgerRepairCommand, "LEDGER_POSTING", 1},
		{receivableWriteOffCommand, "RECEIVABLE", 1},
		{paymentFinancialRepairCommand, "PAYMENT_ORDER", 1},
		{refundExceptionRepairCommand, "REFUND", 1},
		{chargebackFinancialRepairCommand, "CHARGEBACK_CASE", 1},
	} {
		t.Run(tc.command, func(t *testing.T) {
			event := approvalencoding.AuthorizationEvent{CommandType: tc.command, PolicyVersion: tc.policy, TargetResourceType: tc.target, TargetResourceID: id}
			if !validAuthorizationCommandTarget(event) {
				t.Fatal("installed target rejected")
			}
			event.TargetResourceType = "UNINSTALLED_TARGET"
			if validAuthorizationCommandTarget(event) {
				t.Fatal("unknown target accepted")
			}
		})
	}
	for _, tc := range []struct {
		name, command, target, id string
		policy                    int64
	}{
		{"wrong-policy", receivableWriteOffCommand, "RECEIVABLE", id, 2},
		{"wrong-target", receivableWriteOffCommand, "LEDGER_POSTING", id, 1},
		{"noncanonical", receivableWriteOffCommand, "RECEIVABLE", strings.ToUpper(id), 1},
		{"zero-id", receivableWriteOffCommand, "RECEIVABLE", uuid.Nil.String(), 1},
		{"payment-wrong-policy", paymentFinancialRepairCommand, "PAYMENT_ORDER", id, 2},
		{"payment-wrong-target", paymentFinancialRepairCommand, "RECEIVABLE", id, 1},
		{"payment-noncanonical", paymentFinancialRepairCommand, "PAYMENT_ORDER", strings.ToUpper(id), 1},
		{"payment-zero-id", paymentFinancialRepairCommand, "PAYMENT_ORDER", uuid.Nil.String(), 1},
		{"refund-wrong-policy", refundExceptionRepairCommand, "REFUND", id, 2},
		{"refund-wrong-target", refundExceptionRepairCommand, "PAYMENT_ORDER", id, 1},
		{"refund-noncanonical", refundExceptionRepairCommand, "REFUND", strings.ToUpper(id), 1},
		{"refund-zero-id", refundExceptionRepairCommand, "REFUND", uuid.Nil.String(), 1},
		{"chargeback-wrong-policy", chargebackFinancialRepairCommand, "CHARGEBACK_CASE", id, 2},
		{"chargeback-wrong-target", chargebackFinancialRepairCommand, "REFUND", id, 1},
		{"chargeback-noncanonical", chargebackFinancialRepairCommand, "CHARGEBACK_CASE", strings.ToUpper(id), 1},
		{"chargeback-zero-id", chargebackFinancialRepairCommand, "CHARGEBACK_CASE", uuid.Nil.String(), 1},
		{"unknown-command", "FUTURE_COMMAND", "RECEIVABLE", id, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if validAuthorizationCommandTarget(approvalencoding.AuthorizationEvent{CommandType: tc.command, PolicyVersion: tc.policy, TargetResourceType: tc.target, TargetResourceID: tc.id}) {
				t.Fatal("unregistered target accepted")
			}
		})
	}
}
