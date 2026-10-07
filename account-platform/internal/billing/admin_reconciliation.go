package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strings"
	"time"
)

type AdminReconciliationCase struct {
	ID               uuid.UUID       `json:"case_id"`
	RunID            uuid.UUID       `json:"reconciliation_run_id"`
	PaymentOrderID   *uuid.UUID      `json:"payment_order_id,omitempty"`
	MerchantOrderNo  string          `json:"merchant_order_no"`
	CaseType         string          `json:"case_type"`
	Status           string          `json:"status"`
	Severity         string          `json:"severity"`
	OwnerID          *uuid.UUID      `json:"owner_id,omitempty"`
	RepairAction     *string         `json:"repair_action,omitempty"`
	ReviewResult     *string         `json:"review_result,omitempty"`
	ProviderSnapshot json.RawMessage `json:"provider_snapshot,omitempty"`
	InternalSnapshot json.RawMessage `json:"internal_snapshot,omitempty"`
	RefundSnapshot   json.RawMessage `json:"refund_snapshot,omitempty"`
	LedgerSnapshot   json.RawMessage `json:"ledger_snapshot,omitempty"`
	Resolution       *string         `json:"resolution,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	ResolvedAt       *time.Time      `json:"resolved_at,omitempty"`
	UpdatedAt        time.Time       `json:"updated_at"`
	Version          int64           `json:"version"`
}

type ReconciliationCaseCommand struct {
	Action         string
	OwnerID        *uuid.UUID
	RepairAction   string
	ReviewResult   string
	Reason         string
	IdempotencyKey string
	ActorID        uuid.UUID
}

func (s *Service) adminImportReconciliation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel      string              `json:"channel"`
		BusinessDate string              `json:"business_date"`
		Items        []ProviderBillEntry `json:"items"`
	}
	if s.payments == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "payment operations are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	businessDate, err := time.Parse("2006-01-02", body.BusinessDate)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "business_date must use YYYY-MM-DD", httpserver.RequestID(r.Context()))
		return
	}
	run, err := s.payments.ImportProviderBill(r.Context(), adminActor(adminPrincipal(r)), body.Channel, businessDate, body.Items)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, run)
}

func (s *Service) adminReconciliationRuns(w http.ResponseWriter, r *http.Request) {
	s.writeAdminSnapshotCollection(w, r, adminPageReconciliationRuns, "", func(repository adminPaginationItemsRepository, rawIDs []string) (any, error) {
		ids, err := adminUUIDs(rawIDs)
		if err != nil {
			return nil, err
		}
		paged, ok := repository.(adminReconciliationPageRepository)
		if !ok {
			return nil, errors.New("reconciliation repository unavailable")
		}
		return paged.AdminReconciliationRunsByIDs(r.Context(), ids)
	})
}

func (s *Service) adminRunReconciliation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel      string `json:"channel"`
		BusinessDate string `json:"business_date"`
	}
	if s.payments == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "payment operations are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	date, err := time.Parse("2006-01-02", body.BusinessDate)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "valid channel and business date are required", httpserver.RequestID(r.Context()))
		return
	}
	run, err := s.payments.RunAdminReconciliation(r.Context(), adminActor(adminPrincipal(r)), body.Channel, date)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, run)
}

func (s *Service) adminReconciliationCases(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		runID = strings.TrimSpace(r.URL.Query().Get("run_id"))
	}
	if runID != "" {
		parsed, err := uuid.Parse(runID)
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid reconciliation run", httpserver.RequestID(r.Context()))
			return
		}
		runID = parsed.String()
	}
	s.writeAdminSnapshotCollection(w, r, adminPageReconciliationCases, runID, func(repository adminPaginationItemsRepository, rawIDs []string) (any, error) {
		ids, err := adminUUIDs(rawIDs)
		if err != nil {
			return nil, err
		}
		paged, ok := repository.(adminReconciliationPageRepository)
		if !ok {
			return nil, errors.New("reconciliation repository unavailable")
		}
		return paged.AdminReconciliationCasesByIDs(r.Context(), ids)
	})
}

func (s *Service) adminUpdateReconciliationCase(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(adminReconciliationCommandRepository)
	caseID, err := uuid.Parse(r.PathValue("case_id"))
	var body struct {
		Action       string     `json:"action"`
		OwnerID      *uuid.UUID `json:"owner_id"`
		RepairAction string     `json:"repair_action"`
		ReviewResult string     `json:"review_result"`
		Reason       string     `json:"reason"`
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "reconciliation operations are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil || caseID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "valid reconciliation case is required", httpserver.RequestID(r.Context()))
		return
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 120 || len([]rune(strings.TrimSpace(body.Reason))) < 10 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "valid reconciliation command, reason, and idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.UpdateReconciliationCase(r.Context(), caseID, ReconciliationCaseCommand{Action: body.Action, OwnerID: body.OwnerID, RepairAction: body.RepairAction, ReviewResult: body.ReviewResult, Reason: body.Reason, IdempotencyKey: idempotencyKey, ActorID: adminActor(adminPrincipal(r))})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, item)
}

func (r *PostgresRepository) AdminReconciliationRuns(ctx context.Context, limit int) ([]ReconciliationRun, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,provider_id,provider_version,channel,business_date,source_mode,requested_by,input_digest,status,provider_row_count,case_count,started_at,COALESCE(completed_at,started_at) FROM reconciliation_runs ORDER BY started_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ReconciliationRun, 0)
	for rows.Next() {
		var item ReconciliationRun
		var businessDate time.Time
		if err := rows.Scan(&item.ID, &item.ProviderID, &item.ProviderVersion, &item.Channel, &businessDate, &item.SourceMode, &item.RequestedBy, &item.InputDigest, &item.Status, &item.ProviderRowCount, &item.CaseCount, &item.StartedAt, &item.CompletedAt); err != nil {
			return nil, err
		}
		item.BusinessDate = businessDate.Format("2006-01-02")
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminReconciliationCases(ctx context.Context, runID uuid.UUID, limit int) ([]AdminReconciliationCase, error) {
	rows, err := r.pool.Query(ctx, reconciliationCaseSelect+` WHERE reconciliation_run_id=$1 ORDER BY created_at,id LIMIT $2`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminReconciliationCase, 0)
	for rows.Next() {
		var item AdminReconciliationCase
		if err := scanReconciliationCase(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanReconciliationCase(row billingRowScanner, item *AdminReconciliationCase) error {
	return row.Scan(&item.ID, &item.RunID, &item.PaymentOrderID, &item.MerchantOrderNo, &item.CaseType, &item.Status, &item.Severity, &item.OwnerID, &item.RepairAction, &item.ReviewResult, &item.ProviderSnapshot, &item.InternalSnapshot, &item.RefundSnapshot, &item.LedgerSnapshot, &item.Resolution, &item.CreatedAt, &item.ResolvedAt, &item.UpdatedAt, &item.Version)
}

func (r *PostgresRepository) UpdateReconciliationCase(ctx context.Context, caseID uuid.UUID, command ReconciliationCaseCommand) (AdminReconciliationCase, error) {
	command.Action = strings.ToUpper(strings.TrimSpace(command.Action))
	command.Reason = strings.TrimSpace(command.Reason)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.RepairAction = strings.TrimSpace(command.RepairAction)
	command.ReviewResult = strings.TrimSpace(command.ReviewResult)
	if caseID == uuid.Nil || command.ActorID == uuid.Nil || (command.OwnerID != nil && *command.OwnerID == uuid.Nil) || len([]rune(command.Reason)) < 10 || len(command.IdempotencyKey) < 8 || len(command.IdempotencyKey) > 120 {
		return AdminReconciliationCase{}, ErrConflict
	}
	requestPayload, _ := json.Marshal(map[string]any{"action": command.Action, "owner_id": command.OwnerID, "repair_action": command.RepairAction, "review_result": command.ReviewResult, "reason": command.Reason})
	digest := sha256.Sum256(requestPayload)
	requestHash := fmt.Sprintf("%x", digest[:])
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AdminReconciliationCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "reconciliation-case.command", caseID.String()+":"+command.IdempotencyKey); err != nil {
		return AdminReconciliationCase{}, err
	}
	var existingHash string
	var existingActor uuid.UUID
	var existingReceipt []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,actor_id,after_snapshot FROM reconciliation_case_commands WHERE reconciliation_case_id=$1 AND idempotency_key=$2`, caseID, command.IdempotencyKey).Scan(&existingHash, &existingActor, &existingReceipt)
	if err == nil {
		if existingHash != requestHash || existingActor != command.ActorID {
			return AdminReconciliationCase{}, ErrConflict
		}
		var item AdminReconciliationCase
		if err := json.Unmarshal(existingReceipt, &item); err != nil {
			return AdminReconciliationCase{}, err
		}
		if item.ID != caseID {
			return AdminReconciliationCase{}, ErrConflict
		}
		return item, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AdminReconciliationCase{}, err
	}
	var item AdminReconciliationCase
	if err := scanReconciliationCase(tx.QueryRow(ctx, reconciliationCaseSelect+` WHERE id=$1 FOR UPDATE`, caseID), &item); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return AdminReconciliationCase{}, err
	}
	before, _ := json.Marshal(item)
	now := r.now().UTC().Truncate(time.Microsecond)
	switch command.Action {
	case "CLAIM":
		if item.Status != "OPEN" && item.Status != "IN_REVIEW" {
			return AdminReconciliationCase{}, ErrConflict
		}
		if item.OwnerID != nil && *item.OwnerID != command.ActorID {
			return AdminReconciliationCase{}, ErrConflict
		}
		item.OwnerID = &command.ActorID
		item.Status = "IN_REVIEW"
	case "ASSIGN":
		if command.OwnerID == nil || item.Status == "RESOLVED" || item.Status == "IGNORED" {
			return AdminReconciliationCase{}, ErrConflict
		}
		item.OwnerID = command.OwnerID
		item.Status = "IN_REVIEW"
	case "SET_REPAIR_ACTION":
		if command.RepairAction == "" || item.OwnerID == nil || *item.OwnerID != command.ActorID || item.Status == "RESOLVED" || item.Status == "IGNORED" {
			return AdminReconciliationCase{}, ErrConflict
		}
		item.RepairAction = &command.RepairAction
		item.Status = "IN_REVIEW"
	case "RESOLVE", "IGNORE":
		if command.ReviewResult == "" || item.OwnerID == nil || *item.OwnerID != command.ActorID || (item.Status != "OPEN" && item.Status != "IN_REVIEW") {
			return AdminReconciliationCase{}, ErrConflict
		}
		item.ReviewResult, item.Resolution = &command.ReviewResult, &command.Reason
		item.Status = map[bool]string{true: "RESOLVED", false: "IGNORED"}[command.Action == "RESOLVE"]
		item.ResolvedAt = &now
	case "REOPEN":
		if item.Status != "RESOLVED" && item.Status != "IGNORED" {
			return AdminReconciliationCase{}, ErrConflict
		}
		item.Status, item.ResolvedAt = "OPEN", nil
		item.ReviewResult, item.Resolution = nil, nil
	default:
		return AdminReconciliationCase{}, ErrConflict
	}
	item.UpdatedAt = now
	item.Version++
	_, err = tx.Exec(ctx, `UPDATE reconciliation_cases SET status=$2,severity=$3,owner_id=$4,repair_action=$5,review_result=$6,resolution=$7,resolved_at=$8,updated_at=$9,version=version+1 WHERE id=$1`, item.ID, item.Status, item.Severity, item.OwnerID, item.RepairAction, item.ReviewResult, item.Resolution, item.ResolvedAt, now)
	if err == nil {
		manualStatus := "OPEN"
		if item.Status == "RESOLVED" || item.Status == "IGNORED" {
			manualStatus = "RESOLVED"
		}
		_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET status=$2,owner_id=$3,claimed_at=CASE WHEN $3::uuid IS NULL THEN claimed_at ELSE COALESCE(claimed_at,$4) END,first_response_at=CASE WHEN $3::uuid IS NULL THEN first_response_at ELSE COALESCE(first_response_at,$4) END,mitigation_at=CASE WHEN $5<>'' THEN COALESCE(mitigation_at,$4) ELSE mitigation_at END,resolved_at=$6,updated_at=$4 WHERE id=$1`, item.ID, manualStatus, item.OwnerID, now, command.RepairAction, item.ResolvedAt)
	}
	after, _ := json.Marshal(item)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO reconciliation_case_commands(id,reconciliation_case_id,actor_id,idempotency_key,request_hash,action,reason,before_snapshot,after_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, uuid.Must(uuid.NewV7()), item.ID, command.ActorID, command.IdempotencyKey, requestHash, command.Action, command.Reason, before, after, now)
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, item.ID, command.Action, &command.ActorID, map[string]any{"reason": command.Reason, "repair_action": command.RepairAction, "review_result": command.ReviewResult}, now)
	}
	if err != nil {
		return AdminReconciliationCase{}, err
	}
	return item, tx.Commit(ctx)
}

type adminReconciliationCommandRepository interface {
	UpdateReconciliationCase(context.Context, uuid.UUID, ReconciliationCaseCommand) (AdminReconciliationCase, error)
}

type adminReconciliationPageRepository interface {
	AdminReconciliationRunsByIDs(context.Context, []uuid.UUID) ([]ReconciliationRun, error)
	AdminReconciliationCasesByIDs(context.Context, []uuid.UUID) ([]AdminReconciliationCase, error)
}

func (r *PostgresRepository) AdminReconciliationRunsByIDs(ctx context.Context, ids []uuid.UUID) ([]ReconciliationRun, error) {
	items := []ReconciliationRun{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT id,provider_id,provider_version,channel,business_date,source_mode,requested_by,input_digest,status,provider_row_count,case_count,started_at,COALESCE(completed_at,started_at) FROM reconciliation_runs WHERE id=ANY($1) ORDER BY array_position($1::uuid[],id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ReconciliationRun
		var businessDate time.Time
		if err := rows.Scan(&item.ID, &item.ProviderID, &item.ProviderVersion, &item.Channel, &businessDate, &item.SourceMode, &item.RequestedBy, &item.InputDigest, &item.Status, &item.ProviderRowCount, &item.CaseCount, &item.StartedAt, &item.CompletedAt); err != nil {
			return nil, err
		}
		item.BusinessDate = businessDate.Format("2006-01-02")
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminReconciliationCasesByIDs(ctx context.Context, ids []uuid.UUID) ([]AdminReconciliationCase, error) {
	items := []AdminReconciliationCase{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, reconciliationCaseSelect+` WHERE id=ANY($1) ORDER BY array_position($1::uuid[],id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item AdminReconciliationCase
		if err := scanReconciliationCase(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
