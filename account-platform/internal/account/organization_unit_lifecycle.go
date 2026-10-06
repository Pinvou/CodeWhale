package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

type organizationUnitLifecycleState struct {
	OrganizationStatus  string
	OrganizationVersion int64
	UnitStatus          string
	StructureVersion    int64
}

func (r *PostgresRepository) currentOrganizationUnitLifecycle(ctx context.Context, organizationID, unitID uuid.UUID) (organizationUnitLifecycleState, error) {
	var state organizationUnitLifecycleState
	err := r.pool.QueryRow(ctx, `SELECT o.status,o.version,u.status,o.structure_version FROM organizations o JOIN organization_units u ON u.organization_id=o.id WHERE o.id=$1 AND u.id=$2`, organizationID, unitID).Scan(&state.OrganizationStatus, &state.OrganizationVersion, &state.UnitStatus, &state.StructureVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return organizationUnitLifecycleState{}, ErrNotFound
	}
	return state, err
}

// Command receipts describe the original mutation. Dependent lifecycle writes
// must instead use current authority, including a deletion after that receipt.
func (s *Service) synchronizeCreatedUnitLifecycle(ctx context.Context, organizationID, unitID uuid.UUID) (bool, error) {
	if len(s.organizationUnitLifecycle) == 0 && len(s.organizationLifecycle) == 0 {
		return true, nil
	}
	repository, ok := s.repository.(interface {
		currentOrganizationUnitLifecycle(context.Context, uuid.UUID, uuid.UUID) (organizationUnitLifecycleState, error)
	})
	if !ok {
		return false, errors.New("organization unit lifecycle authority is unavailable")
	}
	state, err := repository.currentOrganizationUnitLifecycle(ctx, organizationID, unitID)
	if err != nil {
		return false, err
	}
	if state.OrganizationStatus == "DISABLED" || state.OrganizationStatus == "DELETED" {
		assessment, err := s.applyOrganizationLifecycleProjection(ctx, organizationID, state.OrganizationStatus, state.OrganizationVersion)
		return assessment.Ready, err
	}
	if state.OrganizationStatus != "ACTIVE" || (state.UnitStatus != "ACTIVE" && state.UnitStatus != "DELETION_PENDING" && state.UnitStatus != "DELETED") {
		return false, errors.New("organization unit lifecycle authority is invalid")
	}
	assessment, err := s.applyOrganizationUnitLifecycle(ctx, organizationID, unitID, state.UnitStatus, state.StructureVersion)
	return assessment.Ready, err
}

func (c *HTTPOrganizationLifecycleClient) ApplyUnitLifecycle(ctx context.Context, organizationID, unitID uuid.UUID, status string, structureVersion int64) (OrganizationUnitDeletionAssessment, error) {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/organizations/" + organizationID.String() + "/units/" + unitID.String() + "/lifecycle"
	body, err := json.Marshal(map[string]any{"status": status, "structure_version": structureVersion})
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, errors.New("organization unit lifecycle request is unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.serviceToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	defer response.Body.Close()
	responseBody, err := readOrganizationLifecycleResponse(response)
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	if response.StatusCode != http.StatusOK {
		return OrganizationUnitDeletionAssessment{}, fmt.Errorf("%s organization unit deletion preflight returned HTTP %d", c.domain, response.StatusCode)
	}
	var assessment OrganizationUnitDeletionAssessment
	if err := json.Unmarshal(responseBody, &assessment); err != nil {
		return OrganizationUnitDeletionAssessment{}, errors.New("organization unit lifecycle response is invalid")
	}
	var readiness struct {
		Ready *bool `json:"ready"`
	}
	if json.Unmarshal(responseBody, &readiness) != nil || readiness.Ready == nil || (assessment.Ready && len(assessment.Blockers) != 0) {
		return OrganizationUnitDeletionAssessment{}, errors.New("organization unit lifecycle readiness is invalid")
	}
	if assessment.OrganizationID != organizationID || assessment.UnitID != unitID || assessment.Status != status || assessment.StructureVersion != structureVersion {
		return OrganizationUnitDeletionAssessment{}, fmt.Errorf("%s organization unit deletion preflight returned a conflicting target", c.domain)
	}
	for index := range assessment.Blockers {
		assessment.Blockers[index].Domain = c.domain
	}
	return assessment, nil
}

func (s *Service) applyOrganizationUnitLifecycle(ctx context.Context, organizationID, unitID uuid.UUID, status string, structureVersion int64) (OrganizationUnitDeletionAssessment, error) {
	combined := OrganizationUnitDeletionAssessment{OrganizationID: organizationID, UnitID: unitID, Status: status, StructureVersion: structureVersion, Ready: true, Blockers: []OrganizationLifecycleBlocker{}}
	for _, client := range s.organizationUnitLifecycle {
		assessment, err := client.ApplyUnitLifecycle(ctx, organizationID, unitID, status, structureVersion)
		if err != nil {
			return OrganizationUnitDeletionAssessment{}, err
		}
		combined.Blockers = append(combined.Blockers, assessment.Blockers...)
		combined.Ready = combined.Ready && assessment.Ready
	}
	return combined, nil
}
