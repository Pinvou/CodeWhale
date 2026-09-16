package approvalconsumer

// FailureClass describes the next investigation, not permission to mutate an
// approval or call a domain. Account and the domain must independently prove
// the original result and zero effect before any terminal transition.
type FailureClass string

const (
	FailureUnknown             FailureClass = "UNKNOWN"
	FailurePermanent           FailureClass = "PERMANENT"
	FailureFactOrPolicyChanged FailureClass = "FACT_OR_POLICY_CHANGED"
	FailureWindowEnded         FailureClass = "WINDOW_ENDED"
	FailureTransientSafe       FailureClass = "TRANSIENT_SAFE"
)

type RecoveryAssessment struct {
	Class             FailureClass
	Retryable         bool
	MaxDomainAttempts int
	// Result queries are the only permitted automatic operation for proof-era
	// failures. Classification alone never authorizes invalidation or replay.
	QueryOnly bool
}

// ClassifyProofFailure is a closed command/version registry. A code is only
// recognized if its owner actually emits it with a definite failure before
// an effect. Even a recognized code is not evidence that an effect is absent.
// In particular no proof-era command currently has a demonstrated safe
// second attempt; the legacy Billing 12-attempt delivery protocol is separate.
func ClassifyProofFailure(command string, policyVersion int64, code string) RecoveryAssessment {
	result := RecoveryAssessment{Class: FailureUnknown, Retryable: false, MaxDomainAttempts: 1, QueryOnly: true}
	switch {
	case command == "WALLET_ADJUSTMENT" && policyVersion == 1:
		result.Class = classifyBillingFailure(code)
	case command == "PAYMENT_PROVIDER_RESUME" && policyVersion == 2:
		result.Class = classifyBillingFailure(code)
	case command == "PAYMENT_PRODUCT_RESUME" && policyVersion == 1:
		result.Class = classifyBillingFailure(code)
	case command == "TOKEN_MODEL_LIFECYCLE_CHANGE" && policyVersion == 2:
		result.Class = classifyTokenFailure(code)
	case command == "TOKEN_MODEL_CATALOG_ROLLBACK" && policyVersion == 2:
		result.Class = classifyTokenFailure(code)
	case command == "TOKEN_CREDENTIAL_REVOKE" && policyVersion == 1:
		result.Class = classifyTokenFailure(code)
	}
	return result
}

func classifyBillingFailure(code string) FailureClass {
	switch code {
	case "DOMAIN_REVISION_CHANGED", "COMMAND_SNAPSHOT_MISMATCH", "DOMAIN_PRECONDITION_REJECTED":
		return FailureFactOrPolicyChanged
	case "DOMAIN_RESOURCE_NOT_FOUND":
		return FailurePermanent
	default:
		return FailureUnknown
	}
}

func classifyTokenFailure(code string) FailureClass {
	// DOMAIN_EXECUTION_FAILED currently collapses FAILED and REJECTED and
	// carries no proof of zero effect. Token's result reference must be queried.
	return FailureUnknown
}
