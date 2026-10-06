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
	service := NewService(&fakeRepository{}, &fakeWalletProvisioner{}).WithOrganizationLifecycle(
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
	service := NewService(&fakeRepository{}, &fakeWalletProvisioner{}).WithOrganizationLifecycle(
		fakeOrganizationLifecycleClient{domain: "BILLING", calls: &calls, blockers: []OrganizationLifecycleBlocker{{Code: "BALANCE"}}},
		fakeOrganizationLifecycleClient{domain: "TOKEN", calls: &calls, blockers: []OrganizationLifecycleBlocker{{Code: "USAGE"}}},
	)
	assessment, err := service.preflightOrganizationLifecycleProjection(context.Background(), organizationID, "DELETED", 3)
	if err != nil || assessment.Ready || len(assessment.Blockers) != 2 {
		t.Fatalf("preflight=%#v error=%v", assessment, err)
	}
}
