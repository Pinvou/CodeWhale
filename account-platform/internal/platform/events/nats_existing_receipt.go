package events

import (
	"encoding/json"
	"time"
)

// Both origins remain explicit and closed. A new origin cannot impersonate
// the original fresh receipt, even when its common Q+1 binding is identical.
func validReceiptOrigin(receipt approvalConsumerCutoverReceipt, now time.Time) bool {
	switch receipt.Schema {
	case "approval-consumer-cutover-receipt-v1":
		return receipt.ExistingBroker == nil
	case existingBrokerReceiptSchema:
		if receipt.ExistingBroker == nil {
			return false
		}
		facts := receipt.ExistingBroker
		raw, err := json.Marshal(facts.Plan)
		plan := facts.Plan
		return err == nil && matchesSHA256(raw, facts.PlanSHA256) && plan.validate(now) == nil &&
			plan.IdentitySHA256 == receipt.IdentitySHA256 && plan.ManifestSHA256 == receipt.ManifestSHA256 &&
			plan.ControlVersion == receipt.ControlVersion && plan.ApprovalEpoch == receipt.ApprovalEpoch &&
			plan.RelayEpoch == receipt.AuthorizationRelayEpoch && plan.HighWatermark == receipt.HighWatermark &&
			plan.CapturedAt == receipt.CapturedAt
	}
	return false
}
