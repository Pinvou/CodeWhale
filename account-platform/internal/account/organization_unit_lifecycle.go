package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type OrganizationUnitDeletionAssessment struct {
	OrganizationID   uuid.UUID                      `json:"organization_id"`
	UnitID           uuid.UUID                      `json:"unit_id"`
	Status           string                         `json:"status"`
	StructureVersion int64                          `json:"structure_version"`
	Ready            bool                           `json:"ready"`
	Blockers         []OrganizationLifecycleBlocker `json:"blockers"`
}

type OrganizationUnitLifecycleClient interface {
	ApplyUnitLifecycle(context.Context, uuid.UUID, uuid.UUID, string, int64) (OrganizationUnitDeletionAssessment, error)
}

func (c *HTTPOrganizationLifecycleClient) ApplyUnitLifecycle(ctx context.Context, organizationID, unitID uuid.UUID, status string, structureVersion int64) (OrganizationUnitDeletionAssessment, error) {
	endpoint := *c.baseURL
	endpoint.Path = "/internal/v1/organizations/" + organizationID.String() + "/units/" + unitID.String() + "/lifecycle"
	body, err := json.Marshal(map[string]any{"status": status, "structure_version": structureVersion})
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	if err := serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	if len(responseBody) > 1<<20 {
		return OrganizationUnitDeletionAssessment{}, errors.New("organization lifecycle response exceeds its bound")
	}
	if response.StatusCode != http.StatusOK {
		return OrganizationUnitDeletionAssessment{}, fmt.Errorf("%s organization unit deletion preflight returned HTTP %d", c.domain, response.StatusCode)
	}
	var assessment OrganizationUnitDeletionAssessment
	if err := json.Unmarshal(responseBody, &assessment); err != nil {
		return OrganizationUnitDeletionAssessment{}, fmt.Errorf("decode %s organization unit deletion preflight: %w", c.domain, err)
	}
	if assessment.OrganizationID != organizationID || assessment.UnitID != unitID || assessment.Status != status || assessment.StructureVersion != structureVersion {
		return OrganizationUnitDeletionAssessment{}, fmt.Errorf("%s organization unit deletion preflight returned a conflicting target", c.domain)
	}
	for index := range assessment.Blockers {
		assessment.Blockers[index].Domain = c.domain
	}
	if assessment.Ready != (len(assessment.Blockers) == 0) {
		return OrganizationUnitDeletionAssessment{}, errors.New("organization unit lifecycle returned inconsistent readiness")
	}
	return assessment, nil
}

func (s *Service) applyOrganizationUnitLifecycle(ctx context.Context, organizationID, unitID uuid.UUID, status string, structureVersion int64) (OrganizationUnitDeletionAssessment, error) {
	if len(s.organizationUnitLifecycle) == 0 {
		return OrganizationUnitDeletionAssessment{}, errors.New("organization unit lifecycle authorities are required")
	}
	combined := OrganizationUnitDeletionAssessment{OrganizationID: organizationID, UnitID: unitID, Status: status, StructureVersion: structureVersion, Ready: true, Blockers: []OrganizationLifecycleBlocker{}}
	for _, client := range s.organizationUnitLifecycle {
		assessment, err := client.ApplyUnitLifecycle(ctx, organizationID, unitID, status, structureVersion)
		if err != nil {
			return OrganizationUnitDeletionAssessment{}, err
		}
		combined.Blockers = append(combined.Blockers, assessment.Blockers...)
	}
	combined.Ready = len(combined.Blockers) == 0
	return combined, nil
}
