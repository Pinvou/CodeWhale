package billing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var latePaymentQueryKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,119}$`)

// Only a fresh, explicit review uses this protocol. A retained legacy UNKNOWN
// must never be upgraded into another Provider call.
type LatePaymentQueryInput struct {
	CommandProtocol int       `json:"command_protocol"`
	PaymentOrderID  uuid.UUID `json:"payment_order_id"`
	ProviderID      string    `json:"provider_id"`
	ProviderVersion string    `json:"provider_version"`
	MerchantOrderNo string    `json:"merchant_order_no"`
}

type LatePaymentQueryReceipt struct {
	CommandProtocol int                    `json:"command_protocol"`
	Reference       string                 `json:"query_reference"`
	CaseID          uuid.UUID              `json:"case_id"`
	PaymentOrderID  uuid.UUID              `json:"payment_order_id"`
	ActorID         uuid.UUID              `json:"actor_id"`
	State           string                 `json:"state"`
	Result          *LatePaymentReviewCase `json:"result,omitempty"`
	dispatch        bool
}

type latePaymentQueryRepository interface {
	PrepareLatePaymentQuery(context.Context, uuid.UUID, uuid.UUID, string, LatePaymentQueryInput) (LatePaymentQueryReceipt, error)
	ReadLatePaymentQuery(context.Context, uuid.UUID, uuid.UUID, string, LatePaymentQueryInput) (LatePaymentQueryReceipt, error)
	CompleteLatePaymentQuery(context.Context, uuid.UUID, uuid.UUID, string, LatePaymentQueryInput, PaymentOrder, error) (LatePaymentQueryReceipt, error)
}

func (r *PostgresRepository) ReadLatePaymentQuery(ctx context.Context, caseID, actorID uuid.UUID, key string, input LatePaymentQueryInput) (LatePaymentQueryReceipt, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return LatePaymentQueryReceipt{}, err
	}
	defer tx.Rollback(ctx)
	_, receipt, _, err := r.lockLatePaymentQuery(ctx, tx, caseID, actorID, key, input)
	if err != nil {
		return receipt, err
	}
	// A missing marker is also UNKNOWN; result recovery never admits a command.
	return receipt, tx.Commit(ctx)
}

func queryInputMatches(input LatePaymentQueryInput, item LatePaymentReviewCase) bool {
	return input.CommandProtocol == 1 && input.PaymentOrderID != uuid.Nil && input.PaymentOrderID == item.PaymentOrderID &&
		input.ProviderID == item.ProviderID && input.ProviderVersion == item.ProviderVersion && input.MerchantOrderNo == item.MerchantOrderNo
}

func latePaymentQueryFingerprint(input LatePaymentQueryInput) (string, []byte) {
	raw, _ := json.Marshal(input)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), raw
}

func decodeLatePaymentQueryInput(body io.Reader) (LatePaymentQueryInput, error) {
	var input LatePaymentQueryInput
	raw, err := io.ReadAll(io.LimitReader(body, 8193))
	if err != nil || len(raw) > 8192 {
		return input, ErrConflict
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return input, ErrConflict
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return input, ErrConflict
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return input, ErrConflict
		}
		switch key {
		case "command_protocol", "payment_order_id", "provider_id", "provider_version", "merchant_order_no":
		default:
			return input, ErrConflict
		}
		seen[key] = true
		var field json.RawMessage
		if decoder.Decode(&field) != nil {
			return input, ErrConflict
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(seen) != 5 || decoder.Decode(new(any)) != io.EOF || json.Unmarshal(raw, &input) != nil ||
		input.CommandProtocol != 1 || input.PaymentOrderID == uuid.Nil || input.ProviderID == "" || input.ProviderVersion == "" || input.MerchantOrderNo == "" {
		return input, ErrConflict
	}
	return input, nil
}

func (r *PostgresRepository) lockLatePaymentQuery(ctx context.Context, tx pgx.Tx, caseID, actorID uuid.UUID, key string, input LatePaymentQueryInput) (LatePaymentReviewCase, LatePaymentQueryReceipt, bool, error) {
	if caseID == uuid.Nil || actorID == uuid.Nil || !latePaymentQueryKey.MatchString(key) || input.CommandProtocol != 1 {
		return LatePaymentReviewCase{}, LatePaymentQueryReceipt{}, false, ErrConflict
	}
	if err := lockTransactionKey(ctx, tx, "late-payment.query-command", key); err != nil {
		return LatePaymentReviewCase{}, LatePaymentQueryReceipt{}, false, err
	}
	item, found, err := scanLatePaymentReviewCase(tx.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1 FOR UPDATE OF late,manual`, caseID))
	if err != nil || !found {
		return item, LatePaymentQueryReceipt{}, false, ErrNotFound
	}
	if item.OwnerID == nil || *item.OwnerID != actorID || !queryInputMatches(input, item) {
		return item, LatePaymentQueryReceipt{}, false, ErrConflict
	}
	receipt := LatePaymentQueryReceipt{CommandProtocol: 1, Reference: key, CaseID: caseID, PaymentOrderID: input.PaymentOrderID, ActorID: actorID, State: "UNKNOWN"}
	var originalActor, originalCase uuid.UUID
	var fingerprint string
	var response []byte
	err = tx.QueryRow(ctx, `SELECT actor_id,case_id,request_hash,result_snapshot FROM late_payment_query_commands WHERE idempotency_key=$1`, key).Scan(&originalActor, &originalCase, &fingerprint, &response)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, receipt, false, nil
	}
	if err != nil {
		return item, receipt, false, err
	}
	expected, _ := latePaymentQueryFingerprint(input)
	if originalActor != actorID || originalCase != caseID || fingerprint != expected {
		return item, receipt, false, ErrConflict
	}
	if len(response) != 0 {
		var result LatePaymentReviewCase
		if json.Unmarshal(response, &result) != nil || result.ID != caseID || result.PaymentOrderID != input.PaymentOrderID {
			return item, receipt, false, ErrConflict
		}
		receipt.State, receipt.Result = "COMPLETED", &result
	}
	return item, receipt, true, nil
}

func (r *PostgresRepository) PrepareLatePaymentQuery(ctx context.Context, caseID, actorID uuid.UUID, key string, input LatePaymentQueryInput) (LatePaymentQueryReceipt, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return LatePaymentQueryReceipt{}, err
	}
	defer tx.Rollback(ctx)
	item, receipt, exists, err := r.lockLatePaymentQuery(ctx, tx, caseID, actorID, key, input)
	if err != nil {
		return receipt, err
	}
	if exists {
		return receipt, tx.Commit(ctx)
	}
	if item.ReviewStatus != "REVIEW_REQUIRED_LATE_PAYMENT" && item.ReviewStatus != "PROVIDER_BLIND" {
		return receipt, ErrConflict
	}
	fingerprint, snapshot := latePaymentQueryFingerprint(input)
	_, err = tx.Exec(ctx, `INSERT INTO late_payment_query_commands(idempotency_key,case_id,actor_id,payment_order_id,request_hash,request_snapshot,started_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, key, caseID, actorID, input.PaymentOrderID, fingerprint, snapshot, r.now())
	if err != nil {
		return receipt, err
	}
	if err = tx.Commit(ctx); err != nil {
		// An uncertain commit grants no execution token. The same key can only
		// discover its retained marker on replay, never renew execution.
		return receipt, err
	}
	receipt.dispatch = true
	return receipt, nil
}

func (r *PostgresRepository) CompleteLatePaymentQuery(ctx context.Context, caseID, actorID uuid.UUID, key string, input LatePaymentQueryInput, order PaymentOrder, queryErr error) (LatePaymentQueryReceipt, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return LatePaymentQueryReceipt{}, err
	}
	defer tx.Rollback(ctx)
	item, receipt, exists, err := r.lockLatePaymentQuery(ctx, tx, caseID, actorID, key, input)
	if err != nil {
		return receipt, err
	}
	if !exists {
		return receipt, ErrConflict
	}
	if receipt.State == "COMPLETED" {
		return receipt, tx.Commit(ctx)
	}
	if order.ID != uuid.Nil && order.ID != input.PaymentOrderID || queryErr == nil && order.ID != input.PaymentOrderID ||
		(item.ReviewStatus != "REVIEW_REQUIRED_LATE_PAYMENT" && item.ReviewStatus != "PROVIDER_BLIND") {
		return receipt, ErrConflict
	}
	result, err := r.recordLatePaymentReviewQueryLocked(ctx, tx, item, actorID, order, queryErr)
	if err != nil {
		return receipt, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return receipt, err
	}
	command, err := tx.Exec(ctx, `UPDATE late_payment_query_commands SET result_snapshot=$2,completed_at=$3 WHERE idempotency_key=$1 AND completed_at IS NULL`, key, raw, r.now())
	if err != nil {
		return receipt, err
	}
	if command.RowsAffected() != 1 {
		return receipt, ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return receipt, err
	}
	receipt.State, receipt.Result = "COMPLETED", &result
	return receipt, nil
}

func (s *Service) adminLatePaymentQueryCommand(w http.ResponseWriter, r *http.Request) {
	s.serveLatePaymentQueryCommand(w, r, true)
}

func (s *Service) adminLatePaymentQueryResult(w http.ResponseWriter, r *http.Request) {
	s.serveLatePaymentQueryCommand(w, r, false)
}

func (s *Service) serveLatePaymentQueryCommand(w http.ResponseWriter, r *http.Request, dispatch bool) {
	repository, available := s.repository.(latePaymentQueryRepository)
	caseID, valid := parseLatePaymentCaseID(w, r)
	if !valid {
		return
	}
	if !available || dispatch && s.payments == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "payment operations are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	key := r.Header.Get("Idempotency-Key")
	input, decodeErr := decodeLatePaymentQueryInput(r.Body)
	if decodeErr != nil || !latePaymentQueryKey.MatchString(key) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_QUERY_COMMAND", "invalid payment review query command", httpserver.RequestID(r.Context()))
		return
	}
	actorID := adminActor(adminPrincipal(r))
	var receipt LatePaymentQueryReceipt
	var err error
	if dispatch {
		receipt, err = repository.PrepareLatePaymentQuery(r.Context(), caseID, actorID, key, input)
	} else {
		receipt, err = repository.ReadLatePaymentQuery(r.Context(), caseID, actorID, key, input)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if receipt.dispatch {
		// No transaction or database lock spans the existing Provider flow.
		// A canceled request cannot be detached to run another external call.
		if r.Context().Err() == nil {
			order, queryErr := s.payments.SyncReviewedOrder(r.Context(), input.PaymentOrderID)
			if completed, err := repository.CompleteLatePaymentQuery(r.Context(), caseID, actorID, key, input, order, queryErr); err == nil {
				receipt = completed
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeAdminFinancialJSON(w, http.StatusOK, receipt)
}
