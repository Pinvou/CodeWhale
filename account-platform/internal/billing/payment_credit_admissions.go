package billing

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcredit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (p *PaymentCoordinator) WithPaymentCreditAdmission(authority paymentcredit.Admission) *PaymentCoordinator {
	p.creditAdmission = authority
	return p
}

// preparePaymentCredit runs with the original order row locked. Account owns
// admission; an absent/unknown authority is never an implicit money permit.
func (p *PaymentCoordinator) preparePaymentCredit(ctx context.Context, tx pgx.Tx, order PaymentOrder, transactionID uuid.UUID, provider ProviderOrder) (uuid.UUID, bool, error) {
	if p.creditAdmission == nil || provider.ChannelTransactionID == nil {
		return uuid.Nil, false, nil
	}
	var payer uuid.UUID
	var product json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT payer_user_id,product_snapshot FROM payment_orders WHERE id=$1 FOR UPDATE`, order.ID).Scan(&payer, &product); err != nil {
		return uuid.Nil, false, err
	}
	if payer != order.RequesterID {
		return uuid.Nil, false, ErrConflict
	}
	identity, err := json.Marshal([]string{order.ProviderID, order.ProviderVersion, *provider.ChannelTransactionID})
	if err != nil {
		return uuid.Nil, false, err
	}
	facts, err := json.Marshal(struct {
		OrderID         uuid.UUID
		WalletID        uuid.UUID
		Payer           uuid.UUID
		Beneficiary     *uuid.UUID
		Product         json.RawMessage
		Amount          int64
		Currency        string
		Credits         int64
		ProviderID      string
		ProviderVersion string
		Merchant        string
		SKU             string
		PaidAt          *time.Time
		Customer        string
	}{order.ID, order.WalletID, payer, order.BeneficiaryOrganizationID, product, order.AmountFen, order.Currency, order.CreditMicrocredits, order.ProviderID, order.ProviderVersion, order.MerchantOrderNo, order.ProviderSKU, provider.PaidAt, provider.ExternalCustomerKey})
	if err != nil {
		return uuid.Nil, false, err
	}
	input := paymentcredit.Request{ID: uuid.Must(uuid.NewV7()), UserID: payer, OrderID: order.ID, TransactionDigest: paymentcredit.Digest(identity), FactsDigest: paymentcredit.Digest(facts)}
	decision, err := p.creditAdmission.Admit(ctx, input)
	if err != nil || !decision.Matches(input) || decision.State != "ADMITTED" {
		// Both authoritative closed and unknown admission retain original payment
		// evidence for manual recovery; neither can create financial effects.
		return uuid.Nil, false, nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_credit_attempts(id,payer_user_id,payment_order_id,payment_transaction_id,transaction_digest,facts_digest,fingerprint,state) VALUES($1,$2,$3,$4,$5,$6,$7,'PREPARED')`, input.ID, input.UserID, input.OrderID, transactionID, input.TransactionDigest, input.FactsDigest, input.Fingerprint())
	return input.ID, err == nil, err
}

func markPaymentCreditApplied(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	command, err := tx.Exec(ctx, `UPDATE payment_credit_attempts SET state='APPLIED',resolved_at=clock_timestamp() WHERE id=$1 AND state='PREPARED'`, id)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return paymentcredit.ErrProtocol
	}
	return nil
}

// ResolvePaymentCredit serializes with the original order writer. An absent
// result is ABORTED only after inserting its retained operation tombstone.
// Account must not hold a transaction/user lock while invoking this method.
func (r *PostgresRepository) ResolvePaymentCredit(ctx context.Context, input paymentcredit.Request) (paymentcredit.Decision, error) {
	if !input.Valid() {
		return paymentcredit.Decision{}, ErrConflict
	}
	decision := paymentcredit.Decision{ID: input.ID, Fingerprint: input.Fingerprint()}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return decision, err
	}
	defer tx.Rollback(ctx)
	var payer uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT payer_user_id FROM payment_orders WHERE id=$1 FOR UPDATE`, input.OrderID).Scan(&payer); err != nil {
		return decision, err
	}
	if payer != input.UserID {
		return decision, ErrConflict
	}
	var fingerprint, state string
	var transaction *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT fingerprint,state,payment_transaction_id FROM payment_credit_attempts WHERE id=$1`, input.ID).Scan(&fingerprint, &state, &transaction)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `INSERT INTO payment_credit_attempts(id,payer_user_id,payment_order_id,transaction_digest,facts_digest,fingerprint,state,resolved_at) VALUES($1,$2,$3,$4,$5,$6,'ABORTED',clock_timestamp())`, input.ID, input.UserID, input.OrderID, input.TransactionDigest, input.FactsDigest, input.Fingerprint())
		if err != nil {
			return decision, err
		}
		decision.State = "ABORTED"
		return decision, tx.Commit(ctx)
	}
	if err != nil {
		return decision, err
	}
	if fingerprint != input.Fingerprint() {
		return decision, ErrConflict
	}
	if state == "ABORTED" {
		decision.State = state
		return decision, tx.Commit(ctx)
	}
	if transaction == nil {
		return decision, paymentcredit.ErrProtocol
	}
	var credited, effects bool
	if err = tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM payment_transactions transaction JOIN entitlement_packages package ON package.source_payment_transaction_id=transaction.id JOIN journal_entries journal ON journal.business_reference IN ('payment-transaction:'||transaction.id::text,'repair-payment-transaction:'||transaction.id::text) WHERE transaction.id=$1 AND transaction.payment_order_id=$2 AND transaction.fulfillment_status='CREDITED'),
 EXISTS(SELECT 1 FROM entitlement_packages WHERE source_payment_transaction_id=$1) OR EXISTS(SELECT 1 FROM journal_entries WHERE business_reference IN ('payment-transaction:'||$1::text,'repair-payment-transaction:'||$1::text))`, *transaction, input.OrderID).Scan(&credited, &effects); err != nil {
		return decision, err
	}
	if state == "APPLIED" {
		if !credited {
			return decision, paymentcredit.ErrProtocol
		}
		decision.State = "APPLIED"
		return decision, tx.Commit(ctx)
	}
	if state != "PREPARED" || effects {
		return decision, paymentcredit.ErrProtocol
	}
	if _, err = tx.Exec(ctx, `UPDATE payment_credit_attempts SET state='ABORTED',resolved_at=clock_timestamp() WHERE id=$1 AND state='PREPARED'`, input.ID); err != nil {
		return decision, err
	}
	decision.State = "ABORTED"
	return decision, tx.Commit(ctx)
}

// AcknowledgePaymentCredit records Account's durable terminal outcome. This is
// metadata only: it neither grants money nor releases a pending admission.
func (r *PostgresRepository) AcknowledgePaymentCredit(ctx context.Context, input paymentcredit.Request, state string) (paymentcredit.Decision, error) {
	decision := paymentcredit.Decision{ID: input.ID, Fingerprint: input.Fingerprint(), State: state}
	if !input.Valid() || (state != "APPLIED" && state != "ABORTED") {
		return paymentcredit.Decision{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return decision, err
	}
	defer tx.Rollback(ctx)
	var payer uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT payer_user_id FROM payment_orders WHERE id=$1 FOR UPDATE`, input.OrderID).Scan(&payer); err != nil {
		return decision, err
	}
	if payer != input.UserID {
		return decision, ErrConflict
	}
	command, err := tx.Exec(ctx, `UPDATE payment_credit_attempts SET account_acknowledged_at=COALESCE(account_acknowledged_at,clock_timestamp()) WHERE id=$1 AND fingerprint=$2 AND state=$3`, input.ID, input.Fingerprint(), state)
	if err != nil {
		return decision, err
	}
	if command.RowsAffected() != 1 {
		return decision, paymentcredit.ErrProtocol
	}
	return decision, tx.Commit(ctx)
}
