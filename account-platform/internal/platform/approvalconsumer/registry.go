package approvalconsumer

import "slices"

const (
	AuthorizationSubject = "account.approval-command.authorized.v1"
	AckWaitSeconds       = 30
	MaxAckPending        = 64
	MaxDomainAttempts    = 12

	BillingWorkload = "billing-command-executor"
	TokenWorkload   = "token-command-executor"

	StartExisting                          = "EXISTING"
	StartQuiescingHighWatermarkPlusOne     = "QUIESCING_HIGH_WATERMARK_PLUS_ONE"
	StartLegacySharedPreFenceCompatibility = "LEGACY_SHARED_PRE_FENCE"
)

// Descriptor is the closed consumer-to-command authority mapping. A copy is
// returned to callers so no process can mutate the registry at runtime.
type Descriptor struct {
	CommandType      string `json:"command_type"`
	PolicyVersion    int64  `json:"policy_version"`
	ExecutorIdentity string `json:"executor_identity"`
	Durable          string `json:"durable"`
	Workload         string `json:"workload"`
	StartSource      string `json:"start_source"`
}

var descriptors = []Descriptor{
	{CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-wallet-adjustment-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
	{CommandType: "PAYMENT_PROVIDER_RESUME", PolicyVersion: 2, ExecutorIdentity: BillingWorkload, Durable: "billing-payment-provider-resume-executor-p2-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
	{CommandType: "PAYMENT_PRODUCT_RESUME", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-payment-product-resume-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
	{CommandType: "ENTITLEMENT_COMPONENT_ADJUSTMENT", PolicyVersion: 2, ExecutorIdentity: BillingWorkload, Durable: "billing-entitlement-component-adjustment-executor-p2-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
	{CommandType: "LEDGER_REPAIR", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-ledger-repair-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
	{CommandType: "RECEIVABLE_WRITE_OFF", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-receivable-write-off-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
	{CommandType: "PAYMENT_FINANCIAL_REPAIR", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-payment-financial-repair-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
	{CommandType: "REFUND_EXCEPTION_REPAIR", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-refund-exception-repair-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
	{CommandType: "CHARGEBACK_FINANCIAL_REPAIR", PolicyVersion: 1, ExecutorIdentity: BillingWorkload, Durable: "billing-chargeback-financial-repair-executor-p1-v1", Workload: BillingWorkload, StartSource: StartQuiescingHighWatermarkPlusOne},
	{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", PolicyVersion: 2, ExecutorIdentity: TokenWorkload, Durable: "token-model-lifecycle-executor-v1", Workload: TokenWorkload, StartSource: StartExisting},
	{CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", PolicyVersion: 2, ExecutorIdentity: TokenWorkload, Durable: "token-catalog-rollback-executor-v1", Workload: TokenWorkload, StartSource: StartExisting},
	{CommandType: "TOKEN_CREDENTIAL_REVOKE", PolicyVersion: 1, ExecutorIdentity: TokenWorkload, Durable: "token-credential-revoke-executor-v1", Workload: TokenWorkload, StartSource: StartExisting},
}

var legacyBilling = Descriptor{
	Durable: "billing-command-executor-v1", Workload: BillingWorkload,
	ExecutorIdentity: BillingWorkload, StartSource: StartLegacySharedPreFenceCompatibility,
}

func All() []Descriptor { return slices.Clone(descriptors) }

func LegacyBilling() Descriptor { return legacyBilling }

func LookupDurable(durable string) (Descriptor, bool) {
	if durable == legacyBilling.Durable {
		return legacyBilling, true
	}
	for _, descriptor := range descriptors {
		if descriptor.Durable == durable {
			return descriptor, true
		}
	}
	return Descriptor{}, false
}

func Match(descriptor Descriptor, commandType string, policyVersion int64, executorIdentity string) bool {
	registered, ok := LookupDurable(descriptor.Durable)
	return ok && registered == descriptor && descriptor.CommandType != "" && descriptor.CommandType == commandType &&
		descriptor.PolicyVersion == policyVersion && descriptor.ExecutorIdentity == executorIdentity
}

// MatchRoute accepts the historical shared Billing durable's three frozen
// command mappings in addition to the one-command physical durables. It is for
// read-only shared-subject routing only; Match remains the execution mapping.
func MatchRoute(descriptor Descriptor, commandType string, policyVersion int64, executorIdentity string) bool {
	if descriptor == legacyBilling {
		for _, candidate := range BillingCommands() {
			if candidate.CommandType == commandType && candidate.PolicyVersion == policyVersion && candidate.ExecutorIdentity == executorIdentity {
				return true
			}
		}
		return false
	}
	return Match(descriptor, commandType, policyVersion, executorIdentity)
}

func BillingCommands() []Descriptor {
	result := make([]Descriptor, 0, 3)
	for _, descriptor := range descriptors {
		if descriptor.Workload == BillingWorkload {
			result = append(result, descriptor)
		}
	}
	return result
}

func TokenCommands() []Descriptor {
	result := make([]Descriptor, 0, 3)
	for _, descriptor := range descriptors {
		if descriptor.Workload == TokenWorkload {
			result = append(result, descriptor)
		}
	}
	return result
}
