package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"strings"
)

func (s *Service) requireOrganizationAdminRepository(w http.ResponseWriter, r *http.Request) (organizationAdminRepository, bool) {
	repository, available := s.repository.(organizationAdminRepository)
	if !available {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_ADMIN_UNAVAILABLE", "organization administration is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, available
}

func (s *Service) adminUserMemberships(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	userID, ok := pathUUID(w, r, "user_id")
	if !ok {
		return
	}
	items, err := repository.AdminUserMemberships(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminOrganizations(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	filter, err := parseAdminOrganizationFilter(r)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_ORGANIZATION_FILTER", "organization filters are invalid", httpserver.RequestID(r.Context()))
		return
	}
	page, err := repository.AdminOrganizations(r.Context(), filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, page)
}

func (s *Service) adminCreateOrganization(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	var body struct {
		Name           string    `json:"name"`
		OwnerUserID    uuid.UUID `json:"owner_user_id"`
		IdempotencyKey string    `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.OwnerUserID == uuid.Nil || strings.TrimSpace(body.Name) == "" {
		_ = repository.AdminRecordOrganizationCreationFailure(r.Context(), mustPrincipal(r.Context()), body.OwnerUserID, body.Name, "INVALID_REQUEST")
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "organization name and owner are required", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(body.IdempotencyKey)
	}
	principal := mustPrincipal(r.Context())
	if idempotencyKey == "" {
		fallback := sha256.Sum256([]byte(principal.Issuer + "\x00" + principal.SessionID + "\x00" + body.OwnerUserID.String() + "\x00" + strings.TrimSpace(body.Name)))
		idempotencyKey = fmt.Sprintf("portal-session:%x", fallback[:16])
	}
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 120 {
		_ = repository.AdminRecordOrganizationCreationFailure(r.Context(), principal, body.OwnerUserID, body.Name, "INVALID_IDEMPOTENCY_KEY")
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "an idempotency key between 8 and 120 characters is required", httpserver.RequestID(r.Context()))
		return
	}
	organization, err := repository.AdminCreateOrganization(r.Context(), principal, body.OwnerUserID, body.Name, idempotencyKey)
	if err != nil {
		_ = repository.AdminRecordOrganizationCreationFailure(r.Context(), principal, body.OwnerUserID, body.Name, organizationCreationFailureCode(err))
		s.writeError(w, r, err)
		return
	}
	{
		assessment, lifecycleErr := s.applyOrganizationLifecycleProjection(r.Context(), organization.ID, organization.Status, organization.Version)
		if lifecycleErr != nil || !assessment.Ready {
			api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_LIFECYCLE_UNAVAILABLE", "the organization was created but dependent lifecycle activation must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
			return
		}
	}
	api.WriteJSON(w, http.StatusCreated, organization)
}

func organizationCreationFailureCode(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "OWNER_NOT_FOUND"
	case errors.Is(err, ErrForbidden):
		return "PERMISSION_DENIED"
	case errors.Is(err, ErrConflict):
		return "NAME_OR_IDEMPOTENCY_CONFLICT"
	default:
		return "INTERNAL_ERROR"
	}
}

func (s *Service) adminRenameOrganization(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if err := repository.AdminRenameOrganization(r.Context(), mustPrincipal(r.Context()), organizationID, body.Name); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminUnits(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	tree, err := repository.AdminUnits(r.Context(), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, tree)
}

func (s *Service) adminMembers(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	filter, err := parseAdminMemberFilter(r)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_ORGANIZATION_MEMBER_FILTER", "organization member filters are invalid", httpserver.RequestID(r.Context()))
		return
	}
	page, err := repository.AdminMembers(r.Context(), organizationID, filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, page)
}

func (s *Service) adminAddMember(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body adminMemberRolesBody
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if body.UserID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "user is required", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "admin-member-add:"+organizationID.String()+":"+body.UserID.String())
	var result MembershipMutationResult
	var err error
	if body.UnitIDs == nil {
		result, err = repository.AdminAddMember(r.Context(), mustPrincipal(r.Context()), organizationID, body.UserID, body.Roles, body.UnitAdminUnitIDs, idempotencyKey)
	} else {
		scoped, ok := repository.(interface {
			AdminAddMemberWithUnits(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string, []uuid.UUID, []uuid.UUID, string) (MembershipMutationResult, error)
		})
		if !ok {
			api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNAVAILABLE", "member attachment service is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		result, err = scoped.AdminAddMemberWithUnits(r.Context(), mustPrincipal(r.Context()), organizationID, body.UserID, body.Roles, body.UnitAdminUnitIDs, body.UnitIDs, idempotencyKey)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.applyOrganizationMemberLifecycle(r.Context(), result, result.Member.Status); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE", "the current membership is stored locally but lifecycle synchronization must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	api.WriteJSON(w, status, result)
}

func (s *Service) adminReplaceMemberRoles(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	userID, ok := pathUUID(w, r, "user_id")
	if !ok {
		return
	}
	var body adminMemberRolesBody
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	member, err := repository.AdminReplaceMemberRoles(r.Context(), mustPrincipal(r.Context()), organizationID, userID, body.Roles, body.UnitAdminUnitIDs)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, member)
}

func (s *Service) adminRemoveMember(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	userID, ok := pathUUID(w, r, "user_id")
	if !ok {
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, r.URL.Query().Get("idempotency_key"), "admin-member-remove:"+organizationID.String()+":"+userID.String())
	result, err := repository.AdminRemoveMember(r.Context(), mustPrincipal(r.Context()), organizationID, userID, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.applyOrganizationMemberLifecycle(r.Context(), result, result.Member.Status); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE", "the current membership is stored locally but lifecycle synchronization must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminTransferOwnership(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		NewOwnerUserID uuid.UUID `json:"new_owner_user_id"`
		IdempotencyKey string    `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if body.NewOwnerUserID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "new owner is required", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "admin-ownership-transfer:"+organizationID.String()+":"+body.NewOwnerUserID.String())
	result, err := repository.AdminTransferOwnership(r.Context(), mustPrincipal(r.Context()), organizationID, body.NewOwnerUserID, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminCreateUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		Name           string     `json:"name"`
		ParentID       *uuid.UUID `json:"parent_id"`
		IdempotencyKey string     `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "admin-unit-create:"+organizationID.String()+":"+strings.TrimSpace(body.Name)+":"+fmt.Sprint(body.ParentID))
	if !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "an idempotency key between 8 and 120 characters is required", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.AdminCreateUnit(r.Context(), mustPrincipal(r.Context()), organizationID, body.ParentID, body.Name, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, err := s.applyOrganizationUnitLifecycle(r.Context(), organizationID, result.Unit.ID, "ACTIVE", result.StructureVersion); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNIT_LIFECYCLE_UNAVAILABLE", "the unit was created but dependent lifecycle activation must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	api.WriteJSON(w, status, result)
}

func (s *Service) adminRenameUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, ok := pathUUID(w, r, "unit_id")
	if !ok {
		return
	}
	var body struct {
		Name           string `json:"name"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "admin-unit-rename:"+organizationID.String()+":"+unitID.String()+":"+strings.TrimSpace(body.Name))
	result, err := repository.AdminRenameUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, body.Name, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminMoveUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, ok := pathUUID(w, r, "unit_id")
	if !ok {
		return
	}
	var body struct {
		ParentID                 *uuid.UUID `json:"parent_id"`
		ExpectedStructureVersion int64      `json:"expected_structure_version"`
		IdempotencyKey           string     `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization unit move request", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "admin-unit-move:"+organizationID.String()+":"+unitID.String()+":"+fmt.Sprint(body.ParentID)+":"+fmt.Sprint(body.ExpectedStructureVersion))
	if body.ExpectedStructureVersion < 1 || !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "expected_structure_version and a valid idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.AdminMoveUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, body.ParentID, body.ExpectedStructureVersion, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminDeleteUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, ok := pathUUID(w, r, "unit_id")
	if !ok {
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, r.URL.Query().Get("idempotency_key"), "admin-unit-delete:"+organizationID.String()+":"+unitID.String())
	state, err := repository.AdminUnitState(r.Context(), organizationID, unitID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if state.Unit.Status != "DELETED" {
		blockers, err := repository.AdminUnitDeletionBlockers(r.Context(), organizationID, unitID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if len(blockers) > 0 {
			s.writeOrganizationUnitLifecycleFailure(w, r, OrganizationUnitDeletionAssessment{OrganizationID: organizationID, UnitID: unitID, Status: "DELETION_PENDING", StructureVersion: state.StructureVersion, Blockers: blockers})
			return
		}
		assessment, err := s.applyOrganizationUnitLifecycle(r.Context(), organizationID, unitID, "DELETION_PENDING", state.StructureVersion)
		if err != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNIT_LIFECYCLE_UNAVAILABLE", "organization unit deletion facts could not be confirmed", httpserver.RequestID(r.Context()))
			return
		}
		if !assessment.Ready {
			s.writeOrganizationUnitLifecycleFailure(w, r, assessment)
			return
		}
	}
	result, err := repository.AdminDeleteUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, idempotencyKey)
	if err != nil {
		var blocked *OrganizationUnitBlockedError
		if errors.As(err, &blocked) {
			s.writeOrganizationUnitLifecycleFailure(w, r, OrganizationUnitDeletionAssessment{OrganizationID: organizationID, UnitID: unitID, Status: "DELETION_PENDING", StructureVersion: state.StructureVersion, Blockers: blocked.Blockers})
			return
		}
		s.writeError(w, r, err)
		return
	}
	assessment, err := s.applyOrganizationUnitLifecycle(r.Context(), organizationID, unitID, "DELETED", result.StructureVersion)
	if err != nil || !assessment.Ready {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNIT_LIFECYCLE_UNAVAILABLE", "the unit was deleted locally but dependent lifecycle finalization must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) writeOrganizationUnitLifecycleFailure(w http.ResponseWriter, r *http.Request, assessment OrganizationUnitDeletionAssessment) {
	api.WriteJSON(w, http.StatusConflict, api.Error{Code: "ORGANIZATION_UNIT_LIFECYCLE_BLOCKED", Message: "organization unit deletion is blocked by associated resources", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"organization_id": assessment.OrganizationID, "unit_id": assessment.UnitID, "requested_status": assessment.Status, "structure_version": assessment.StructureVersion, "blockers": assessment.Blockers}})
}

func (s *Service) adminSetOrganizationStatus(w http.ResponseWriter, r *http.Request) {
	s.adminSetOrganizationResourceStatus(w, r, "organization")
}

func (s *Service) adminDeleteOrganization(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := api.DecodeJSON(w, r, &body); err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body must be valid JSON", httpserver.RequestID(r.Context()))
			return
		}
		body.Reason = strings.TrimSpace(body.Reason)
		if body.Reason == "" || len([]rune(body.Reason)) > 500 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "reason must contain between 1 and 500 characters", httpserver.RequestID(r.Context()))
			return
		}
	}
	s.adminDeleteOrganizationResource(w, r, "organization", body.Reason)
}

func (s *Service) adminOrganizationDeletionReadiness(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	readiness, err := s.assessOrganizationDeletion(r.Context(), repository, organizationID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.writeError(w, r, err)
			return
		}
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_LIFECYCLE_UNAVAILABLE", "organization lifecycle facts could not be confirmed", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, readiness)
}

func (s *Service) writeOrganizationLifecycleFailure(w http.ResponseWriter, r *http.Request, assessment OrganizationLifecycleAssessment, err error) {
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_LIFECYCLE_UNAVAILABLE", "organization lifecycle facts could not be confirmed", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusConflict, api.Error{Code: "ORGANIZATION_LIFECYCLE_BLOCKED", Message: "organization lifecycle is blocked by unconverged facts", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"organization_id": assessment.OrganizationID, "requested_status": assessment.Status, "blockers": assessment.Blockers}})
}

func (s *Service) adminSetOrganizationResourceStatus(w http.ResponseWriter, r *http.Request, resource string) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	pathName := resource + "_id"
	resourceID, ok := pathUUID(w, r, pathName)
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	body.Status = strings.ToUpper(strings.TrimSpace(body.Status))
	if body.Status != "ACTIVE" && body.Status != "DISABLED" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "status must be ACTIVE or DISABLED", httpserver.RequestID(r.Context()))
		return
	}
	principal := mustPrincipal(r.Context())
	var err error

	state, stateErr := repository.AdminOrganizationState(r.Context(), resourceID)
	if stateErr != nil {
		s.writeError(w, r, stateErr)
		return
	}
	if state.Status == body.Status {
		if len(s.organizationLifecycle) > 0 {
			assessment, lifecycleErr := s.applyOrganizationLifecycleProjection(r.Context(), resourceID, body.Status, state.Version)
			if lifecycleErr != nil || !assessment.Ready {
				s.writeOrganizationLifecycleFailure(w, r, assessment, lifecycleErr)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	nextVersion := state.Version + 1
	if len(s.organizationLifecycle) > 0 {
		assessment, lifecycleErr := s.applyOrganizationLifecycleProjection(r.Context(), resourceID, body.Status, nextVersion)
		if lifecycleErr != nil || !assessment.Ready {
			s.writeOrganizationLifecycleFailure(w, r, assessment, lifecycleErr)
			return
		}
	}
	err = repository.AdminSetOrganizationStatus(r.Context(), principal, resourceID, body.Status)

	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminDeleteOrganizationResource(w http.ResponseWriter, r *http.Request, resource, reason string) {
	repository, ok := s.requireOrganizationAdminRepository(w, r)
	if !ok {
		return
	}
	resourceID, ok := pathUUID(w, r, resource+"_id")
	if !ok {
		return
	}
	principal := mustPrincipal(r.Context())
	var err error

	state, stateErr := repository.AdminOrganizationState(r.Context(), resourceID)
	if stateErr != nil {
		s.writeError(w, r, stateErr)
		return
	}
	if state.Status == "DELETED" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if state.Status != "DISABLED" {
		s.writeOrganizationLifecycleFailure(w, r, OrganizationLifecycleAssessment{OrganizationID: resourceID, Status: "DELETED", AccountVersion: state.Version + 1, Ready: false, Blockers: []OrganizationLifecycleBlocker{{Code: "ORGANIZATION_NOT_DISABLED", Domain: "ACCOUNT", ResourceType: "organization", ResourceID: resourceID.String()}}}, nil)
		return
	}
	blockers, blockerErr := repository.AdminOrganizationDeletionBlockers(r.Context(), resourceID)
	if blockerErr != nil {
		s.writeError(w, r, blockerErr)
		return
	}
	if len(blockers) > 0 {
		s.writeOrganizationLifecycleFailure(w, r, OrganizationLifecycleAssessment{OrganizationID: resourceID, Status: "DELETED", AccountVersion: state.Version + 1, Ready: false, Blockers: blockers}, nil)
		return
	}
	if len(s.organizationLifecycle) > 0 {
		assessment, lifecycleErr := s.confirmOrganizationDeletionProjection(r.Context(), resourceID, state.Version)
		if lifecycleErr != nil || !assessment.Ready {
			s.writeOrganizationLifecycleFailure(w, r, assessment, lifecycleErr)
			return
		}
	}
	err = repository.AdminDeleteOrganization(r.Context(), principal, resourceID, state.Version, reason)

	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
