package account

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

type fakeOrganizationUnitLifecycleClient struct {
	domain   string
	calls    *[]string
	blockers []OrganizationLifecycleBlocker
}

func (f fakeOrganizationUnitLifecycleClient) ApplyUnitLifecycle(_ context.Context, organizationID, unitID uuid.UUID, status string, structureVersion int64) (OrganizationUnitDeletionAssessment, error) {
	*f.calls = append(*f.calls, f.domain+":"+status)
	return OrganizationUnitDeletionAssessment{OrganizationID: organizationID, UnitID: unitID, Status: status, StructureVersion: structureVersion, Ready: len(f.blockers) == 0, Blockers: f.blockers}, nil
}

func TestOrganizationUnitDeletionRestrictsEveryDomainBeforeFinalization(t *testing.T) {
	organizationID := uuid.Must(uuid.NewV7())
	unitID := uuid.Must(uuid.NewV7())
	calls := []string{}
	service := NewService(&f06LifecycleRepository{}, &fakeWalletProvisioner{}).WithOrganizationUnitLifecycle(
		fakeOrganizationUnitLifecycleClient{domain: "BILLING", calls: &calls},
		fakeOrganizationUnitLifecycleClient{domain: "TOKEN", calls: &calls},
	)
	assessment, err := service.applyOrganizationUnitLifecycle(context.Background(), organizationID, unitID, "DELETION_PENDING", 4)
	if err != nil || !assessment.Ready {
		t.Fatalf("prepare=%#v error=%v", assessment, err)
	}
	assessment, err = service.applyOrganizationUnitLifecycle(context.Background(), organizationID, unitID, "DELETED", 5)
	if err != nil || !assessment.Ready {
		t.Fatalf("finalize=%#v error=%v", assessment, err)
	}
	want := []string{"BILLING:DELETION_PENDING", "TOKEN:DELETION_PENDING", "BILLING:DELETED", "TOKEN:DELETED"}
	for index := range want {
		if calls[index] != want[index] {
			t.Fatalf("calls=%v want=%v", calls, want)
		}
	}
}

func TestOrganizationUnitDeletionCollectsCrossDomainBlockers(t *testing.T) {
	organizationID := uuid.Must(uuid.NewV7())
	unitID := uuid.Must(uuid.NewV7())
	calls := []string{}
	service := NewService(&f06LifecycleRepository{}, &fakeWalletProvisioner{}).WithOrganizationUnitLifecycle(
		fakeOrganizationUnitLifecycleClient{domain: "BILLING", calls: &calls, blockers: []OrganizationLifecycleBlocker{{Code: "GRANT"}}},
		fakeOrganizationUnitLifecycleClient{domain: "TOKEN", calls: &calls, blockers: []OrganizationLifecycleBlocker{{Code: "ROUTE"}}},
	)
	assessment, err := service.applyOrganizationUnitLifecycle(context.Background(), organizationID, unitID, "DELETION_PENDING", 4)
	if err != nil || assessment.Ready || len(assessment.Blockers) != 2 {
		t.Fatalf("assessment=%#v error=%v", assessment, err)
	}
}

func TestOrganizationUnitRequestHashIncludesStructureVersionAndTarget(t *testing.T) {
	unitID := uuid.Must(uuid.NewV7())
	parentID := uuid.Must(uuid.NewV7())
	first := organizationUnitRequestHash("MOVE", unitID, &parentID, "", 4)
	if first == organizationUnitRequestHash("MOVE", unitID, &parentID, "", 5) {
		t.Fatal("structure version must be part of the command fingerprint")
	}
	if first == organizationUnitRequestHash("MOVE", uuid.Must(uuid.NewV7()), &parentID, "", 4) {
		t.Fatal("unit target must be part of the command fingerprint")
	}
}
