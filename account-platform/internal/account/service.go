package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

var ErrWalletProvisioning = errors.New("personal wallet provisioning is incomplete")

type Service struct {
	repository Repository
	wallets    WalletProvisioner
	now        func() time.Time
}

func NewService(repository Repository, wallets WalletProvisioner) *Service {
	return &Service{repository: repository, wallets: wallets, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Bootstrap(ctx context.Context, principal identity.Principal) (Me, error) {
	me, _, err := s.repository.Bootstrap(ctx, principal)
	if err != nil {
		return Me{}, err
	}
	if me.Status == "ACTIVE" {
		return me, nil
	}
	if err := s.wallets.EnsurePersonalWallet(ctx, me.UserID); err != nil {
		return me, fmt.Errorf("%w: %v", ErrWalletProvisioning, err)
	}
	if err := s.repository.ActivateUser(ctx, me.UserID); err != nil {
		return me, err
	}
	return s.repository.GetMe(ctx, principal)
}

func (s *Service) Handler(verify identity.VerifyFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/account/bootstrap", s.bootstrap)
	mux.HandleFunc("GET /v1/me", s.getMe)
	mux.HandleFunc("GET /v1/me/authorization-context", s.getAuthorizationContext)
	mux.HandleFunc("GET /v1/me/organizations", s.listOrganizations)
	mux.HandleFunc("POST /v1/sessions/current/active-context", s.setActiveContext)
	mux.HandleFunc("POST /v1/organizations", s.createOrganization)
	mux.HandleFunc("GET /v1/organizations/{organization_id}/units", s.listUnits)
	mux.HandleFunc("POST /v1/organizations/{organization_id}/units", s.createUnit)
	mux.HandleFunc("GET /v1/organizations/{organization_id}/members", s.listMembers)
	mux.HandleFunc("PUT /v1/organizations/{organization_id}/members/{user_id}/roles", s.replaceMemberRoles)
	mux.HandleFunc("PATCH /v1/organizations/{organization_id}/units/{unit_id}/parent", s.moveUnit)
	mux.HandleFunc("POST /v1/organizations/{organization_id}/invitations", s.createInvitation)
	mux.HandleFunc("POST /v1/invitations/accept", s.acceptInvitation)
	mux.HandleFunc("DELETE /v1/organizations/{organization_id}/members/{user_id}", s.revokeMembership)
	mux.HandleFunc("POST /v1/organizations/{organization_id}/ownership", s.transferOwnership)
	return identity.Middleware(verify, mux)
}

func (s *Service) listUnits(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	units, err := s.repository.ListUnits(r.Context(), mustPrincipal(r.Context()), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": units})
}

func (s *Service) listMembers(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	members, err := s.repository.ListMembers(r.Context(), mustPrincipal(r.Context()), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": members})
}

func (s *Service) replaceMemberRoles(w http.ResponseWriter, r *http.Request) {
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
	if err := api.DecodeJSON(w, r, &body); err != nil || body.Roles == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member roles request", httpserver.RequestID(r.Context()))
		return
	}
	member, err := s.repository.ReplaceMemberRoles(r.Context(), mustPrincipal(r.Context()), organizationID, userID, body.Roles)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, member)
}

func (s *Service) bootstrap(w http.ResponseWriter, r *http.Request) {
	me, err := s.Bootstrap(r.Context(), mustPrincipal(r.Context()))
	if errors.Is(err, ErrWalletProvisioning) {
		api.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"code": "BILLING_UNAVAILABLE", "message": "the account is provisioning; retry this request", "request_id": httpserver.RequestID(r.Context()), "account": me,
		})
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, me)
}

func (s *Service) getMe(w http.ResponseWriter, r *http.Request) {
	me, err := s.repository.GetMe(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, me)
}

func (s *Service) getAuthorizationContext(w http.ResponseWriter, r *http.Request) {
	resolved, err := s.repository.ResolveAuthorizationContext(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, resolved)
}

func (s *Service) listOrganizations(w http.ResponseWriter, r *http.Request) {
	organizations, err := s.repository.ListOrganizations(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": organizations})
}

func (s *Service) setActiveContext(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ContextType    string     `json:"context_type"`
		OrganizationID *uuid.UUID `json:"organization_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || (body.ContextType != "PERSONAL" && body.ContextType != "ORGANIZATION") || (body.ContextType == "PERSONAL" && body.OrganizationID != nil) || (body.ContextType == "ORGANIZATION" && body.OrganizationID == nil) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid active context", httpserver.RequestID(r.Context()))
		return
	}
	context, err := s.repository.SetActiveContext(r.Context(), mustPrincipal(r.Context()), body.OrganizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, context)
}

func (s *Service) createOrganization(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization request", httpserver.RequestID(r.Context()))
		return
	}
	organization, err := s.repository.CreateOrganization(r.Context(), mustPrincipal(r.Context()), body.Name)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, organization)
}

func (s *Service) createUnit(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		ParentID *uuid.UUID `json:"parent_id"`
		Name     string     `json:"name"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization unit request", httpserver.RequestID(r.Context()))
		return
	}
	unit, err := s.repository.CreateUnit(r.Context(), mustPrincipal(r.Context()), organizationID, body.ParentID, body.Name)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, unit)
}

func (s *Service) moveUnit(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, ok := pathUUID(w, r, "unit_id")
	if !ok {
		return
	}
	var body struct {
		ParentID        *uuid.UUID `json:"parent_id"`
		ExpectedVersion int64      `json:"expected_version"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.ExpectedVersion < 1 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid unit move request", httpserver.RequestID(r.Context()))
		return
	}
	unit, err := s.repository.MoveUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, body.ParentID, body.ExpectedVersion)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, unit)
}

func (s *Service) createInvitation(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		Email          string   `json:"email"`
		Roles          []string `json:"roles"`
		ExpiresInHours int      `json:"expires_in_hours"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.ExpiresInHours < 1 || body.ExpiresInHours > 24*30 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid invitation request", httpserver.RequestID(r.Context()))
		return
	}
	invitation, err := s.repository.CreateInvitation(r.Context(), mustPrincipal(r.Context()), organizationID, body.Email, body.Roles, s.now().Add(time.Duration(body.ExpiresInHours)*time.Hour))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, invitation)
}

func (s *Service) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || strings.TrimSpace(body.Token) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid invitation token", httpserver.RequestID(r.Context()))
		return
	}
	organization, err := s.repository.AcceptInvitation(r.Context(), mustPrincipal(r.Context()), body.Token)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, organization)
}

func (s *Service) revokeMembership(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	userID, ok := pathUUID(w, r, "user_id")
	if !ok {
		return
	}
	if err := s.repository.RevokeMembership(r.Context(), mustPrincipal(r.Context()), organizationID, userID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) transferOwnership(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		NewOwnerUserID uuid.UUID `json:"new_owner_user_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.NewOwnerUserID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid ownership transfer request", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.repository.TransferOwnership(r.Context(), mustPrincipal(r.Context()), organizationID, body.NewOwnerUserID); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "INTERNAL_ERROR", "the request could not be completed"
	switch {
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "NOT_FOUND", "the requested account resource was not found"
	case errors.Is(err, ErrForbidden):
		status, code, message = http.StatusForbidden, "ACCOUNT_FORBIDDEN", "the account action is not permitted"
	case errors.Is(err, ErrConflict):
		status, code, message = http.StatusConflict, "ACCOUNT_CONFLICT", "the account resource changed or conflicts with existing data"
	case errors.Is(err, ErrLastOwner):
		status, code, message = http.StatusConflict, "LAST_OWNER_REQUIRED", "the organization must retain at least one owner"
	case errors.Is(err, ErrInvalidHierarchy):
		status, code, message = http.StatusUnprocessableEntity, "ORG_UNIT_HIERARCHY_INVALID", "the requested organization hierarchy is invalid"
	case errors.Is(err, ErrInvitationExpired):
		status, code, message = http.StatusGone, "INVITATION_EXPIRED", "the invitation has expired"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}

func mustPrincipal(ctx context.Context) identity.Principal {
	principal, ok := identity.FromContext(ctx)
	if !ok {
		panic("identity middleware did not provide a principal")
	}
	return principal
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	value, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid resource identifier", httpserver.RequestID(r.Context()))
		return uuid.Nil, false
	}
	return value, true
}

type HTTPWalletProvisioner struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPWalletProvisioner(rawURL, serviceToken string) (*HTTPWalletProvisioner, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("BILLING_HTTP_URL must be an absolute URL")
	}
	if strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("INTERNAL_SERVICE_TOKEN is required")
	}
	return &HTTPWalletProvisioner{baseURL: parsed, serviceToken: serviceToken, client: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (p *HTTPWalletProvisioner) EnsurePersonalWallet(ctx context.Context, userID uuid.UUID) error {
	endpoint := *p.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/wallets/ensure"
	body := strings.NewReader(fmt.Sprintf(`{"owner_type":"PERSONAL","owner_id":%q}`, userID.String()))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+p.serviceToken)
	request.Header.Set("Idempotency-Key", "personal-wallet:"+userID.String())
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return fmt.Errorf("billing wallet provisioning returned HTTP %d", response.StatusCode)
	}
	return nil
}
