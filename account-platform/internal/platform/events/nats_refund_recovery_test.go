package events

import (
	"encoding/json"
	"testing"
)

func TestRefundRecoverySnapshotRetainsOriginalIdentitiesAndPositions(t *testing.T) {
	_, receipt := existingReceiptFixture(t)
	original := receipt.ExistingBroker.Plan
	original.StreamFirst, original.StreamMessages, original.StreamBytes = "10", "5", "1000"
	copyPlan := func() existingBrokerPlan {
		raw, _ := json.Marshal(original)
		var p existingBrokerPlan
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	current := copyPlan()
	current.StreamFirst, current.StreamMessages, current.StreamBytes = "12", "3", "600"
	if !refundRecoverySnapshotMatches(original, current) || !refundRecoverySnapshotMatches(original, original) {
		t.Fatal("explicit recovery cannot snapshot unchanged consumers and retained messages")
	}
	for name, mutate := range map[string]func(*existingBrokerPlan){
		"new-message":       func(p *existingBrokerPlan) { p.HighWatermark = "9007199254740994" },
		"replacement":       func(p *existingBrokerPlan) { p.Existing[1].CreatedAt = p.StreamCreatedAt },
		"ack-moved":         func(p *existingBrokerPlan) { p.Existing[1].AckStream = "1" },
		"delivery-moved":    func(p *existingBrokerPlan) { p.Existing[1].DeliveredConsumer = "1" },
		"config-changed":    func(p *existingBrokerPlan) { p.Existing[1].Configuration.MaxDeliver = 12 },
		"control-changed":   func(p *existingBrokerPlan) { p.ControlVersion = "4" },
		"stream-replaced":   func(p *existingBrokerPlan) { p.StreamCreatedAt = p.CapturedAt },
		"count-increased":   func(p *existingBrokerPlan) { p.StreamMessages = "6" },
		"interior-removal":  func(p *existingBrokerPlan) { p.StreamMessages = "4"; p.StreamBytes = "800" },
		"bytes-only-change": func(p *existingBrokerPlan) { p.StreamBytes = "999" },
	} {
		t.Run(name, func(t *testing.T) {
			p := copyPlan()
			mutate(&p)
			if refundRecoverySnapshotMatches(original, p) {
				t.Fatal("unapproved recovery drift accepted")
			}
		})
	}
}
