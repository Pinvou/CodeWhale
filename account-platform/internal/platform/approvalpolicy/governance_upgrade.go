package approvalpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

// BillingPolicySetUpgradeV1 is an explicit operations contract, not a runtime
// digest alias. It adds eight Billing policies without changing the original
// twenty-two policies. Future changes must establish a different contract.
const BillingPolicySetUpgradeV1 = "billing-policy-set-v1"

const legacyGovernancePolicyDigest = "d9a329010b29c105785ade35fbffcdd5b9816ebcdd84693616a795532d0c2783"
const expandedGovernancePolicyDigest = "f2b71d1b118a69974dad6154dcd50dd1ec06f36c20f8c1cf4316ecd799ab74ee"

var addedBillingGovernanceCommands = []string{
	"CHARGEBACK_FINANCIAL_REPAIR", "ENTITLEMENT_COMPONENT_ADJUSTMENT", "LEDGER_REPAIR",
	"PAYMENT_FINANCIAL_REPAIR", "RECEIVABLE_WRITE_OFF", "RECONCILIATION_FINANCIAL_REPAIR",
	"REFUND_EXCEPTION_REPAIR", "USAGE_SETTLEMENT_REPAIR",
}

// CanExpandGovernancePolicySet proves the entire new manifest and the unchanged
// old subset against independently fixed published digests. Database facts are
// still verified by VerifyPolicySet under the caller's Account SHARE lock.
func CanExpandGovernancePolicySet(from, to string) bool {
	policies, err := governancePolicies()
	if prior, ok := lifecyclePreviousPolicies(policies); ok {
		policies = prior
	}
	return err == nil && canExpandGovernancePolicySet(from, to, policies)
}

func canExpandGovernancePolicySet(from, to string, policies []Policy) bool {
	if from != legacyGovernancePolicyDigest || to != expandedGovernancePolicyDigest || len(policies) != 30 {
		return false
	}
	old := make([]Policy, 0, 22)
	for _, policy := range policies {
		if !slices.Contains(addedBillingGovernanceCommands, policy.CommandType) {
			old = append(old, policy)
		}
	}
	digest := func(value []Policy) string {
		raw, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	return len(old) == 22 && digest(old) == from && digest(policies) == to
}
