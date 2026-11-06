package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func directPaymentRepairHash(orderID, actorID uuid.UUID, reason string) string {
	body, _ := json.Marshal(struct {
		Protocol int       `json:"command_protocol"`
		OrderID  uuid.UUID `json:"order_id"`
		ActorID  uuid.UUID `json:"actor_id"`
		Reason   string    `json:"reason"`
	}{1, orderID, actorID, reason})
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// A command identity is independent of the existing per-transaction repair
// audit identity. Historical audits do not prove an unknown request key.
func readDirectPaymentRepair(ctx context.Context, database queryable, orderID, actorID uuid.UUID, key, reason string) (PaymentOrder, bool, error) {
	var savedOrder uuid.UUID
	var protocol int
	var savedReason, savedHash string
	var body []byte
	err := database.QueryRow(ctx, `SELECT payment_order_id,command_protocol,reason,request_hash,response_body FROM payment_order_repair_commands WHERE actor_id=$1 AND idempotency_key=$2`, actorID, key).Scan(&savedOrder, &protocol, &savedReason, &savedHash, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, false, nil
	}
	if err != nil {
		return PaymentOrder{}, false, err
	}
	var order PaymentOrder
	if protocol != 1 || savedOrder != orderID || savedReason != reason || savedHash != directPaymentRepairHash(orderID, actorID, reason) || json.Unmarshal(body, &order) != nil || order.ID != orderID {
		return PaymentOrder{}, false, ErrConflict
	}
	return order, true, nil
}

func directPaymentRepairWriteError(err error) error {
	var conflict *pgconn.PgError
	if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == "payment_order_repair_commands_actor_key" {
		return ErrConflict
	}
	return err
}
