package account

import (
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
)

func TestApprovalPolicyForItemUsesFrozenPermissions(t *testing.T) {
	for _, commandType := range []string{"PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME"} {
		policy, err := approvalpolicy.Lookup(commandType)
		if err != nil {
			t.Fatal(err)
		}
		item := ApprovalRequest{
			CommandType:         policy.CommandType,
			PolicyVersion:       policy.PolicyVersion,
			ApprovalMode:        string(policy.Mode),
			RequesterPermission: policy.RequesterPermission,
			ApproverPermission:  policy.ApproverPermission,
		}
		resolved, err := approvalPolicyForItem(item)
		if err != nil || resolved.ApproverPermission != "platform.billing.payment-controls.resume.approve" {
			t.Fatalf("expected payment-control recovery policy, got %#v, %v", resolved, err)
		}
		item.ApproverPermission = "platform.billing.repairs.approve"
		if _, err := approvalPolicyForItem(item); !errors.Is(err, ErrApprovalInvalidated) {
			t.Fatalf("modified frozen permission must invalidate approval, got %v", err)
		}
	}
}

func TestApprovalAllowedActionsRequireExactCurrentCapability(t *testing.T) {
	requester, approver := uuid.New(), uuid.New()
	item := ApprovalRequest{CommandType: "WALLET_ADJUSTMENT", RequesterUserID: requester, Status: "SUBMITTED"}
	if actions := approvalAllowedActions(item, requester, false, false, true); len(actions) != 0 {
		t.Fatalf("revoked requester must have no action, got %v", actions)
	}
	if actions := approvalAllowedActions(item, requester, true, false, true); len(actions) != 1 || actions[0] != "WITHDRAW" {
		t.Fatalf("authorized requester must be able to withdraw, got %v", actions)
	}
	if actions := approvalAllowedActions(item, approver, false, true, true); len(actions) != 2 || actions[0] != "APPROVE" || actions[1] != "REJECT" {
		t.Fatalf("authorized independent approver must be able to decide, got %v", actions)
	}
}
