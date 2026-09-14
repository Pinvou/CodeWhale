package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/google/uuid"
)

type advancementActivationReader struct {
	state string
	calls int
	err   error
}

func (r *advancementActivationReader) Snapshot(context.Context) (tokengovernance.Snapshot, error) {
	r.calls++
	return tokengovernance.Snapshot{State: r.state, Epoch: 1}, r.err
}

func TestApprovalAdvancementProjection(t *testing.T) {
	requester, approver := uuid.New(), uuid.New()
	for _, state := range []string{"PRE_ACTIVATION", "ACTIVE", "SAFE_ROLLBACK", "UNAVAILABLE"} {
		reader := &advancementActivationReader{state: state}
		if state == "UNAVAILABLE" {
			reader.err = errors.New("unavailable")
		}
		projection := (&PostgresRepository{}).WithTokenGovernanceActivation(reader).approvalAdvancementProjection(context.Background())
		for _, policy := range approvalpolicy.All() {
			if policy.Mode != approvalpolicy.ApprovalRequired {
				continue
			}
			advance := projection(policy.CommandType)
			want := approvalpolicy.Executable(policy.CommandType) && (!approvalpolicy.TokenExecutableApproval(policy.CommandType) || state == "ACTIVE")
			if advance != want {
				t.Fatalf("%s/%s projection=%v", state, policy.CommandType, advance)
			}
			item := ApprovalRequest{CommandType: policy.CommandType, RequesterUserID: requester, Status: "DRAFT"}
			draftActions := []string{}
			approvalActions := []string{"REJECT"}
			if want {
				draftActions = []string{"EDIT", "SUBMIT"}
				approvalActions = []string{"APPROVE", "REJECT"}
			}
			if got := approvalAllowedActions(item, requester, true, false, advance); !slices.Equal(got, draftActions) {
				t.Fatalf("draft %s/%s: %v", state, policy.CommandType, got)
			}
			item.Status = "SUBMITTED"
			if got := approvalAllowedActions(item, approver, false, true, advance); !slices.Equal(got, approvalActions) {
				t.Fatalf("approve %s/%s: %v", state, policy.CommandType, got)
			}
			if got := approvalAllowedActions(item, requester, true, false, advance); !slices.Equal(got, []string{"WITHDRAW"}) {
				t.Fatalf("withdraw: %v", got)
			}
		}
		if reader.calls != 1 {
			t.Fatalf("projection must observe Token once per response, calls=%d", reader.calls)
		}
	}
}

func TestApprovalAdvancementRegistryAndActivation(t *testing.T) {
	for _, state := range []string{"PRE_ACTIVATION", "ACTIVE", "SAFE_ROLLBACK"} {
		for _, policy := range approvalpolicy.All() {
			if policy.Mode != approvalpolicy.ApprovalRequired {
				continue
			}
			t.Run(state+"/"+policy.CommandType, func(t *testing.T) {
				reader := &advancementActivationReader{state: state}
				repository := (&PostgresRepository{}).WithTokenGovernanceActivation(reader)
				err := repository.requireApprovalAdvancement(context.Background(), policy.CommandType)
				switch {
				case !approvalpolicy.Executable(policy.CommandType):
					if !errors.Is(err, ErrApprovalCommandNotExecutable) || reader.calls != 0 {
						t.Fatalf("policy-only must fail before domain access: %v, calls=%d", err, reader.calls)
					}
				case approvalpolicy.TokenExecutableApproval(policy.CommandType):
					if reader.calls != 1 || (state == "ACTIVE" && err != nil) || (state != "ACTIVE" && !errors.Is(err, ErrTokenGovernanceInactive)) {
						t.Fatalf("Token activation mismatch: %v, calls=%d", err, reader.calls)
					}
				default:
					if err != nil || reader.calls != 0 {
						t.Fatalf("existing Billing commands must not depend on Token: %v", err)
					}
				}
			})
		}
	}
	if err := (&PostgresRepository{}).requireApprovalAdvancement(context.Background(), "UNKNOWN_COMMAND"); !errors.Is(err, ErrApprovalCommandNotExecutable) {
		t.Fatalf("unknown command must fail closed: %v", err)
	}
}

func TestPolicyOnlyActionsPreserveSafeClosure(t *testing.T) {
	requester, approver := uuid.New(), uuid.New()
	for _, policy := range approvalpolicy.All() {
		if policy.Mode != approvalpolicy.ApprovalRequired || approvalpolicy.Executable(policy.CommandType) {
			continue
		}
		draft := approvalTestDraft()
		draft.CommandType = policy.CommandType
		if _, _, err := validateApprovalDraftRequest(draft); err == nil {
			t.Fatalf("%s draft create/update must be rejected", policy.CommandType)
		}
		item := ApprovalRequest{CommandType: policy.CommandType, RequesterUserID: requester, Status: "SUBMITTED"}
		if got := approvalAllowedActions(item, requester, true, false, false); !slices.Equal(got, []string{"WITHDRAW"}) {
			t.Fatalf("%s requester actions: %v", policy.CommandType, got)
		}
		if got := approvalAllowedActions(item, approver, false, true, false); !slices.Equal(got, []string{"REJECT"}) {
			t.Fatalf("%s approver actions: %v", policy.CommandType, got)
		}
		if got := approvalAllowedActions(item, requester, false, true, false); len(got) != 0 {
			t.Fatalf("self/revoked actions: %v", got)
		}
		item.Status = "DRAFT"
		if got := approvalAllowedActions(item, requester, true, false, false); len(got) != 0 {
			t.Fatalf("inactive draft actions: %v", got)
		}
	}
}

func TestApprovalNonExecutableHTTPError(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Service{}).writeError(recorder, httptest.NewRequest(http.MethodPost, "/", nil), ErrApprovalCommandNotExecutable)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"APPROVAL_COMMAND_NOT_EXECUTABLE"`) {
		t.Fatalf("unexpected response: %d %s", recorder.Code, recorder.Body.String())
	}
}
