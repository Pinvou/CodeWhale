package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

type memberQuotaRepository interface {
	AllocateMemberQuota(context.Context, MemberQuotaAllocation) (MemberQuotaGrant, bool, error)
	ListMemberQuotaGrants(context.Context, uuid.UUID, *uuid.UUID, int) ([]MemberQuotaGrant, error)
	RevokeMemberQuota(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) (MemberQuotaGrant, error)
}

func (s *Service) listMemberQuotaGrants(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(memberQuotaRepository)
	organizationID, err := uuid.Parse(r.PathValue("organization_id"))
	if !ok || err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member quota lookup", httpserver.RequestID(r.Context()))
		return
	}
	var memberUserID *uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("member_user_id")); raw != "" {
		parsed, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member quota lookup", httpserver.RequestID(r.Context()))
			return
		}
		memberUserID = &parsed
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := repository.ListMemberQuotaGrants(r.Context(), organizationID, memberUserID, limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type memberQuotaBatchItem struct {
	MemberUserID       uuid.UUID `json:"member_user_id"`
	PackageID          uuid.UUID `json:"package_id"`
	ComponentID        uuid.UUID `json:"component_id"`
	ComponentType      string    `json:"component_type"`
	AmountMicrocredits int64     `json:"amount_microcredits"`
	MembershipVersion  int64     `json:"membership_version"`
}

type memberQuotaBatchResult struct {
	Index   int               `json:"index"`
	Status  string            `json:"status"`
	Created bool              `json:"created,omitempty"`
	Grant   *MemberQuotaGrant `json:"grant,omitempty"`
	Code    string            `json:"code,omitempty"`
}

func (s *Service) allocateMemberQuotaBatch(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(memberQuotaRepository)
	organizationID, err := uuid.Parse(r.PathValue("organization_id"))
	var body struct {
		CreatedBy uuid.UUID              `json:"created_by"`
		Items     []memberQuotaBatchItem `json:"items"`
	}
	batchKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !ok || err != nil || api.DecodeJSON(w, r, &body) != nil || body.CreatedBy == uuid.Nil || len(body.Items) == 0 || len(body.Items) > 200 || len(batchKey) < 16 || len(batchKey) > 128 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member quota allocation batch", httpserver.RequestID(r.Context()))
		return
	}
	results := make([]memberQuotaBatchResult, 0, len(body.Items))
	for index, item := range body.Items {
		grant, created, itemErr := repository.AllocateMemberQuota(r.Context(), MemberQuotaAllocation{OrganizationID: organizationID, MemberUserID: item.MemberUserID, PackageID: item.PackageID, ComponentID: item.ComponentID, ComponentType: item.ComponentType, AmountMicrocredits: item.AmountMicrocredits, MembershipVersion: item.MembershipVersion, CreatedBy: body.CreatedBy, IdempotencyKey: deriveMemberQuotaChildKey(batchKey, index, item)})
		result := memberQuotaBatchResult{Index: index}
		if itemErr == nil {
			result.Status, result.Created, result.Grant = "SUCCEEDED", created, &grant
		} else {
			result.Status, result.Code = "FAILED", memberQuotaErrorCode(itemErr)
		}
		results = append(results, result)
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusMultiStatus, map[string]any{"items": results})
}

func deriveMemberQuotaChildKey(batchKey string, index int, item memberQuotaBatchItem) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("v1:%s:%d:%s:%s:%s", batchKey, index, item.MemberUserID, item.PackageID, item.ComponentID)))
	return hex.EncodeToString(digest[:])
}

func (s *Service) revokeMemberQuotaGrant(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(memberQuotaRepository)
	organizationID, organizationErr := uuid.Parse(r.PathValue("organization_id"))
	grantID, grantErr := uuid.Parse(r.PathValue("grant_id"))
	var body struct {
		ActorUserID uuid.UUID `json:"actor_user_id"`
		Reason      string    `json:"reason"`
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !ok || organizationErr != nil || grantErr != nil || api.DecodeJSON(w, r, &body) != nil || body.ActorUserID == uuid.Nil || strings.TrimSpace(body.Reason) == "" || len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member quota revocation", httpserver.RequestID(r.Context()))
		return
	}
	grant, err := repository.RevokeMemberQuota(r.Context(), organizationID, grantID, body.ActorUserID, body.Reason, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, grant)
}

func memberQuotaErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrInsufficientComponentCapacity):
		return "INSUFFICIENT_COMPONENT_CAPACITY"
	case errors.Is(err, ErrIdempotencyConflict):
		return "IDEMPOTENCY_CONFLICT"
	case errors.Is(err, ErrNotFound):
		return "NOT_FOUND"
	case errors.Is(err, ErrConflict):
		return "BILLING_CONFLICT"
	default:
		return "INTERNAL_ERROR"
	}
}
