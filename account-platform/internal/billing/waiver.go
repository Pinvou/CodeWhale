package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WaiverAuthority is the hold-row winner, including the original settlement
// evidence when a concurrent commit won. It is an internal service contract.
type WaiverAuthority struct {
	Hold          AdminHold       `json:"hold"`
	UsageSnapshot json.RawMessage `json:"usage_snapshot"`
}

type WaiverCoordinator interface {
	Release(context.Context, uuid.UUID, uuid.UUID, string, string) (AdminHold, error)
}

type HTTPWaiverCoordinator struct {
	baseURL *url.URL
	token   string
	client  *http.Client
}

func NewHTTPWaiverCoordinator(rawURL, serviceToken string) (*HTTPWaiverCoordinator, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("invalid Token waiver coordinator configuration")
	}
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPWaiverCoordinator{baseURL: parsed, token: serviceToken, client: client}, nil
}

func (c *HTTPWaiverCoordinator) Release(ctx context.Context, holdID, usageID uuid.UUID, reason, actor string) (AdminHold, error) {
	body, _ := json.Marshal(map[string]any{"hold_id": holdID, "usage_request_id": usageID, "reason": reason, "actor": actor})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL.String()+"/internal/v1/usage-waivers", bytes.NewReader(body))
	if err != nil {
		return AdminHold{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return AdminHold{}, errors.New("Token waiver coordinator unavailable")
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(payload) > 1<<20 {
		return AdminHold{}, errors.New("invalid Token waiver response")
	}
	if response.StatusCode == http.StatusConflict {
		return AdminHold{}, ErrUsageAlreadyTerminal
	}
	if response.StatusCode != http.StatusOK {
		return AdminHold{}, errors.New("Token waiver coordinator unavailable")
	}
	var result WaiverAuthority
	var proof struct {
		Waiver bool   `json:"admin_waiver"`
		Actor  string `json:"admin_actor"`
		Reason string `json:"admin_release_reason"`
	}
	if json.Unmarshal(payload, &result) != nil || result.Hold.ID != holdID || result.Hold.UsageRequestID != usageID || result.Hold.WalletID == uuid.Nil || result.Hold.MaxMicrocredits <= 0 || result.Hold.ExpiresAt.IsZero() || result.Hold.UpdatedAt.IsZero() || result.Hold.Status != "RELEASED" || result.Hold.ActualMicrocredits == nil || *result.Hold.ActualMicrocredits != 0 || json.Unmarshal(result.UsageSnapshot, &proof) != nil || !proof.Waiver || strings.TrimSpace(proof.Actor) == "" || strings.TrimSpace(proof.Reason) == "" {
		return AdminHold{}, errors.New("invalid Token waiver authority")
	}
	return result.Hold, nil
}

func (s *Service) WithWaiverCoordinator(coordinator WaiverCoordinator) *Service {
	s.waivers = coordinator
	return s
}

func (s *Service) resolveUsageWaiver(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HoldID  uuid.UUID `json:"hold_id"`
		UsageID uuid.UUID `json:"usage_request_id"`
		Reason  string    `json:"reason"`
		Actor   string    `json:"actor"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.HoldID == uuid.Nil || body.UsageID == uuid.Nil || strings.TrimSpace(body.Reason) == "" || strings.TrimSpace(body.Actor) == "" {
		api.WriteError(w, 400, "INVALID_REQUEST", "invalid usage waiver", httpserver.RequestID(r.Context()))
		return
	}
	repo, ok := s.repository.(interface {
		ResolveUsageWaiver(context.Context, uuid.UUID, uuid.UUID, string, string) (WaiverAuthority, error)
	})
	if !ok {
		api.WriteError(w, 503, "INTERNAL_ERROR", "usage waiver unavailable", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repo.ResolveUsageWaiver(r.Context(), body.HoldID, body.UsageID, body.Reason, body.Actor)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, 200, result)
}

func (r *PostgresRepository) ResolveUsageWaiver(ctx context.Context, holdID, usageID uuid.UUID, reason, actor string) (WaiverAuthority, error) {
	// The same row lock used by Commit determines the only financial winner.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return WaiverAuthority{}, err
	}
	defer tx.Rollback(ctx)
	var result WaiverAuthority
	h := &result.Hold
	err = tx.QueryRow(ctx, `SELECT id,usage_request_id,wallet_id,status,max_microcredits,actual_microcredits,expires_at,updated_at,COALESCE(usage_snapshot,'{}'::jsonb) FROM usage_holds WHERE id=$1`, holdID).Scan(&h.ID, &h.UsageRequestID, &h.WalletID, &h.Status, &h.MaxMicrocredits, &h.ActualMicrocredits, &h.ExpiresAt, &h.UpdatedAt, &result.UsageSnapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if _, err := getWallet(ctx, tx, h.WalletID, true); err != nil {
		return result, err
	}
	err = tx.QueryRow(ctx, `SELECT id,usage_request_id,wallet_id,status,max_microcredits,actual_microcredits,expires_at,updated_at,COALESCE(usage_snapshot,'{}'::jsonb) FROM usage_holds WHERE id=$1 FOR UPDATE`, holdID).Scan(&h.ID, &h.UsageRequestID, &h.WalletID, &h.Status, &h.MaxMicrocredits, &h.ActualMicrocredits, &h.ExpiresAt, &h.UpdatedAt, &result.UsageSnapshot)
	if err != nil {
		return result, err
	}
	if h.UsageRequestID != usageID {
		return result, ErrConflict
	}
	var old map[string]json.RawMessage
	if err = json.Unmarshal(result.UsageSnapshot, &old); err != nil {
		return result, err
	}
	if h.Status == "COMMITTED" {
		return result, tx.Commit(ctx)
	}
	if h.Status == "RELEASED" {
		var waiver bool
		_ = json.Unmarshal(old["admin_waiver"], &waiver)
		if waiver {
			return result, tx.Commit(ctx)
		}
		// A definite normal release can race an operator's prepared decision.
		// The explicit decision may annotate that zero-charge terminal; normal
		// release or timeout alone never creates an administrator waiver.
	}
	if h.Status != "ACTIVE" && h.Status != "REVIEW" && h.Status != "RELEASED" {
		return result, ErrUsageAlreadyTerminal
	}
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(actor) == "" {
		return result, ErrConflict
	}
	now := r.now()
	if h.Status != "RELEASED" {
		if _, err := getWallet(ctx, tx, h.WalletID, true); err != nil {
			return result, err
		}
		if err := releasePackageAllocations(ctx, tx, h.ID, now); err != nil {
			return result, err
		}
		command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits-$2,version=version+1,updated_at=$3 WHERE id=$1 AND reserved_microcredits >= $2`, h.WalletID, h.MaxMicrocredits, now)
		if err != nil {
			return result, err
		}
		if command.RowsAffected() != 1 {
			return result, ErrConflict
		}
	}
	metadata, _ := json.Marshal(map[string]any{"admin_waiver": true, "admin_release_reason": reason, "admin_actor": actor})
	err = tx.QueryRow(ctx, `UPDATE usage_holds SET status='RELEASED',actual_microcredits=0,usage_snapshot=COALESCE(usage_snapshot,'{}'::jsonb)||$2::jsonb,terminal_at=COALESCE(terminal_at,$3),updated_at=$3 WHERE id=$1 RETURNING usage_snapshot`, holdID, metadata, now).Scan(&result.UsageSnapshot)
	if err != nil {
		return result, err
	}
	zero := int64(0)
	h.Status, h.ActualMicrocredits, h.UpdatedAt = "RELEASED", &zero, now
	return result, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminUsageHold(ctx context.Context, holdID uuid.UUID) (AdminHold, error) {
	var h AdminHold
	err := r.pool.QueryRow(ctx, `SELECT id,usage_request_id,wallet_id,status,max_microcredits,actual_microcredits,expires_at,updated_at FROM usage_holds WHERE id=$1`, holdID).Scan(&h.ID, &h.UsageRequestID, &h.WalletID, &h.Status, &h.MaxMicrocredits, &h.ActualMicrocredits, &h.ExpiresAt, &h.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}
