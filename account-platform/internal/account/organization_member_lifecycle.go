package account

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type OrganizationMemberLifecycleState struct {
	OrganizationID    uuid.UUID `json:"organization_id"`
	UserID            uuid.UUID `json:"user_id"`
	Status            string    `json:"status"`
	MembershipVersion int64     `json:"membership_version"`
}

type OrganizationMemberLifecycleClient interface {
	ApplyMemberLifecycle(context.Context, uuid.UUID, uuid.UUID, string, int64) (OrganizationMemberLifecycleState, error)
}

func (c *HTTPOrganizationLifecycleClient) ApplyMemberLifecycle(ctx context.Context, organizationID, userID uuid.UUID, status string, membershipVersion int64) (OrganizationMemberLifecycleState, error) {
	endpoint := *c.baseURL
	endpoint.Path = "/internal/v1/organizations/" + organizationID.String() + "/members/" + userID.String() + "/lifecycle"
	body, err := json.Marshal(map[string]any{"status": status, "membership_version": membershipVersion})
	if err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	if err := serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	if response.StatusCode != http.StatusOK {
		return OrganizationMemberLifecycleState{}, fmt.Errorf("%s organization member lifecycle returned HTTP %d", c.domain, response.StatusCode)
	}
	var state OrganizationMemberLifecycleState
	if err := json.Unmarshal(responseBody, &state); err != nil {
		return OrganizationMemberLifecycleState{}, err
	}
	if state.OrganizationID != organizationID || state.UserID != userID || state.Status != status || state.MembershipVersion != membershipVersion {
		return OrganizationMemberLifecycleState{}, fmt.Errorf("%s organization member lifecycle returned a conflicting target", c.domain)
	}
	return state, nil
}

func (s *Service) applyOrganizationMemberLifecycle(ctx context.Context, result MembershipMutationResult, status string) error {
	for _, client := range s.organizationMemberLifecycle {
		if _, err := client.ApplyMemberLifecycle(ctx, result.OrganizationID, result.Member.UserID, status, result.Member.Version); err != nil {
			return err
		}
	}
	return nil
}
