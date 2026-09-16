package approvalconsumer

import "testing"

func TestProofRecoveryClassificationsAreClosedAndQueryOnly(t *testing.T) {
	for _, descriptor := range All() {
		for _, code := range []string{"", "DOMAIN_EXECUTION_FAILED", "DEPENDENCY_TIMEOUT_2", "TRANSIENT_SAFE", "WINDOW_ENDED", "DOMAIN_REVISION_CHANGED", "COMMAND_SNAPSHOT_MISMATCH", "DOMAIN_PRECONDITION_REJECTED", "DOMAIN_RESOURCE_NOT_FOUND"} {
			got := ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, code)
			want := FailureUnknown
			if descriptor.Workload == BillingWorkload {
				switch code {
				case "DOMAIN_REVISION_CHANGED", "COMMAND_SNAPSHOT_MISMATCH":
					want = FailureFactOrPolicyChanged
				case "DOMAIN_RESOURCE_NOT_FOUND":
					want = FailurePermanent
				}
			}
			if got.Class != want || got.Retryable || got.MaxDomainAttempts != 1 || !got.QueryOnly {
				t.Fatalf("%s/%d %q: %+v want class %s and query-only", descriptor.CommandType, descriptor.PolicyVersion, code, got, want)
			}
		}
		for _, version := range []int64{0, descriptor.PolicyVersion + 1} {
			got := ClassifyProofFailure(descriptor.CommandType, version, "DOMAIN_RESOURCE_NOT_FOUND")
			if got.Class != FailureUnknown || got.Retryable || !got.QueryOnly {
				t.Fatalf("unregistered version was classified: %+v", got)
			}
		}
	}
	for _, command := range []string{"", "WALLET_ADJUSTMENT ", "UNREGISTERED"} {
		got := ClassifyProofFailure(command, 1, "DOMAIN_RESOURCE_NOT_FOUND")
		if got.Class != FailureUnknown || got.Retryable || !got.QueryOnly {
			t.Fatalf("unregistered command was classified: %+v", got)
		}
	}
}
