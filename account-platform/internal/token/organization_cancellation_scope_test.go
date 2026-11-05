package token

import (
	"testing"

	"github.com/google/uuid"
)

// Original ownership survives schema upgrades with optional snapshot fields
// absent. It grants neither a foreign user nor an unspecified organization.
func TestOriginalCancellationOwnershipSurvivesHistoricalSnapshots(t *testing.T) {
	user, organization := uuid.New(), uuid.New()
	for _, snapshot := range []string{"", "{}", `{"context_type":"ORGANIZATION"}`} {
		usage := UsageRequest{UserID: user, OrganizationID: &organization, RequestSnapshot: []byte(snapshot)}
		if !cancellationOwnerMatches(usage, user) || cancellationOwnerMatches(usage, uuid.New()) || cancellationOwnerMatches(usage, uuid.Nil) {
			t.Fatal("optional historical snapshot changed original user ownership")
		}
	}
	if sameCancellationOrganization(&organization, nil) || sameCancellationOrganization(nil, &organization) || sameCancellationOrganization(&organization, ptrUUID(uuid.New())) {
		t.Fatal("explicit organizational cancellation borrowed another scope")
	}
	if !sameCancellationOrganization(nil, nil) || !sameCancellationOrganization(&organization, &organization) {
		t.Fatal("unchanged original cancellation scope was rejected")
	}
}
