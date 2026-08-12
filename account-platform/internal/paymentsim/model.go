package paymentsim

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound = errors.New("simulated payment not found")
	ErrConflict = errors.New("simulated payment conflict")
	ErrExpired  = errors.New("simulated payment expired")
)

type CreateOrder struct {
	MerchantOrderNo string
	Channel         string
	AmountFen       int64
	Currency        string
	CallbackURL     string
	Scenario        string
	ExpiresAt       time.Time
}

type Order struct {
	ID                   uuid.UUID  `json:"order_id"`
	MerchantOrderNo      string     `json:"merchant_order_no"`
	Channel              string     `json:"channel"`
	AmountFen            int64      `json:"amount_fen"`
	Currency             string     `json:"currency"`
	Status               string     `json:"status"`
	Scenario             string     `json:"scenario,omitempty"`
	PaymentURL           string     `json:"payment_url,omitempty"`
	ChannelTransactionID *string    `json:"channel_transaction_id,omitempty"`
	ExpiresAt            time.Time  `json:"expires_at"`
	PaidAt               *time.Time `json:"paid_at,omitempty"`
}

type Notification struct {
	ID                  uuid.UUID
	OrderID             uuid.UUID
	CallbackURL         string
	Channel             string
	Payload             []byte
	Signature           string
	Attempts            int
	RemainingDeliveries int
}

type Repository interface {
	Create(context.Context, CreateOrder) (Order, bool, error)
	GetByMerchantOrder(context.Context, string) (Order, error)
	GetByPaymentToken(context.Context, string) (Order, error)
	Complete(context.Context, string) (Order, error)
	ClaimNotifications(context.Context, int) ([]Notification, error)
	MarkNotification(context.Context, Notification, error) error
	Ping(context.Context) error
	Close()
}
