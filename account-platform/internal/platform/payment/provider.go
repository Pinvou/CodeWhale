package payment

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrUnsupported = errors.New("payment provider capability is unsupported")

type Order struct {
	MerchantOrderNo string
	Channel         string
	AmountFen       int64
	Currency        string
	Mobile          string
	ExternalSKU     string
}

type ProviderOrder struct {
	MerchantOrderNo             string     `json:"merchant_order_no"`
	ProviderOrderReference      string     `json:"provider_order_reference,omitempty"`
	PaymentURL                  string     `json:"payment_url"`
	QRData                      string     `json:"qr_data,omitempty"`
	Status                      string     `json:"status"`
	Channel                     string     `json:"channel"`
	AmountFen                   int64      `json:"amount_fen"`
	Currency                    string     `json:"currency"`
	AmountSource                string     `json:"amount_source,omitempty"`
	ChannelTransactionID        *string    `json:"channel_transaction_id,omitempty"`
	PaidAt                      *time.Time `json:"paid_at,omitempty"`
	ProviderStatus              string     `json:"provider_status,omitempty"`
	ProviderTraceID             string     `json:"provider_trace_id,omitempty"`
	ProviderEventAt             *time.Time `json:"provider_event_at,omitempty"`
	ProviderPayableUntil        *time.Time `json:"provider_payable_until,omitempty"`
	PaymentInstructionExpiresAt *time.Time `json:"payment_instruction_expires_at,omitempty"`
}

type Refund struct {
	MerchantRefundNo string `json:"merchant_refund_no"`
	ChannelRefundID  string `json:"channel_refund_id,omitempty"`
	AmountFen        int64  `json:"amount_fen"`
	Status           string `json:"status"`
}

type BillEntry struct {
	EntryType            string    `json:"entry_type,omitempty"`
	ProviderReferenceID  string    `json:"provider_reference_id,omitempty"`
	MerchantOrderNo      string    `json:"merchant_order_no"`
	ChannelTransactionID string    `json:"channel_transaction_id"`
	Channel              string    `json:"channel"`
	AmountFen            int64     `json:"amount_fen"`
	Currency             string    `json:"currency"`
	Status               string    `json:"status"`
	PaidAt               time.Time `json:"paid_at"`
}

type Capabilities struct {
	ProviderID                     string   `json:"provider_id"`
	Version                        string   `json:"version"`
	Channels                       []string `json:"channels"`
	OrderQuerySupported            bool     `json:"order_query_supported"`
	OrderHistoryRetentionSeconds   int64    `json:"order_history_retention_seconds"`
	MinimumQueryIntervalSeconds    int64    `json:"minimum_query_interval_seconds"`
	MaximumPayableWindowSeconds    int64    `json:"maximum_payable_window_seconds"`
	MaximumLateConfirmationSeconds int64    `json:"maximum_late_confirmation_seconds"`
	AuthoritativeUnpaidTerminal    bool     `json:"authoritative_unpaid_terminal"`
	RefundSupported                bool     `json:"refund_supported"`
	RefundIdempotencySupported     bool     `json:"refund_idempotency_supported"`
	CloseSupported                 bool     `json:"close_supported"`
	NotificationSupported          bool     `json:"notification_supported"`
	BillSupported                  bool     `json:"bill_supported"`
	BillFileImportSupported        bool     `json:"bill_file_import_supported"`
	ReconciliationMode             string   `json:"reconciliation_mode"`
	FormalCollectionAdmitted       bool     `json:"formal_collection_admitted"`
	ManualRepairSupported          bool     `json:"manual_repair_supported"`
	CommerceCustomerRequired       bool     `json:"commerce_customer_required"`
	ReusePendingOrderAfterExpiry   bool     `json:"reuse_pending_order_after_expiry"`
	ReviewLatePayment              bool     `json:"review_late_payment"`
	TestOnly                       bool     `json:"test_only"`
}

func (c Capabilities) Valid() bool {
	if strings.TrimSpace(c.ProviderID) == "" || strings.TrimSpace(c.Version) == "" || len(c.Channels) == 0 || c.MinimumQueryIntervalSeconds < 0 || c.OrderHistoryRetentionSeconds < 0 || c.MaximumPayableWindowSeconds < 0 || c.MaximumLateConfirmationSeconds < 0 {
		return false
	}
	seen := map[string]bool{}
	for _, channel := range c.Channels {
		channel = strings.ToUpper(strings.TrimSpace(channel))
		if channel == "" || seen[channel] {
			return false
		}
		seen[channel] = true
	}
	return (!c.OrderQuerySupported || c.MinimumQueryIntervalSeconds > 0) && (!c.RefundSupported || c.RefundIdempotencySupported)
}

func (c Capabilities) SupportsChannel(channel string) bool {
	channel = strings.ToUpper(strings.TrimSpace(channel))
	for _, candidate := range c.Channels {
		if strings.EqualFold(candidate, channel) {
			return true
		}
	}
	return false
}

func (c Capabilities) FormalCollectionEligible(reconciliationGraceSeconds int64) bool {
	if c.NotificationSupported || c.BillSupported || c.BillFileImportSupported {
		return true
	}
	if reconciliationGraceSeconds < 1 {
		reconciliationGraceSeconds = 7 * 24 * 60 * 60
	}
	requiredRetention := c.MaximumPayableWindowSeconds + c.MaximumLateConfirmationSeconds + reconciliationGraceSeconds
	return c.OrderQuerySupported && c.MaximumPayableWindowSeconds > 0 && c.MaximumLateConfirmationSeconds > 0 && c.AuthoritativeUnpaidTerminal && c.OrderHistoryRetentionSeconds >= requiredRetention
}

type Provider interface {
	Capabilities() Capabilities
	Create(context.Context, Order, string) (ProviderOrder, error)
	Get(context.Context, string) (ProviderOrder, error)
	Close(context.Context, string) (ProviderOrder, error)
	Refund(context.Context, string, string, int64) (Refund, error)
	Bill(context.Context, string, time.Time) ([]BillEntry, error)
}
