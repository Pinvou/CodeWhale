package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ChargebackPrincipalEvidencePage struct {
	Principal    ChargebackCashPrincipal `json:"cash_principal"`
	Observations []json.RawMessage       `json:"observations"`
	NextAfter    *uuid.UUID              `json:"next_after"`
}

func (r *PostgresRepository) ChargebackPrincipalEvidence(ctx context.Context, caseID uuid.UUID, after uuid.UUID, limit int) (ChargebackPrincipalEvidencePage, error) {
	result := ChargebackPrincipalEvidencePage{Observations: []json.RawMessage{}}
	if caseID == uuid.Nil || limit < 1 || limit > 100 {
		return result, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	result.Principal, err = readChargebackCashPrincipal(ctx, tx, caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT id,jsonb_build_object('id',id,'payment_transaction_id',payment_transaction_id,'provider_id',provider_id,'provider_version',provider_version,'provider_chargeback_id',provider_chargeback_id,'provider_transaction_id',provider_transaction_id,'amount_fen',amount_fen::text,'currency',currency,'supplier_evidence_reference',supplier_evidence_reference,'payload_digest',payload_digest,'observation_digest',observation_digest,'recorded_by_service',recorded_by_service,'recorded_by_subject',recorded_by_subject,'received_at',received_at)
 FROM chargeback_principal_evidence WHERE chargeback_case_id=$1 AND id>$2 ORDER BY id LIMIT $3`, caseID, after, limit+1)
	if err != nil {
		return result, err
	}
	var last uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var raw json.RawMessage
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return result, err
		}
		if len(result.Observations) == limit {
			result.NextAfter = &last
			break
		}
		result.Observations = append(result.Observations, raw)
		last = id
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

type chargebackPrincipalEvidenceRepository interface {
	ChargebackPrincipalEvidence(context.Context, uuid.UUID, uuid.UUID, int) (ChargebackPrincipalEvidencePage, error)
}

func (s *Service) chargebackPrincipalEvidence(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(chargebackPrincipalEvidenceRepository)
	if !ok {
		s.writeError(w, r, payment.ErrUnsupported)
		return
	}
	caseID, err := uuid.Parse(r.PathValue("case_id"))
	after := uuid.Nil
	limit := 20
	query := r.URL.Query()
	valid := err == nil
	for key, values := range query {
		if (key != "after" && key != "limit") || len(values) != 1 {
			valid = false
		}
	}
	if query.Has("after") {
		after, err = uuid.Parse(query.Get("after"))
		valid = valid && err == nil
	}
	if query.Has("limit") {
		limit, err = strconv.Atoi(query.Get("limit"))
		valid = valid && err == nil
	}
	if !valid || caseID == uuid.Nil || limit < 1 || limit > 100 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chargeback principal query", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.ChargebackPrincipalEvidence(r.Context(), caseID, after, limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, result)
}
