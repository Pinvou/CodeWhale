package account

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

type fakeOrganizationLifecycleClient struct {
	domain   string
	calls    *[]string
	blockers []OrganizationLifecycleBlocker
}

func (f fakeOrganizationLifecycleClient) Apply(_ context.Context, organizationID uuid.UUID, status string, version int64) (OrganizationLifecycleAssessment, error) {
	*f.calls = append(*f.calls, f.domain+":apply:"+status)
	return OrganizationLifecycleAssessment{OrganizationID: organizationID, Status: status, AccountVersion: version, Ready: len(f.blockers) == 0, Blockers: f.blockers}, nil
}

func (f fakeOrganizationLifecycleClient) Preflight(_ context.Context, organizationID uuid.UUID, status string, version int64) (OrganizationLifecycleAssessment, error) {
	*f.calls = append(*f.calls, f.domain+":preflight:"+status)
	return OrganizationLifecycleAssessment{OrganizationID: organizationID, Status: status, AccountVersion: version, Ready: len(f.blockers) == 0, Blockers: f.blockers}, nil
}

func TestOrganizationDeletionPreflightsAllDomainsBeforeOrderedCommit(t *testing.T) {
	organizationID := uuid.Must(uuid.NewV7())
	calls := []string{}
	service := NewService(&f06LifecycleRepository{}, &f06LifecycleWallet{}).WithOrganizationLifecycle(
		fakeOrganizationLifecycleClient{domain: "BILLING", calls: &calls},
		fakeOrganizationLifecycleClient{domain: "TOKEN", calls: &calls},
	)
	assessment, err := service.preflightOrganizationLifecycleProjection(context.Background(), organizationID, "DELETED", 3)
	if err != nil || !assessment.Ready {
		t.Fatalf("preflight=%#v error=%v", assessment, err)
	}
	assessment, err = service.applyOrganizationLifecycleProjection(context.Background(), organizationID, "DELETED", 3)
	if err != nil || !assessment.Ready {
		t.Fatalf("apply=%#v error=%v", assessment, err)
	}
	want := []string{"BILLING:preflight:DELETED", "TOKEN:preflight:DELETED", "BILLING:apply:DELETED", "TOKEN:apply:DELETED"}
	if len(calls) != len(want) {
		t.Fatalf("calls=%v", calls)
	}
	for index := range want {
		if calls[index] != want[index] {
			t.Fatalf("calls=%v want=%v", calls, want)
		}
	}
}

func TestOrganizationDeletionPreflightCollectsEveryDomainBlocker(t *testing.T) {
	organizationID := uuid.Must(uuid.NewV7())
	calls := []string{}
	service := NewService(&f06LifecycleRepository{}, &f06LifecycleWallet{}).WithOrganizationLifecycle(
		fakeOrganizationLifecycleClient{domain: "BILLING", calls: &calls, blockers: []OrganizationLifecycleBlocker{{Code: "BALANCE"}}},
		fakeOrganizationLifecycleClient{domain: "TOKEN", calls: &calls, blockers: []OrganizationLifecycleBlocker{{Code: "USAGE"}}},
	)
	assessment, err := service.preflightOrganizationLifecycleProjection(context.Background(), organizationID, "DELETED", 3)
	if err != nil || assessment.Ready || len(assessment.Blockers) != 2 {
		t.Fatalf("preflight=%#v error=%v", assessment, err)
	}
}

func TestOrganizationDeletionReadinessIsReadOnlyAndExplainsBlockers(t *testing.T) {
	organizationID := uuid.Must(uuid.NewV7())
	calls := []string{}
	repository := &f06LifecycleRepository{organizationState: &AdminOrganizationState{ID: organizationID, Status: "DISABLED", Version: 7}}
	service := NewService(repository, &f06LifecycleWallet{}).WithOrganizationLifecycle(
		fakeOrganizationLifecycleClient{domain: "BILLING", calls: &calls, blockers: []OrganizationLifecycleBlocker{{Code: "WALLET_BALANCE_NOT_ZERO", Domain: "BILLING", ResourceType: "wallet", ResourceID: organizationID.String()}}},
		fakeOrganizationLifecycleClient{domain: "TOKEN", calls: &calls},
	)
	readiness, err := service.assessOrganizationDeletion(context.Background(), repository, organizationID)
	if err != nil || readiness.Ready || readiness.CurrentStatus != "DISABLED" || readiness.AccountVersion != 7 || len(readiness.Blockers) != 1 {
		t.Fatalf("readiness=%#v error=%v", readiness, err)
	}
	if len(calls) != 2 || calls[0] != "BILLING:preflight:DELETED" || calls[1] != "TOKEN:preflight:DELETED" {
		t.Fatalf("readiness must only preflight every domain: calls=%v", calls)
	}
}

func TestOrganizationDeletionReadinessRequiresDisabledStatusBeforeDomainCalls(t *testing.T) {
	organizationID := uuid.Must(uuid.NewV7())
	calls := []string{}
	repository := &f06LifecycleRepository{organizationState: &AdminOrganizationState{ID: organizationID, Status: "ACTIVE", Version: 3}}
	service := NewService(repository, &f06LifecycleWallet{}).WithOrganizationLifecycle(fakeOrganizationLifecycleClient{domain: "BILLING", calls: &calls})
	readiness, err := service.assessOrganizationDeletion(context.Background(), repository, organizationID)
	if err != nil || readiness.Ready || len(readiness.Blockers) != 1 || readiness.Blockers[0].Code != "ORGANIZATION_NOT_DISABLED" {
		t.Fatalf("readiness=%#v error=%v", readiness, err)
	}
	if len(calls) != 0 {
		t.Fatalf("active organization should be blocked before domain calls: %v", calls)
	}
}
