package token

import (
	"context"
	"encoding/base64"
	"errors"

	"net/http"

	"strconv"
	"strings"

	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"

	"github.com/google/uuid"
)

func (s *Service) modelProtocols(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.resolveModelUsagePermission(w, r, "personal.models.read", "org.models.read"); !ok {
		return
	}
	protocols := s.executionUpstream.Protocols()
	if len(protocols) == 0 {
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_PROTOCOLS_UNAVAILABLE", "the model gateway adapter declares no supported protocol", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"adapter_version": s.executionUpstream.AdapterVersion(), "protocols": protocols})
}

func (s *Service) getUsageRequest(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveModelUsagePermission(w, r, "personal.usage.read", "org.usage.read.self")
	if !ok {
		return
	}
	usageID, err := uuid.Parse(r.PathValue("usage_request_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid usage request", httpserver.RequestID(r.Context()))
		return
	}
	usage, err := s.repository.GetUsage(r.Context(), usageID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.writeError(w, r, err)
		return
	}
	if err != nil || !usageContextMatches(resolved, usage) {
		s.writeError(w, r, ErrNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{
		"usage_request_id": usage.ID, "status": usage.Status, "execution_status": usage.ExecutionStatus, "delivery_status": usage.DeliveryStatus, "cancel_status": usage.CancelStatus, "settlement_status": usage.SettlementStatus, "display_status": usageDisplayStatus(usage),
		"model":                usage.Model,
		"maximum_microcredits": usage.MaximumMicrocredits, "actual_microcredits": usage.ActualMicrocredits,
		"cancel_requested_at": usage.CancelRequestedAt, "provider_terminal_at": usage.ProviderTerminalAt, "final_usage_observed_at": usage.FinalUsageObservedAt,
		"last_persisted_sequence": usage.LastPersistedSequence, "last_client_confirmed_sequence": usage.LastClientConfirmedSequence,
		"started_at": usage.StartedAt, "updated_at": usage.UpdatedAt,
	})
}

// A lost response header is recovered by a read of the original invocation
// identity. Missing facts never authorize a new invocation or release funds.
func (s *Service) getPersonalUsageByIdempotencyKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, ok := s.authorizePersonalUsageUser(w, r, "personal.usage.cancel")
	if !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) < 16 || len(key) > 128 {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key must contain 16 to 128 characters", httpserver.RequestID(r.Context()))
		return
	}
	usage, err := s.repository.FindUsageByIdempotency(r.Context(), userID, digestString([]byte(key)))
	if err != nil || usage.UserID != userID || usage.OrganizationID != nil {
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.writeError(w, r, err)
			return
		}
		s.writeError(w, r, ErrNotFound)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"usage_request_id": usage.ID, "execution_status": usage.ExecutionStatus, "delivery_status": usage.DeliveryStatus, "cancel_status": usage.CancelStatus, "settlement_status": usage.SettlementStatus})
}

func (s *Service) cancelUsageRequest(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authorizePersonalUsageUser(w, r, "personal.usage.cancel")
	if !ok {
		return
	}
	usageID, err := uuid.Parse(r.PathValue("usage_request_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid usage request", httpserver.RequestID(r.Context()))
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) < 16 || len(key) > 128 {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key must contain 16 to 128 characters", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	result, preDispatch, err := s.repository.RequestCancellation(r.Context(), usageID, userID, digestString([]byte(key)), cancellationFingerprint(usageID), s.executionUpstream.AdapterVersion())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if preDispatch {
		result, err = s.finishPreDispatchCancellation(context.WithoutCancel(r.Context()), usageID)
		if err != nil {
			if definitiveBillingFailure(err) {
				s.writeError(w, r, err)
				return
			}
			w.Header().Set("Location", "/v1/usage-requests/"+usageID.String())
			api.WriteJSON(w, http.StatusAccepted, result)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) listMyUsageRequests(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveModelUsagePermission(w, r, "personal.usage.read", "org.usage.read.self")
	if !ok {
		return
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be between 1 and 100", httpserver.RequestID(r.Context()))
			return
		}
		limit = parsed
	}
	var cursor *UsageHistoryCursor
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		parsed, err := decodeUsageCursor(raw)
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "cursor is invalid", httpserver.RequestID(r.Context()))
			return
		}
		cursor = &parsed
	}
	usageRequests, err := s.repository.ListUsageForContext(r.Context(), resolved, limit+1, cursor)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var nextCursor *string
	if len(usageRequests) > limit {
		last := usageRequests[limit-1]
		encoded := encodeUsageCursor(UsageHistoryCursor{StartedAt: last.StartedAt, ID: last.UsageRequestID})
		nextCursor = &encoded
		usageRequests = usageRequests[:limit]
	}
	for index := range usageRequests {
		usageRequests[index].DisplayStatus = usageDisplayStatus(UsageRequest{Status: usageRequests[index].status, ExecutionStatus: usageRequests[index].ExecutionStatus, DeliveryStatus: usageRequests[index].DeliveryStatus, CancelStatus: usageRequests[index].CancelStatus, SettlementStatus: usageRequests[index].SettlementStatus})
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": usageRequests, "next_cursor": nextCursor})
}

func (s *Service) listMyUsageTurns(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveModelUsagePermission(w, r, "personal.usage.read", "org.usage.read.self")
	if !ok {
		return
	}
	store, ok := s.repository.(usageTurnProjectionStore)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "USAGE_HISTORY_PROJECTION_UNAVAILABLE", "usage turn projection is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	ready, err := store.UsageHistoryProjectionReady(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !ready {
		api.WriteError(w, http.StatusServiceUnavailable, "USAGE_HISTORY_PROJECTION_BUILDING", "usage turn projection is being prepared", httpserver.RequestID(r.Context()))
		return
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 100 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be between 1 and 100", httpserver.RequestID(r.Context()))
			return
		}
		limit = parsed
	}
	var cursor *UsageHistoryCursor
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		if !strings.HasPrefix(raw, usageTurnCursorPrefix) {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "usage turn cursor is invalid", httpserver.RequestID(r.Context()))
			return
		}
		parsed, parseErr := decodeUsageCursor(strings.TrimPrefix(raw, usageTurnCursorPrefix))
		if parseErr != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "usage turn cursor is invalid", httpserver.RequestID(r.Context()))
			return
		}
		cursor = &parsed
	}
	usageTurns, err := store.ListUsageTurnsForContext(r.Context(), resolved, limit+1, cursor)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	usageTurns, pageCursor := paginateUsageHistoryRecords(usageTurns, limit)
	var nextCursor *string
	if pageCursor != nil {
		encoded := usageTurnCursorPrefix + encodeUsageCursor(*pageCursor)
		nextCursor = &encoded
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": usageTurns, "next_cursor": nextCursor, "view_version": "logical-turns-v1"})
}

func paginateUsageHistoryRecords(records []UserUsageTurnRecord, limit int) ([]UserUsageTurnRecord, *UsageHistoryCursor) {
	if limit < 1 || len(records) == 0 {
		return records, nil
	}
	groupCount := 0
	lastGroupKey := ""
	var lastIncludedHead *UsageHistoryCursor
	for index, record := range records {
		groupKey := "turn:" + record.LogicalTurnID
		if record.LogicalTurnID == "" {
			groupKey = "request:" + record.UsageRequestID.String()
		}
		if groupKey == lastGroupKey {
			continue
		}
		lastGroupKey = groupKey
		groupCount++
		if groupCount == limit {
			cursorStartedAt := record.cursorStartedAt
			if cursorStartedAt.IsZero() {
				cursorStartedAt = record.StartedAt
			}
			lastIncludedHead = &UsageHistoryCursor{StartedAt: cursorStartedAt, ID: record.UsageRequestID}
		}
		if groupCount > limit {
			return records[:index], lastIncludedHead
		}
	}
	return records, nil
}

func encodeUsageCursor(cursor UsageHistoryCursor) string {
	payload := cursor.StartedAt.UTC().Format(time.RFC3339Nano) + "|" + cursor.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func decodeUsageCursor(raw string) (UsageHistoryCursor, error) {
	if len(raw) > 256 {
		return UsageHistoryCursor{}, ErrForbidden
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return UsageHistoryCursor{}, err
	}
	parts := strings.SplitN(string(payload), "|", 2)
	if len(parts) != 2 {
		return UsageHistoryCursor{}, ErrForbidden
	}
	startedAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return UsageHistoryCursor{}, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return UsageHistoryCursor{}, err
	}
	return UsageHistoryCursor{StartedAt: startedAt, ID: id}, nil
}

func usageContextMatches(context AuthorizationContext, usage UsageRequest) bool {
	if usage.UserID != context.UserID {
		return false
	}
	if context.ContextType == "PERSONAL" {
		return usage.OrganizationID == nil
	}
	return context.ContextType == "ORGANIZATION" && context.OrganizationID != nil && usage.OrganizationID != nil && *context.OrganizationID == *usage.OrganizationID
}

func usageDisplayStatus(usage UsageRequest) string {
	if usage.CancelStatus == "LOCAL_CONFIRMED" {
		return "CANCELLED_BEFORE_DISPATCH"
	}
	if usage.SettlementStatus == "SETTLED" || usage.Status == "COMMITTED" {
		return "SETTLED"
	}
	if usage.SettlementStatus == "RELEASED" || usage.Status == "RELEASED" {
		return "RELEASED"
	}
	if usage.Status == "REVIEW" || usage.ExecutionStatus == "REVIEW" || usage.SettlementStatus == "REVIEW" {
		return "MANUAL_REVIEW"
	}
	if usage.DeliveryStatus == "USER_STOPPED" {
		return "DISPLAY_STOPPED_EXECUTION_CONTINUES"
	}
	switch usage.Status {
	case "CREATED", "RESERVED", "UPSTREAM_ACTIVE":
		return "IN_PROGRESS"
	case "SETTLEMENT_PENDING":
		return "SETTLEMENT_PENDING"
	default:
		return "MANUAL_REVIEW"
	}
}

type usageTurnProjectionStore interface {
	UsageHistoryProjectionReady(context.Context) (bool, error)
	ListUsageTurnsForContext(context.Context, AuthorizationContext, int, *UsageHistoryCursor) ([]UserUsageTurnRecord, error)
}

const usageTurnCursorPrefix = "turn-v1."
