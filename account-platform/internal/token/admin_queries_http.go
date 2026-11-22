package token

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/google/uuid"
)

type adminPaginationRepository interface {
	AdminPaginationPage(context.Context, AdminPaginationRequest, AdminSnapshotMemberSource) (AdminPaginationPage, error)
	reserveAdminQueryRate(context.Context, uuid.UUID, string, string, bool) error
	adminModelsByIDs(context.Context, []string) ([]AdminCatalogModel, error)
	adminModelVersionsByIDs(context.Context, uuid.UUID, []string) ([]AdminModelVersion, error)
	adminLifecyclePlansByIDs(context.Context, uuid.UUID, []string) ([]LifecyclePlan, error)
	adminProtocolsByIDs(context.Context, []string) ([]ProtocolDefinition, error)
	adminFormulasByIDs(context.Context, []string) ([]ProtocolFormulaVersion, error)
	adminTokenAccountsByIDs(context.Context, []string) ([]AdminTokenAccount, error)
	adminTokenCredentialsByIDs(context.Context, uuid.UUID, []string) ([]AdminCredentialSafeMetadata, error)
	adminTokenRoutesByIDs(context.Context, uuid.UUID, []string) ([]RoutingPolicy, error)
	adminProvisioningsByIDs(context.Context, []string) ([]RouteProvisioning, error)
	adminProvisioningDetail(context.Context, uuid.UUID) (AdminProvisioningDetail, error)
	adminProvisioningAttemptsByIDs(context.Context, uuid.UUID, []string) ([]AdminProvisioningAttempt, error)
	adminProvisioningExists(context.Context, uuid.UUID) (bool, error)
	adminAuditByIDs(context.Context, string, []string) ([]AdminResourceAudit, error)
}

type adminModelDetailRepository interface {
	adminModelDetail(context.Context, uuid.UUID) (AdminModelDetail, error)
}

type adminPaginationPageRepository interface {
	AdminPaginationPage(context.Context, AdminPaginationRequest, AdminSnapshotMemberSource) (AdminPaginationPage, error)
}

func (s *Service) paginationRepository() (adminPaginationRepository, bool) {
	repository, ok := s.repository.(adminPaginationRepository)
	return repository, ok
}

func adminAuthorizationVersion(w http.ResponseWriter) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(w.Header().Get("X-Authorization-Version")), 10, 64)
	if err != nil || value < 1 {
		return 0, errors.New("authorization version is unavailable")
	}
	return value, nil
}

func adminSubject(r *http.Request) uuid.UUID {
	principal, _ := identity.FromContext(r.Context())
	if id, err := uuid.Parse(principal.Subject); err == nil {
		return id
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(principal.Issuer+"|"+principal.Subject))
}

func writeAdminQueryError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		api.WriteError(w, http.StatusNotFound, "TOKEN_NOT_FOUND", "the token resource was not found", httpserver.RequestID(r.Context()))
		return
	}
	var rateError adminQueryRateLimitError
	if errors.As(err, &rateError) {
		w.Header().Set("Retry-After", strconv.Itoa(rateError.RetryAfter))
		api.WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", "the query rate limit was exceeded", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrAdminSnapshotResourceChanged) {
		api.WriteError(w, http.StatusGone, "SNAPSHOT_RESOURCE_CHANGED", "a resource in the snapshot is no longer available", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if requestdeadline.WriteContextDeadlineError(w, r) {
			return
		}
		class, _ := requestdeadline.Classify(r.Method, r.URL.Path, r.URL.Query().Get("cursor"))
		if class == requestdeadline.SnapshotBuild {
			w.Header().Set("Retry-After", "5")
			api.WriteError(w, http.StatusServiceUnavailable, "PAGINATION_SNAPSHOT_UNAVAILABLE", "the pagination snapshot could not be built in time", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusGatewayTimeout, "QUERY_TIMEOUT", "the paginated query timed out", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrForbidden) {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the requested resource is not visible", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrAdminQueryInvalidFilter) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", "the query filters are invalid", httpserver.RequestID(r.Context()))
		return
	}
	classified := ClassifyAdminPaginationError(err)
	if classified.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(classified.RetryAfter))
	}
	api.WriteError(w, classified.Status, classified.Code, "the paginated Token administration query could not be completed", httpserver.RequestID(r.Context()))
}

func (s *Service) trustedAdminQuerySource(r *http.Request) string {
	if source, ok := s.verifiedAdminQuerySource(r); ok {
		return source
	}
	peerText := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(peerText); err == nil {
		peerText = host
	}
	peer := net.ParseIP(peerText)
	if peer != nil {
		return peer.String()
	}
	return "untrusted"
}

func (s *Service) verifiedAdminQuerySource(r *http.Request) (string, bool) {
	peerText := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(peerText); err == nil {
		peerText = host
	}
	peer := net.ParseIP(peerText)
	if peer == nil || !peer.IsLoopback() && !peer.IsPrivate() || len(s.adminProxyHMACKey) < 32 {
		return "", false
	}
	source := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Pinvou-Client-Source")))
	if source == nil {
		return "", false
	}
	canonical := source.String()
	provided, err := hex.DecodeString(strings.TrimSpace(r.Header.Get("X-Pinvou-Client-Source-Signature")))
	if err != nil || len(provided) != sha256.Size {
		return "", false
	}
	mac := hmac.New(sha256.New, s.adminProxyHMACKey)
	_, _ = mac.Write([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n" + canonical))
	return canonical, hmac.Equal(provided, mac.Sum(nil))
}

func (s *Service) adminQueryPage(w http.ResponseWriter, r *http.Request, filter adminQueryFilter, resource, scope, sort string, defaultLimit, maximum int, source AdminSnapshotMemberSource, loader func(context.Context, []string) (any, error)) bool {
	repository, ok := s.paginationRepository()
	if !ok {
		return false
	}
	if err := repository.reserveAdminQueryRate(r.Context(), adminSubject(r), resource, s.trustedAdminQuerySource(r), resource == "owner-candidates"); err != nil {
		if !errors.Is(err, ErrAdminQueryRateLimited) {
			slog.WarnContext(r.Context(), "admin query rate reservation failed",
				"request_id", httpserver.RequestID(r.Context()),
				"resource", resource,
				"sql_state", adminPostgresSQLState(err),
			)
		}
		writeAdminQueryError(w, r, err)
		return true
	}
	limit, err := filter.limit(defaultLimit, maximum)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	authorizationVersion, err := adminAuthorizationVersion(w)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_VERSION_INVALID", "authorization version is unavailable", httpserver.RequestID(r.Context()))
		return true
	}
	page, items, err := resolveAdminQueryPage(r.Context(), repository, AdminPaginationRequest{SubjectID: adminSubject(r), AuthorizationVersion: authorizationVersion, Resource: resource, Scope: scope, Filters: filter.paginationValues(), Sort: sort, Cursor: filter.cursor(), Limit: limit}, source, loader)
	if errors.Is(err, ErrAdminPaginationDisabled) {
		legacyOnly := true
		for key := range filter.present {
			legacyOnly = legacyOnly && key == "limit"
		}
		if legacyOnly {
			return false
		}
		writeAdminQueryError(w, r, err)
		return true
	}
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	api.WriteJSON(w, http.StatusOK, AdminQueryEnvelope{Items: items, NextCursor: page.NextCursor, HasMore: page.HasMore, Limit: page.Limit, AsOf: page.AsOf, SnapshotExpiresAt: page.SnapshotExpiresAt, RequestID: httpserver.RequestID(r.Context())})
	return true
}

func resolveAdminQueryPage(ctx context.Context, repository adminPaginationPageRepository, request AdminPaginationRequest, source AdminSnapshotMemberSource, loader func(context.Context, []string) (any, error)) (AdminPaginationPage, any, error) {
	page, err := repository.AdminPaginationPage(ctx, request, source)
	if err != nil {
		return AdminPaginationPage{}, nil, err
	}
	items, err := loader(ctx, page.ResourceIDs)
	if err != nil {
		return AdminPaginationPage{}, nil, err
	}
	return page, items, nil
}

func (s *Service) paginatedModels(w http.ResponseWriter, r *http.Request) bool {
	filter, err := parseAdminQuery(r.URL.Query(), []string{"q", "lifecycle_status", "protocol_code", "source_adapter", "source_missing"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	source, err := modelSnapshotSource(filter)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	repo, ok := s.paginationRepository()
	if !ok {
		return false
	}
	return s.adminQueryPage(w, r, filter, "models", "platform", adminModelsSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) { return repo.adminModelsByIDs(ctx, ids) })
}

func (s *Service) adminModelDetail(w http.ResponseWriter, r *http.Request) {
	modelID, ok := adminUUID(w, r, "model_id")
	if !ok {
		return
	}
	repository, ok := s.repository.(adminModelDetailRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "model detail is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.adminModelDetail(r.Context(), modelID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminModelVersions(w http.ResponseWriter, r *http.Request) {
	modelID, ok := adminUUID(w, r, "model_id")
	if !ok {
		return
	}
	filter, err := parseAdminQuery(r.URL.Query(), nil)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	repository, ok := s.paginationRepository()
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "model version history is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if !s.adminQueryPage(w, r, filter, "model-versions", "model:"+modelID.String(), adminModelVersionsSort, 25, 100, modelVersionSnapshotSource(modelID), func(ctx context.Context, ids []string) (any, error) {
		return repository.adminModelVersionsByIDs(ctx, modelID, ids)
	}) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}

func (s *Service) paginatedProtocols(w http.ResponseWriter, r *http.Request) bool {
	filter, err := parseAdminQuery(r.URL.Query(), []string{"q", "status", "protocol_code", "published", "created_from", "created_to"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	source, err := protocolSnapshotSource(filter)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	repo, ok := s.paginationRepository()
	if !ok {
		return false
	}
	return s.adminQueryPage(w, r, filter, "protocols", "platform", adminProtocolsSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) { return repo.adminProtocolsByIDs(ctx, ids) })
}

func (s *Service) adminFormulas(w http.ResponseWriter, r *http.Request) {
	filter, err := parseAdminQuery(r.URL.Query(), []string{"q", "status", "protocol_definition_id", "metering_unit_code", "published", "created_from", "created_to"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	source, err := formulaSnapshotSource(filter)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	repo, ok := s.paginationRepository()
	if !ok {
		api.WriteError(w, 503, "ADMIN_UNAVAILABLE", "token administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if !s.adminQueryPage(w, r, filter, "formulas", "platform", adminFormulasSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) { return repo.adminFormulasByIDs(ctx, ids) }) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}

func (s *Service) paginatedTokenAccounts(w http.ResponseWriter, r *http.Request) bool {
	filter, err := parseAdminQuery(r.URL.Query(), []string{"q", "owner_type", "owner_id", "status", "health_status", "credential_status"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	source, err := tokenAccountSnapshotSource(filter)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	repo, ok := s.paginationRepository()
	if !ok {
		return false
	}
	return s.adminQueryPage(w, r, filter, "token-accounts", "platform", adminTokenAccountsSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) { return repo.adminTokenAccountsByIDs(ctx, ids) })
}

func (s *Service) adminTokenCredentials(w http.ResponseWriter, r *http.Request) {
	accountID, ok := adminUUID(w, r, "token_account_id")
	if !ok {
		return
	}
	filter, err := parseAdminQuery(r.URL.Query(), []string{"status"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	source, err := tokenCredentialSnapshotSource(filter, accountID)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	repo, ok := s.paginationRepository()
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "credential metadata is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if !s.adminQueryPage(w, r, filter, "token-credentials", "token-account:"+accountID.String(), adminTokenCredentialsSort, 25, 100, source, func(ctx context.Context, ids []string) (any, error) {
		return repo.adminTokenCredentialsByIDs(ctx, accountID, ids)
	}) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}

func (s *Service) adminTokenRoutes(w http.ResponseWriter, r *http.Request) {
	accountID, ok := adminUUID(w, r, "token_account_id")
	if !ok {
		return
	}
	filter, err := parseAdminQuery(r.URL.Query(), []string{"health_status"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	source, err := tokenRouteSnapshotSource(filter, accountID)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	repo, ok := s.paginationRepository()
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "token routes are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if !s.adminQueryPage(w, r, filter, "token-routes", "token-account:"+accountID.String(), adminTokenRoutesSort, 25, 100, source, func(ctx context.Context, ids []string) (any, error) {
		return repo.adminTokenRoutesByIDs(ctx, accountID, ids)
	}) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}

func (s *Service) paginatedProvisionings(w http.ResponseWriter, r *http.Request) bool {
	filter, err := parseAdminQuery(r.URL.Query(), []string{"q", "status", "model_id", "token_account_id", "scope_type", "scope_id", "sla_overdue"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	source, err := provisioningSnapshotSource(filter)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	repo, ok := s.paginationRepository()
	if !ok {
		return false
	}
	return s.adminQueryPage(w, r, filter, "provisionings", "platform", adminProvisioningsSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) { return repo.adminProvisioningsByIDs(ctx, ids) })
}

func (s *Service) adminOwnerCandidates(w http.ResponseWriter, r *http.Request) {
	filter, err := parseAdminQuery(r.URL.Query(), []string{"q", "owner_type", "status", "organization_id"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	source, err := s.ownerCandidateSnapshotSource(r, filter)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	if !s.adminQueryPage(w, r, filter, "owner-candidates", "platform", adminOwnerCandidatesSort, 20, 50, source, func(_ context.Context, ids []string) (any, error) { return decodeOwnerCandidates(ids) }) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}

func (s *Service) adminResourceAudit(w http.ResponseWriter, r *http.Request) {
	filter, err := parseAdminQuery(r.URL.Query(), []string{"resource_type", "resource_id", "action", "actor_id", "occurred_from", "occurred_to"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	resourceType, err := filter.enum("resource_type", "MODEL", "TOKEN_ACCOUNT")
	if err != nil || resourceType == "" {
		writeAdminQueryError(w, r, ErrAdminQueryInvalidFilter)
		return
	}
	requiredRead := "platform.tokens.read"
	if resourceType == "MODEL" {
		requiredRead = "platform.models.read"
	}
	if !authorization.GrantedPermission(r.Context(), requiredRead) {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the requested resource is not visible", httpserver.RequestID(r.Context()))
		return
	}
	resourceID, err := filter.uuid("resource_id")
	if err != nil || resourceID == nil {
		writeAdminQueryError(w, r, ErrAdminQueryInvalidFilter)
		return
	}
	source, err := auditSnapshotSource(filter, resourceType, *resourceID)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	repo, ok := s.paginationRepository()
	if !ok {
		api.WriteError(w, 503, "ADMIN_UNAVAILABLE", "token administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	scope := resourceType + ":" + resourceID.String()
	if !s.adminQueryPage(w, r, filter, "resource-audit:"+strings.ToLower(resourceType), scope, adminResourceAuditSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) {
		return repo.adminAuditByIDs(ctx, resourceType, ids)
	}) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}

func (s *Service) adminProvisioningAttempts(w http.ResponseWriter, r *http.Request) {
	provisioningID, ok := adminUUID(w, r, "provisioning_id")
	if !ok {
		return
	}
	filter, err := parseAdminQuery(r.URL.Query(), []string{"status"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	source, err := provisioningAttemptSnapshotSource(filter, provisioningID)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	repo, ok := s.paginationRepository()
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "token administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	exists, err := repo.adminProvisioningExists(r.Context(), provisioningID)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	if !exists {
		s.writeError(w, r, ErrNotFound)
		return
	}
	if !s.adminQueryPage(w, r, filter, "provisioning-attempts", provisioningID.String(), adminProvisioningAttemptsSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) {
		return repo.adminProvisioningAttemptsByIDs(ctx, provisioningID, ids)
	}) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}
