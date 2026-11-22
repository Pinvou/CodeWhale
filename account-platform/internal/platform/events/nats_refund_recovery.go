package events

import (
	"context"
	"reflect"
	"time"
)

// PrepareRefundRecoveryPlan reads a new exact snapshot of retained messages.
// The caller separately authenticates the original SQL attempt and records the
// explicit recovery authorization. This function neither updates consumers nor
// relaxes the exact snapshot checks used by the subsequent single attempt.
func PrepareRefundRecoveryPlan(ctx context.Context, rawURL string, cfg NATSConfig, originalFiles ExistingBrokerResourceFiles, facts FreshApprovalControl, operationID string) ([]byte, error) {
	originalFiles = originalFiles.withAuthentication(cfg)
	original, err := readBrokerOperation(originalFiles, facts, "refund")
	if err != nil || operationID == original.OperationID {
		return nil, ErrNATSResources
	}
	raw, err := PrepareRefundUpgradePlan(ctx, rawURL, cfg, originalFiles.ApprovalConsumerResourceFiles, facts, operationID)
	if err != nil {
		return nil, err
	}
	current, err := decodeBrokerOperation(raw, publicSHA256(raw), time.Now().UTC(), "refund")
	if err != nil || !refundRecoverySnapshotMatches(original, current) {
		return nil, ErrNATSResources
	}
	return raw, nil
}

func refundRecoverySnapshotMatches(original, current existingBrokerPlan) bool {
	oldFirst, a := exactUnsigned(original.StreamFirst)
	newFirst, b := exactUnsigned(current.StreamFirst)
	oldMessages, c := exactUnsigned(original.StreamMessages)
	newMessages, d := exactUnsigned(current.StreamMessages)
	oldBytes, e := exactUnsigned(original.StreamBytes)
	newBytes, f := exactUnsigned(current.StreamBytes)
	oldTime, g := exactTime(original.CapturedAt, time.Now().UTC())
	newTime, h := exactTime(current.CapturedAt, time.Now().UTC())
	unchanged := newFirst == oldFirst && newMessages == oldMessages && newBytes == oldBytes
	reduced := newFirst > oldFirst && newMessages < oldMessages && newBytes < oldBytes && oldMessages-newMessages <= newFirst-oldFirst
	return a && b && c && d && e && f && g && h && !newTime.Before(oldTime) &&
		current.StreamCreatedAt == original.StreamCreatedAt && current.HighWatermark == original.HighWatermark &&
		current.IdentitySHA256 == original.IdentitySHA256 && current.ManifestSHA256 == original.ManifestSHA256 &&
		current.ControlVersion == original.ControlVersion && current.ApprovalEpoch == original.ApprovalEpoch && current.RelayEpoch == original.RelayEpoch &&
		(unchanged || reduced) &&
		reflect.DeepEqual(current.Existing, original.Existing)
}
