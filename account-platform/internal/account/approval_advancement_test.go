package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/google/uuid"
)

type advancementActivationReader struct {
	state string
	calls int
	err   error
}

type billingAdvancementReader struct {
	snapshot billinggovernance.Snapshot
	calls    int
	err      error
}

func (r *billingAdvancementReader) Snapshot(context.Context) (billinggovernance.Snapshot, error) {
	r.calls++
	return r.snapshot, r.err
}

func billingAdvancementSnapshot(state string) billinggovernance.Snapshot {
	now := time.Now().UTC()
	rows := []billinggovernance.Activation{
		{CommandType: "CHARGEBACK_FINANCIAL_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: now},
		{CommandType: "ENTITLEMENT_COMPONENT_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: now},
		{CommandType: "LEDGER_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: now},
		{CommandType: "PAYMENT_FINANCIAL_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: now},
		{CommandType: "PAYMENT_PRODUCT_RESUME", PolicyVersion: 1, SnapshotSchemaVersion: 1, State: state, Version: 1, UpdatedAt: now},
		{CommandType: "PAYMENT_PROVIDER_RESUME", PolicyVersion: 2, SnapshotSchemaVersion: 1, State: state, Version: 1, UpdatedAt: now},
		{CommandType: "RECEIVABLE_WRITE_OFF", PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: now},
		{CommandType: "RECONCILIATION_FINANCIAL_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: now},
		{CommandType: "REFUND_EXCEPTION_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: now},
		{CommandType: "USAGE_SETTLEMENT_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: now},
		{CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1, State: state, Version: 1, UpdatedAt: now},
	}
	return billinggovernance.Snapshot{SchemaVersion: 1, Activations: rows}
}

// billingAdvancementSnapshotWithCorrectionV2 models the only activatable
// projection of the entitlement correction command: the appended policy v2 /
// snapshot schema v2 row replaces the v1 row in the projection.
func billingAdvancementSnapshotWithCorrectionV2(state string) billinggovernance.Snapshot {
	snapshot := billingAdvancementSnapshot(state)
	snapshot.Activations[1] = billinggovernance.Activation{
		CommandType: "ENTITLEMENT_COMPONENT_ADJUSTMENT", PolicyVersion: 2, SnapshotSchemaVersion: 2,
		State: state, Version: 1, UpdatedAt: time.Now().UTC(),
	}
	return snapshot
}

func (r *advancementActivationReader) Snapshot(context.Context) (tokengovernance.Snapshot, error) {
	r.calls++
	return tokengovernance.Snapshot{State: r.state, Epoch: 1}, r.err
}

func TestApprovalAdvancementProjection(t *testing.T) {
	for _, variant := range []struct {
		name     string
		snapshot billinggovernance.Snapshot
	}{
		{"correction-inactive", billingAdvancementSnapshot("ACTIVE")},
		{"correction-v2-active", billingAdvancementSnapshotWithCorrectionV2("ACTIVE")},
	} {
		t.Run(variant.name, func(t *testing.T) { assertApprovalAdvancementProjection(t, variant.snapshot) })
	}
}

func assertApprovalAdvancementProjection(t *testing.T, billingSnapshot billinggovernance.Snapshot) {
	requester, approver := uuid.New(), uuid.New()
	for _, state := range []string{"PRE_ACTIVATION", "ACTIVE", "SAFE_ROLLBACK", "UNAVAILABLE"} {
		reader := &advancementActivationReader{state: state}
		billingReader := &billingAdvancementReader{snapshot: billingSnapshot}
		if state == "UNAVAILABLE" {
			reader.err = errors.New("unavailable")
		}
		projection := (&PostgresRepository{}).WithTokenGovernanceActivation(reader).WithBillingGovernanceActivation(billingReader).approvalAdvancementProjection(context.Background())
		for _, policy := range approvalpolicy.All() {
			if policy.Mode != approvalpolicy.ApprovalRequired {
				continue
			}
			advance := projection(policy)
			billingActive := false
			if activation, ok := billingSnapshot.Find(policy.CommandType, policy.PolicyVersion, policy.SnapshotSchemaVersion); ok {
				billingActive = activation.State == "ACTIVE"
			}
			want := approvalpolicy.Executable(policy.CommandType) &&
				(approvalpolicy.TokenExecutableApproval(policy.CommandType) && state == "ACTIVE" ||
					!approvalpolicy.TokenExecutableApproval(policy.CommandType) && billingActive)
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
			if got := approvalAllowedActions(item, requester, true, false, advance, true); !slices.Equal(got, draftActions) {
				t.Fatalf("draft %s/%s: %v", state, policy.CommandType, got)
			}
			item.Status = "SUBMITTED"
			if got := approvalAllowedActions(item, approver, false, true, advance, true); !slices.Equal(got, approvalActions) {
				t.Fatalf("approve %s/%s: %v", state, policy.CommandType, got)
			}
			if got := approvalAllowedActions(item, requester, true, false, advance, true); !slices.Equal(got, []string{"WITHDRAW"}) {
				t.Fatalf("withdraw: %v", got)
			}
		}
		if reader.calls != 1 {
			t.Fatalf("projection must observe Token once per response, calls=%d", reader.calls)
		}
		if billingReader.calls != 1 {
			t.Fatalf("projection must observe Billing once per response, calls=%d", billingReader.calls)
		}
	}
}

func TestApprovalAdvancementRegistryAndActivation(t *testing.T) {
	billingVariants := []struct {
		name     string
		snapshot billinggovernance.Snapshot
	}{
		{"correction-inactive", billingAdvancementSnapshot("ACTIVE")},
		{"correction-v2-active", billingAdvancementSnapshotWithCorrectionV2("ACTIVE")},
	}
	for _, state := range []string{"PRE_ACTIVATION", "ACTIVE", "SAFE_ROLLBACK"} {
		for _, variant := range billingVariants {
			for _, policy := range approvalpolicy.All() {
				if policy.Mode != approvalpolicy.ApprovalRequired {
					continue
				}
				t.Run(state+"/"+variant.name+"/"+policy.CommandType, func(t *testing.T) {
					reader := &advancementActivationReader{state: state}
					billingReader := &billingAdvancementReader{snapshot: variant.snapshot}
					repository := (&PostgresRepository{}).WithTokenGovernanceActivation(reader).WithBillingGovernanceActivation(billingReader)
					err := repository.requireApprovalAdvancement(context.Background(), policy)
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
						if reader.calls != 0 || billingReader.calls != 1 {
							t.Fatalf("Billing activation observation mismatch: %v token=%d billing=%d", err, reader.calls, billingReader.calls)
						}
						activation, ok := variant.snapshot.Find(policy.CommandType, policy.PolicyVersion, policy.SnapshotSchemaVersion)
						switch {
						case !ok:
							if !errors.Is(err, ErrBillingGovernanceUnavailable) {
								t.Fatalf("missing Billing activation row: %v", err)
							}
						case activation.State == "ACTIVE":
							if err != nil {
								t.Fatalf("active Billing activation rejected: %v", err)
							}
						default:
							if !errors.Is(err, ErrBillingGovernanceInactive) {
								t.Fatalf("inactive Billing activation accepted: %v", err)
							}
						}
					}
				})
			}
		}
	}
	if err := (&PostgresRepository{}).requireApprovalAdvancement(context.Background(), approvalpolicy.Policy{CommandType: "UNKNOWN_COMMAND"}); !errors.Is(err, ErrApprovalCommandNotExecutable) {
		t.Fatalf("unknown command must fail closed: %v", err)
	}
}

func TestBillingApprovalAdvancementFailsClosed(t *testing.T) {
	for _, commandType := range []string{"WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME"} {
		policy, err := approvalpolicy.Lookup(commandType)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name   string
			reader billinggovernance.Reader
			want   error
		}{
			{name: "active", reader: &billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")}},
			{name: "missing-reader", want: ErrBillingGovernanceUnavailable},
			{name: "inactive", reader: &billingAdvancementReader{snapshot: billingSnapshotWithCommandState(commandType, "INACTIVE")}, want: ErrBillingGovernanceInactive},
			{name: "transport", reader: &billingAdvancementReader{err: errors.New("unavailable")}, want: ErrBillingGovernanceUnavailable},
			{name: "missing-row", reader: &billingAdvancementReader{snapshot: billingSnapshotWithoutCommand(commandType)}, want: ErrBillingGovernanceUnavailable},
			{name: "wrong-policy", reader: &billingAdvancementReader{snapshot: billingSnapshotWithWrongBinding(commandType, true)}, want: ErrBillingGovernanceUnavailable},
			{name: "wrong-snapshot-schema", reader: &billingAdvancementReader{snapshot: billingSnapshotWithWrongBinding(commandType, false)}, want: ErrBillingGovernanceUnavailable},
		} {
			t.Run(commandType+"/"+test.name, func(t *testing.T) {
				repository := (&PostgresRepository{}).WithBillingGovernanceActivation(test.reader)
				if err := repository.requireApprovalAdvancement(context.Background(), policy); !errors.Is(err, test.want) {
					t.Fatalf("got %v want %v", err, test.want)
				}
			})
		}
	}
	policy, err := approvalpolicy.Lookup("WALLET_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	legacyProvider := policy
	legacyProvider.CommandType = "PAYMENT_PROVIDER_RESUME"
	legacyProvider.PolicyVersion = 1
	if err := (&PostgresRepository{}).WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")}).requireApprovalAdvancement(context.Background(), legacyProvider); !errors.Is(err, ErrBillingGovernanceUnavailable) {
		t.Fatalf("legacy provider policy version must not use current activation: %v", err)
	}
}

func billingSnapshotWithCommandState(commandType, state string) billinggovernance.Snapshot {
	snapshot := billingAdvancementSnapshot("ACTIVE")
	for index := range snapshot.Activations {
		if snapshot.Activations[index].CommandType == commandType {
			snapshot.Activations[index].State = state
		}
	}
	return snapshot
}

func billingSnapshotWithoutCommand(commandType string) billinggovernance.Snapshot {
	snapshot := billingAdvancementSnapshot("ACTIVE")
	for index := range snapshot.Activations {
		if snapshot.Activations[index].CommandType == commandType {
			snapshot.Activations = append(snapshot.Activations[:index], snapshot.Activations[index+1:]...)
			break
		}
	}
	return snapshot
}

func billingSnapshotWithWrongBinding(commandType string, policyVersion bool) billinggovernance.Snapshot {
	snapshot := billingAdvancementSnapshot("ACTIVE")
	for index := range snapshot.Activations {
		if snapshot.Activations[index].CommandType != commandType {
			continue
		}
		if policyVersion {
			snapshot.Activations[index].PolicyVersion++
		} else {
			snapshot.Activations[index].SnapshotSchemaVersion++
		}
	}
	return snapshot
}

func TestBillingApprovalProjectionRejectsSnapshotReturnedWithError(t *testing.T) {
	policy, err := approvalpolicy.Lookup("WALLET_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	reader := &billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE"), err: errors.New("late transport failure")}
	projection := (&PostgresRepository{}).WithBillingGovernanceActivation(reader).approvalAdvancementProjection(context.Background())
	if projection(policy) || reader.calls != 1 {
		t.Fatalf("snapshot returned with error must fail closed, calls=%d", reader.calls)
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
		if got := approvalAllowedActions(item, requester, true, false, false, true); !slices.Equal(got, []string{"WITHDRAW"}) {
			t.Fatalf("%s requester actions: %v", policy.CommandType, got)
		}
		if got := approvalAllowedActions(item, approver, false, true, false, true); !slices.Equal(got, []string{"REJECT"}) {
			t.Fatalf("%s approver actions: %v", policy.CommandType, got)
		}
		if got := approvalAllowedActions(item, requester, false, true, false, true); len(got) != 0 {
			t.Fatalf("self/revoked actions: %v", got)
		}
		item.Status = "DRAFT"
		if got := approvalAllowedActions(item, requester, true, false, false, true); len(got) != 0 {
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

func TestBillingGovernanceHTTPErrors(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{ErrBillingGovernanceInactive, "BILLING_COMMAND_GOVERNANCE_INACTIVE"},
		{ErrBillingGovernanceUnavailable, "BILLING_COMMAND_GOVERNANCE_UNAVAILABLE"},
	} {
		recorder := httptest.NewRecorder()
		(&Service{}).writeError(recorder, httptest.NewRequest(http.MethodPost, "/", nil), test.err)
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"`+test.code+`"`) {
			t.Fatalf("unexpected response for %s: %d %s", test.code, recorder.Code, recorder.Body.String())
		}
	}
}
