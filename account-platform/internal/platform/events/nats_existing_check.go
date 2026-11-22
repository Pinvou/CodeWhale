package events

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// CheckExistingBrokerConsumers requires the new origin and the original plan.
// It never relaxes the original fresh checker or generates a replacement Q.
func CheckExistingBrokerConsumers(ctx context.Context, rawURL string, cfg NATSConfig, files ExistingBrokerResourceFiles) (FreshApprovalControl, error) {
	files = files.withAuthentication(cfg)
	if _, err := files.Load(time.Now().UTC()); err != nil {
		return FreshApprovalControl{}, err
	}
	raw, err := files.VerifiedReceipt(time.Now().UTC())
	if err != nil {
		return FreshApprovalControl{}, err
	}
	var receipt approvalConsumerCutoverReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.Schema != existingBrokerReceiptSchema || receipt.ExistingBroker == nil || receipt.ExistingBroker.PlanSHA256 != files.PlanSHA256 {
		return FreshApprovalControl{}, ErrNATSResources
	}
	plan := receipt.ExistingBroker.Plan
	version, _ := strconv.ParseInt(plan.ControlVersion, 10, 64)
	epoch, _ := strconv.ParseInt(plan.ApprovalEpoch, 10, 64)
	relay, _ := strconv.ParseInt(plan.RelayEpoch, 10, 64)
	facts := FreshApprovalControl{Version: version, ApprovalEpoch: epoch, RelayEpoch: relay}
	current, err := InitializeExistingBrokerConsumers(ctx, rawURL, cfg, files, facts, false, false)
	if err != nil || string(current) != string(raw) {
		return FreshApprovalControl{}, ErrNATSResources
	}
	return facts, nil
}
