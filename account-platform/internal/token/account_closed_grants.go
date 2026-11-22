package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

// ClosedGrantBillingClient is deliberately separate from ordinary allocation.
// Billing verifies anonymization and commits the financial reclaim receipt.
type ClosedGrantBillingClient interface {
	ReclaimAccountClosedGrant(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) (json.RawMessage, error)
}

// A transport success is not a receipt unless its frozen target and terminal
// financial effect are consistent with this command. Decode int64 directly.
func validateClosedGrantReceipt(payload json.RawMessage, org, id uuid.UUID) error {
	var receipt struct {
		Grant struct {
			ID             uuid.UUID `json:"grant_id"`
			OrganizationID uuid.UUID `json:"organization_id"`
			Status         string    `json:"status"`
		} `json:"grant"`
		Amount    int64 `json:"amount_microcredits"`
		Lifecycle int64 `json:"subject_lifecycle_version"`
		Replayed  *bool `json:"replayed"`
	}
	if json.Unmarshal(payload, &receipt) != nil || receipt.Grant.ID != id || receipt.Grant.OrganizationID != org || receipt.Grant.Status != "RECLAIMED_ACCOUNT_CLOSED" || receipt.Amount < 1 || receipt.Lifecycle < 2 || receipt.Replayed == nil {
		return errors.New("closed grant receipt is not confirmed")
	}
	return nil
}

func (c *HTTPBillingClient) ReclaimAccountClosedGrant(ctx context.Context, organizationID, grantID, _ uuid.UUID, reason, key string) (json.RawMessage, error) {
	path := "/internal/v1/organizations/" + organizationID.String() + "/account-closed-grants/" + grantID.String() + "/reclaim"
	return c.rawDelegated(ctx, http.MethodPost, path, map[string]any{"reason": reason}, key, authorization.DelegationRequest{Audience: "billing-service", Permission: "org.grants.reclaim_closed", OrganizationID: &organizationID, TargetResourceType: "member-quota-grant", TargetResourceID: grantID.String(), HTTPMethod: http.MethodPost, HTTPPath: path})
}

func validClosedGrantInput(key, reason string) bool {
	return utf8.ValidString(key) && utf8.RuneCountInString(key) >= 16 && utf8.RuneCountInString(key) <= 128 && utf8.ValidString(reason) && utf8.RuneCountInString(reason) >= 1 && utf8.RuneCountInString(reason) <= 500
}

func (s *Service) closedGrantClient(w http.ResponseWriter, r *http.Request) (AuthorizationContext, uuid.UUID, ClosedGrantBillingClient, bool) {
	resolved, org, _, ok := s.resolveOrganizationBilling(w, r, "org.grants.reclaim_closed")
	if !ok {
		return AuthorizationContext{}, uuid.Nil, nil, false
	}
	client, ok := s.billing.(ClosedGrantBillingClient)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "closed grant reclaim is unavailable", httpserver.RequestID(r.Context()))
	}
	return resolved, org, client, ok
}

func (s *Service) reclaimClosedGrant(w http.ResponseWriter, r *http.Request) {
	resolved, org, client, ok := s.closedGrantClient(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("grant_id"))
	var body struct {
		Reason string `json:"reason"`
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if err != nil || id == uuid.Nil || api.DecodeJSON(w, r, &body) != nil || !validClosedGrantInput(key, strings.TrimSpace(body.Reason)) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid closed grant reclaim", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.ReclaimAccountClosedGrant(r.Context(), org, id, resolved.UserID, strings.TrimSpace(body.Reason), key)
	if err == nil {
		err = validateClosedGrantReceipt(payload, org, id)
	}
	if err != nil {
		s.writeClosedGrantError(w, r, err)
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) writeClosedGrantError(w http.ResponseWriter, r *http.Request, err error) {
	// An uncertain transport must preserve the original command key for replay.
	var failure *BillingHTTPError
	if errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500 {
		api.WriteError(w, failure.Status, "CLOSED_GRANT_RECLAIM_REJECTED", "the closed grant cannot be reclaimed", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "the closed grant command result is not confirmed", httpserver.RequestID(r.Context()))
}

func (s *Service) reclaimClosedGrantsBatch(w http.ResponseWriter, r *http.Request) {
	resolved, org, client, ok := s.closedGrantClient(w, r)
	if !ok {
		return
	}
	var body struct {
		GrantIDs []uuid.UUID `json:"grant_ids"`
		Reason   string      `json:"reason"`
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if api.DecodeJSON(w, r, &body) != nil || !validClosedGrantInput(key, strings.TrimSpace(body.Reason)) || len(body.GrantIDs) < 1 || len(body.GrantIDs) > 200 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid closed grant batch", httpserver.RequestID(r.Context()))
		return
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range body.GrantIDs {
		if seen[id] {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "duplicate grant identifier", httpserver.RequestID(r.Context()))
			return
		}
		seen[id] = true
	}
	items := make([]map[string]any, 0, len(body.GrantIDs))
	for i, id := range body.GrantIDs {
		digest := sha256.Sum256([]byte(fmt.Sprintf("account-closed-reclaim-batch-v1\n%s\n%d\n%s", key, i, id)))
		payload, err := client.ReclaimAccountClosedGrant(r.Context(), org, id, resolved.UserID, strings.TrimSpace(body.Reason), hex.EncodeToString(digest[:]))
		if err == nil {
			err = validateClosedGrantReceipt(payload, org, id)
		}
		item := map[string]any{"grant_id": id, "status": "SUCCEEDED"}
		if err != nil {
			item["status"] = "FAILED"
			item["code"] = "COMMAND_RESULT_UNKNOWN"
			var failure *BillingHTTPError
			if errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500 {
				item["code"] = "ACCOUNT_CLOSED_GRANT_RECLAIM_REJECTED"
			}
		} else {
			item["result"] = payload
		}
		items = append(items, item)
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusMultiStatus, map[string]any{"items": items})
}
