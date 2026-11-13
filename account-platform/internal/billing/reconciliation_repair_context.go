package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReconciliationRepairContext is a read model, never an eligibility proof.
// The date belongs to the case's original run, not a later observation.
// Only the existing owner precheck can confirm evidence and closure eligibility.
type ReconciliationRepairContext struct {
	CaseID                uuid.UUID `json:"case_id"`
	RunID                 uuid.UUID `json:"reconciliation_run_id"`
	CaseType              string    `json:"case_type"`
	Status                string    `json:"status"`
	CaseVersion           int64     `json:"case_version,string"`
	Channel               string    `json:"channel"`
	BusinessDate          string    `json:"business_date"`
	ProviderID            string    `json:"provider_id"`
	ProviderVersion       string    `json:"provider_version"`
	MerchantOrderNo       string    `json:"merchant_order_no"`
	AmountFen             *int64    `json:"amount_fen,string"`
	Currency              string    `json:"currency"`
	ProviderTransactionID string    `json:"provider_transaction_id"`
	SnapshotAvailable     bool      `json:"snapshot_available"`
}

type reconciliationRepairContextRepository interface {
	AdminReconciliationRepairContext(context.Context, uuid.UUID) (ReconciliationRepairContext, error)
}

func (s *Service) adminReconciliationRepairContext(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("case_id"))
	if err != nil || id == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a valid reconciliation case is required", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(reconciliationRepairContextRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "RECONCILIATION_CONTEXT_UNAVAILABLE", "reconciliation context is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	item, err := repository.AdminReconciliationRepairContext(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (r *PostgresRepository) AdminReconciliationRepairContext(ctx context.Context, id uuid.UUID) (ReconciliationRepairContext, error) {
	var item ReconciliationRepairContext
	var date time.Time
	var snapshotRaw []byte
	err := r.pool.QueryRow(ctx, `SELECT c.id,c.reconciliation_run_id,c.case_type,c.status,c.version,
 r.channel,r.business_date,r.provider_id,r.provider_version,c.merchant_order_no,c.provider_snapshot
 FROM reconciliation_cases c JOIN reconciliation_runs r ON r.id=c.reconciliation_run_id WHERE c.id=$1`, id).
		Scan(&item.CaseID, &item.RunID, &item.CaseType, &item.Status, &item.CaseVersion,
			&item.Channel, &date, &item.ProviderID, &item.ProviderVersion, &item.MerchantOrderNo, &snapshotRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return ReconciliationRepairContext{}, err
	}
	item.BusinessDate = date.Format("2006-01-02")
	item.AmountFen, item.Currency, item.ProviderTransactionID, item.SnapshotAvailable = reconciliationRepairSnapshotContext(snapshotRaw)
	return item, nil
}

func reconciliationRepairSnapshotContext(raw []byte) (*int64, string, string, bool) {
	var snapshot reconciliationProviderSnapshot
	var sourceFields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &snapshot) != nil || json.Unmarshal(raw, &sourceFields) != nil ||
		sourceFields["amount_fen"] == nil || string(sourceFields["amount_fen"]) == "null" {
		return nil, "", "", false
	}
	// Keep exact source facts, including explicit zero/negative values. A missing
	// or corrupt source amount is not silently rendered as zero.
	return &snapshot.AmountFen, snapshot.Currency, snapshot.ChannelTransactionID, true
}
