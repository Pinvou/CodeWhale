package token

import (
	"context"

	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func (s *Service) paginatedUsage(w http.ResponseWriter, r *http.Request) bool {
	filter, err := parseAdminQuery(r.URL.Query(), []string{"q", "execution_status", "settlement_status", "model", "token_account_id", "organization_id", "started_from", "started_to"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	source, err := usageSnapshotSource(filter)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return true
	}
	_, paginationOK := s.paginationRepository()
	repo, ok := s.repository.(usageQueryRepository)
	if !paginationOK || !ok {
		return false
	}
	return s.adminQueryPage(w, r, filter, "usage-requests", "platform", adminUsageSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) { return repo.adminUsageByIDs(ctx, ids) })
}
func (s *Service) adminReconciliationCases(w http.ResponseWriter, r *http.Request) {
	filter, err := parseAdminQuery(r.URL.Query(), []string{"q", "status", "severity", "case_type", "owner_id", "sla_overdue"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	source, err := reconciliationSnapshotSource(filter)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	_, paginationOK := s.paginationRepository()
	repo, ok := s.repository.(usageQueryRepository)
	if !paginationOK || !ok {
		api.WriteError(w, 503, "ADMIN_UNAVAILABLE", "token administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if !s.adminQueryPage(w, r, filter, "reconciliation-cases", "platform", adminReconciliationSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) { return repo.adminReconciliationByIDs(ctx, ids) }) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}
func (s *Service) adminUsageEvidence(w http.ResponseWriter, r *http.Request) {
	usageID, ok := adminUUID(w, r, "usage_request_id")
	if !ok {
		return
	}
	filter, err := parseAdminQuery(r.URL.Query(), []string{"evidence_type"})
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	source, err := evidenceSnapshotSource(filter, usageID)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	_, paginationOK := s.paginationRepository()
	repo, ok := s.repository.(usageQueryRepository)
	if !paginationOK || !ok {
		api.WriteError(w, 503, "ADMIN_UNAVAILABLE", "token administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if !s.adminQueryPage(w, r, filter, "usage-evidence", usageID.String(), adminUsageEvidenceSort, 50, 200, source, func(ctx context.Context, ids []string) (any, error) { return repo.adminEvidenceByIDs(ctx, ids) }) {
		writeAdminQueryError(w, r, ErrAdminPaginationDisabled)
	}
}
