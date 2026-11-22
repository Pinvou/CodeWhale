package account

import (
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
)

func TestLifecycleReconciliationUsesExactRegisteredVersionMapping(t *testing.T) {
	all := approvalconsumer.All()
	for _, descriptor := range all {
		candidate := ApprovalReconciliationCandidate{CommandType: descriptor.CommandType, PolicyVersion: descriptor.PolicyVersion, ExecutorIdentity: descriptor.ExecutorIdentity}
		if !reconciliationDescriptorMatches(all, candidate) {
			t.Fatal("existing registered reconciliation changed", descriptor.CommandType)
		}
	}
	descriptor, ok := approvalconsumer.LookupDurable("token-model-lifecycle-executor-v1")
	if !ok {
		t.Fatal("original lifecycle durable missing")
	}
	candidate := ApprovalReconciliationCandidate{CommandType: descriptor.CommandType, PolicyVersion: 3, ExecutorIdentity: descriptor.ExecutorIdentity}
	if !reconciliationDescriptorMatches(all, candidate) {
		t.Fatal("supported lifecycle3 reconciliation rejected")
	}
	for _, version := range []int64{0, 1, 4} {
		invalid := candidate
		invalid.PolicyVersion = version
		if reconciliationDescriptorMatches(all, invalid) {
			t.Fatal("unknown lifecycle version accepted", version)
		}
	}
	for _, field := range []string{"durable", "workload", "source", "command", "executor", "policy"} {
		t.Run("forged-"+field, func(t *testing.T) {
			forged := descriptor
			switch field {
			case "durable":
				forged.Durable = "forged-lifecycle-durable"
			case "workload":
				forged.Workload = approvalconsumer.BillingWorkload
			case "source":
				forged.StartSource = approvalconsumer.StartLegacySharedPreFenceCompatibility
			case "command":
				forged.CommandType = "TOKEN_MODEL_CATALOG_ROLLBACK"
			case "executor":
				forged.ExecutorIdentity = approvalconsumer.BillingWorkload
			case "policy":
				forged.PolicyVersion = 3
			}
			if reconciliationDescriptorMatches([]approvalconsumer.Descriptor{forged}, candidate) {
				t.Fatal("forged consumer identity accepted")
			}
		})
	}
	for _, invalid := range []ApprovalReconciliationCandidate{
		{CommandType: candidate.CommandType, PolicyVersion: 3, ExecutorIdentity: approvalconsumer.BillingWorkload},
		{CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", PolicyVersion: 3, ExecutorIdentity: descriptor.ExecutorIdentity},
		{CommandType: "UNKNOWN", PolicyVersion: 3, ExecutorIdentity: descriptor.ExecutorIdentity},
	} {
		if reconciliationDescriptorMatches(all, invalid) {
			t.Fatal("unregistered reconciliation tuple accepted")
		}
	}
}
