package token

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
)

type unitOwnershipResolver interface {
	UnitActive(context.Context, string, uuid.UUID, uuid.UUID) (bool, error)
}

// Account owns organization membership and unit lifecycle; caller UUIDs are
// never authority for associating an existing ORG_UNIT wallet with a tenant.
func (r *HTTPContextResolver) UnitActive(ctx context.Context, authorization string, organizationID, unitID uuid.UUID) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL.String()+"/v1/organizations/"+organizationID.String()+"/units", nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", authorization)
	response, err := r.client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, ErrForbidden
	}
	var body struct {
		Items []struct {
			ID             uuid.UUID `json:"unit_id"`
			OrganizationID uuid.UUID `json:"organization_id"`
			Status         string    `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&body); err != nil {
		return false, err
	}
	for _, unit := range body.Items {
		if unit.ID == unitID && unit.OrganizationID == organizationID && unit.Status == "ACTIVE" {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) unitOwnerActive(r *http.Request, ownerType string, organizationID, unitID *uuid.UUID) bool {
	if ownerType != "ORG_UNIT" {
		return true
	}
	if organizationID == nil || unitID == nil {
		return false
	}
	resolver, ok := s.contexts.(unitOwnershipResolver)
	if !ok {
		return false
	}
	active, err := resolver.UnitActive(r.Context(), r.Header.Get("Authorization"), *organizationID, *unitID)
	return err == nil && active
}

func (s *Service) ownerAllowed(r *http.Request, resolved AuthorizationContext, ownerType string, ownerID uuid.UUID, organizationID, unitID *uuid.UUID) bool {
	return authorizeOwner(resolved, ownerType, ownerID, organizationID, unitID) && s.unitOwnerActive(r, ownerType, organizationID, unitID)
}
