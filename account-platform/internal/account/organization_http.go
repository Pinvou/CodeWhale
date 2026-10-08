package account

import (
	"context"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"strings"
)

type organizationRepository interface {
	ListOrganizations(context.Context, identity.Principal) ([]Organization, error)
	SetActiveContext(context.Context, identity.Principal, *uuid.UUID, int64) (Context, error)
	ListUnits(context.Context, identity.Principal, uuid.UUID, *uuid.UUID) (UnitTree, error)
	ListMembers(context.Context, identity.Principal, uuid.UUID, *uuid.UUID) ([]Member, error)
	ListQuotaMembers(context.Context, identity.Principal, uuid.UUID) ([]QuotaMember, error)
	ListMyRoleBindings(context.Context, identity.Principal) ([]RoleBinding, error)
	ListOrganizationRoleBindings(context.Context, identity.Principal, uuid.UUID) ([]RoleBinding, error)
	GrantOrganizationRoleBinding(context.Context, identity.Principal, RoleBindingGrant) (RoleBinding, error)
	RevokeOrganizationRoleBinding(context.Context, identity.Principal, uuid.UUID, uuid.UUID) (RoleBinding, error)
	ReplaceMemberRoles(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string) (Member, error)
	CreateUnit(context.Context, identity.Principal, uuid.UUID, *uuid.UUID, string, string) (UnitMutationResult, error)
	MoveUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, *uuid.UUID, int64, string) (UnitMutationResult, error)
	CreateInvitation(context.Context, identity.Principal, uuid.UUID, InvitationCreateRequest) (Invitation, error)
	ListMyInvitations(context.Context, identity.Principal) ([]Invitation, error)
	RevokeInvitation(context.Context, identity.Principal, uuid.UUID, uuid.UUID) (Invitation, error)
	AcceptInvitation(context.Context, identity.Principal, string, *uuid.UUID) (MembershipMutationResult, error)
	RevokeMembership(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (MembershipMutationResult, error)
	TransferOwnership(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (OwnershipTransferResult, error)
	Ping(context.Context) error
}

func (s *Service) requireOrganizationRepository(w http.ResponseWriter, r *http.Request) (organizationRepository, bool) {
	repository, available := s.repository.(organizationRepository)
	if !available {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNAVAILABLE", "organization service is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, available
}

func (s *Service) listUnits(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	rootUnitID, err := optionalQueryUUID(r, "root_unit_id")
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid resource identifier", httpserver.RequestID(r.Context()))
		return
	}
	tree, err := repository.ListUnits(r.Context(), mustPrincipal(r.Context()), organizationID, rootUnitID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, tree)
}

func (s *Service) listMembers(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, err := optionalQueryUUID(r, "unit_id")
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid resource identifier", httpserver.RequestID(r.Context()))
		return
	}
	members, err := repository.ListMembers(r.Context(), mustPrincipal(r.Context()), organizationID, unitID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": members})
}

func (s *Service) listQuotaMembers(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	members, err := repository.ListQuotaMembers(r.Context(), mustPrincipal(r.Context()), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": members})
}

func (s *Service) listMyRoleBindings(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	items, err := repository.ListMyRoleBindings(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) listOrganizationRoleBindings(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	items, err := repository.ListOrganizationRoleBindings(r.Context(), mustPrincipal(r.Context()), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) grantOrganizationRoleBinding(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body roleBindingGrantBody
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.GrantOrganizationRoleBinding(r.Context(), mustPrincipal(r.Context()), RoleBindingGrant{UserID: body.UserID, RoleCode: body.RoleCode, OrganizationID: &organizationID, UnitID: body.UnitID, StartsAt: body.StartsAt, ExpiresAt: body.ExpiresAt, IdempotencyKey: body.IdempotencyKey, Reason: body.Reason})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusCreated, item)
}

func (s *Service) revokeOrganizationRoleBinding(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	bindingID, ok := pathUUID(w, r, "binding_id")
	if !ok {
		return
	}
	item, err := repository.RevokeOrganizationRoleBinding(r.Context(), mustPrincipal(r.Context()), organizationID, bindingID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) replaceMemberRoles(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
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
	var body struct {
		Roles []string `json:"roles"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	member, err := repository.ReplaceMemberRoles(r.Context(), mustPrincipal(r.Context()), organizationID, userID, body.Roles)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, member)
}

func (s *Service) listOrganizations(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	organizations, err := repository.ListOrganizations(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": organizations})
}

func (s *Service) setActiveContext(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	var body struct {
		ContextType            string     `json:"context_type"`
		OrganizationID         *uuid.UUID `json:"organization_id"`
		ExpectedContextVersion int64      `json:"expected_context_version"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.ExpectedContextVersion < 1 || (body.ContextType != "PERSONAL" && body.ContextType != "ORGANIZATION") || (body.ContextType == "PERSONAL" && body.OrganizationID != nil) || (body.ContextType == "ORGANIZATION" && body.OrganizationID == nil) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid active context", httpserver.RequestID(r.Context()))
		return
	}
	context, err := repository.SetActiveContext(r.Context(), mustPrincipal(r.Context()), body.OrganizationID, body.ExpectedContextVersion)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("X-Context-Version", stringInt64(context.ContextVersion))
	w.Header().Set("X-Authorization-Version", stringInt64(context.AuthorizationVersion))
	w.Header().Set("X-Personal-Authorization-Version", stringInt64(context.PersonalAuthorizationVersion))
	w.Header().Set("X-Platform-Authorization-Version", stringInt64(context.PlatformAuthorizationVersion))
	w.Header().Set("X-Organization-Authorization-Version", stringInt64(context.OrganizationAuthorizationVersion))
	api.WriteJSON(w, http.StatusOK, context)
}

func (s *Service) createUnit(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		ParentID       *uuid.UUID `json:"parent_id"`
		Name           string     `json:"name"`
		IdempotencyKey string     `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization unit request", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "unit-create:"+organizationID.String()+":"+strings.TrimSpace(body.Name)+":"+fmt.Sprint(body.ParentID))
	if !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "an idempotency key between 8 and 120 characters is required", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.CreateUnit(r.Context(), mustPrincipal(r.Context()), organizationID, body.ParentID, body.Name, idempotencyKey)
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

func (s *Service) moveUnit(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
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
		ExpectedVersion          int64      `json:"expected_version"`
		IdempotencyKey           string     `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid unit move request", httpserver.RequestID(r.Context()))
		return
	}
	if body.ExpectedStructureVersion == 0 {
		body.ExpectedStructureVersion = body.ExpectedVersion
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "unit-move:"+organizationID.String()+":"+unitID.String()+":"+fmt.Sprint(body.ParentID)+":"+fmt.Sprint(body.ExpectedStructureVersion))
	if body.ExpectedStructureVersion < 1 || !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "expected_structure_version and a valid idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.MoveUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, body.ParentID, body.ExpectedStructureVersion, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) createInvitation(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		TargetType     string   `json:"target_type"`
		Target         string   `json:"target"`
		Email          string   `json:"email"`
		Roles          []string `json:"roles"`
		ExpiresInHours int      `json:"expires_in_hours"`
		IdempotencyKey string   `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.ExpiresInHours < 1 || body.ExpiresInHours > 24*30 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid invitation request", httpserver.RequestID(r.Context()))
		return
	}
	if strings.TrimSpace(body.TargetType) == "" && strings.TrimSpace(body.Email) != "" {
		body.TargetType, body.Target = "EMAIL", body.Email
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "invitation-create:"+organizationID.String()+":"+strings.ToUpper(strings.TrimSpace(body.TargetType))+":"+strings.TrimSpace(body.Target))
	invitation, err := repository.CreateInvitation(r.Context(), mustPrincipal(r.Context()), organizationID, InvitationCreateRequest{TargetType: body.TargetType, Target: body.Target, Roles: body.Roles, ExpiresInHours: body.ExpiresInHours, IdempotencyKey: idempotencyKey})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, invitation)
}

func (s *Service) listMyInvitations(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	items, err := repository.ListMyInvitations(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	invitationID, ok := pathUUID(w, r, "invitation_id")
	if !ok {
		return
	}
	invitation, err := repository.RevokeInvitation(r.Context(), mustPrincipal(r.Context()), organizationID, invitationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, invitation)
}

func (s *Service) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
		return
	}

	var body struct {
		Token        string     `json:"token"`
		InvitationID *uuid.UUID `json:"invitation_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || (strings.TrimSpace(body.Token) == "") == (body.InvitationID == nil) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid invitation token", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.AcceptInvitation(r.Context(), mustPrincipal(r.Context()), body.Token, body.InvitationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.applyOrganizationMemberLifecycle(r.Context(), result, "ACTIVE"); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE", "the invitation was accepted locally but grant activation must be retried", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) revokeMembership(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
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
	idempotencyKey := organizationUnitIdempotencyKey(r, r.URL.Query().Get("idempotency_key"), "membership-remove:"+organizationID.String()+":"+userID.String())
	result, err := repository.RevokeMembership(r.Context(), mustPrincipal(r.Context()), organizationID, userID, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.applyOrganizationMemberLifecycle(r.Context(), result, "REVOKED"); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE", "the membership was revoked locally but grant suspension must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) transferOwnership(w http.ResponseWriter, r *http.Request) {
	repository, available := s.requireOrganizationRepository(w, r)
	if !available {
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
	if err := api.DecodeJSON(w, r, &body); err != nil || body.NewOwnerUserID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid ownership transfer request", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "ownership-transfer:"+organizationID.String()+":"+body.NewOwnerUserID.String())
	result, err := repository.TransferOwnership(r.Context(), mustPrincipal(r.Context()), organizationID, body.NewOwnerUserID, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}
