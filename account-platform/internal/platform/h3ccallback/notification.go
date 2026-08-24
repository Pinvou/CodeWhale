package h3ccallback

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcallback"
	"github.com/google/uuid"
)

var ErrInvalidNotification = errors.New("invalid H3C payment callback")

type callbackPayload struct {
	NotificationID              uuid.UUID `json:"notificationId"`
	MerchantOrderNo             string    `json:"merchantOrderNo"`
	ProviderOrderNo             string    `json:"providerOrderNo"`
	ProviderTransactionID       string    `json:"providerTransactionId"`
	Channel                     string    `json:"channel"`
	ProviderStatus              string    `json:"providerStatus"`
	AmountFen                   int64     `json:"amountFen"`
	Currency                    string    `json:"currency"`
	PaidAt                      string    `json:"paidAt"`
	OccurredAt                  time.Time `json:"occurredAt"`
	SentAt                      time.Time `json:"sentAt"`
	TraceID                     string    `json:"traceId"`
	ProviderPayableUntil        time.Time `json:"providerPayableUntil"`
	PaymentInstructionExpiresAt time.Time `json:"paymentInstructionExpiresAt"`
}

type Profile struct{}

func NewProfile() *Profile { return &Profile{} }

func (*Profile) ProviderID() string { return "h3c_mall" }

func (*Profile) ProviderVersion() string { return "h3c-mall-v1" }

func (*Profile) ParseNotification(body []byte, now time.Time) (paymentcallback.Notification, error) {
	if len(body) == 0 || len(body) > 1<<20 {
		return paymentcallback.Notification{}, ErrInvalidNotification
	}
	var payload callbackPayload
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(&struct{}{}) == nil {
		return paymentcallback.Notification{}, ErrInvalidNotification
	}
	payload.MerchantOrderNo = strings.TrimSpace(payload.MerchantOrderNo)
	payload.ProviderOrderNo = strings.TrimSpace(payload.ProviderOrderNo)
	payload.ProviderTransactionID = strings.TrimSpace(payload.ProviderTransactionID)
	payload.Channel = strings.ToUpper(strings.TrimSpace(payload.Channel))
	payload.ProviderStatus = strings.ToUpper(strings.TrimSpace(payload.ProviderStatus))
	payload.Currency = strings.ToUpper(strings.TrimSpace(payload.Currency))
	payload.TraceID = strings.TrimSpace(payload.TraceID)
	if payload.Currency == "" {
		payload.Currency = "CNY"
	}
	if payload.Channel == "" {
		payload.Channel = "ALIPAY"
	}
	if payload.NotificationID == uuid.Nil || payload.MerchantOrderNo == "" || len(payload.MerchantOrderNo) > 128 || payload.Channel != "ALIPAY" || payload.AmountFen <= 0 || payload.Currency != "CNY" || payload.SentAt.IsZero() || payload.OccurredAt.IsZero() || payload.SentAt.Before(now.Add(-5*time.Minute)) || payload.SentAt.After(now.Add(5*time.Minute)) || payload.OccurredAt.After(payload.SentAt.Add(time.Minute)) || payload.ProviderStatus == "" || len(payload.ProviderStatus) > 64 {
		return paymentcallback.Notification{}, ErrInvalidNotification
	}
	provider := payment.ProviderOrder{
		MerchantOrderNo: payload.MerchantOrderNo, ProviderOrderReference: payload.ProviderOrderNo,
		Status: normalizedStatus(payload.ProviderStatus), ProviderStatus: payload.ProviderStatus, Channel: payload.Channel,
		AmountFen: payload.AmountFen, Currency: payload.Currency, ProviderTraceID: payload.TraceID,
		ProviderEventAt: &payload.OccurredAt, ProviderPayableUntil: optionalTime(payload.ProviderPayableUntil),
		PaymentInstructionExpiresAt: optionalTime(payload.PaymentInstructionExpiresAt), AmountSource: "PROVIDER",
	}
	if provider.Status == "SUCCESS" {
		paidAt, err := parseProviderTime(payload.PaidAt)
		if payload.ProviderTransactionID == "" || err != nil {
			return paymentcallback.Notification{}, ErrInvalidNotification
		}
		provider.ChannelTransactionID, provider.PaidAt = &payload.ProviderTransactionID, &paidAt
	}
	return paymentcallback.Notification{ID: payload.NotificationID, SentAt: payload.SentAt.UTC(), Provider: provider}, nil
}

func normalizedStatus(status string) string {
	switch status {
	case "PAID":
		return "SUCCESS"
	case "PENDING", "PROCESSING", "WAIT_PAY":
		return "PENDING"
	case "PAY_CLOSED":
		return "CLOSED"
	case "FAILED":
		return "REVIEW_REQUIRED"
	default:
		// Unknown future statuses are preserved as evidence and require review.
		return "REVIEW_REQUIRED"
	}
}

func parseProviderTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, location)
	if err != nil {
		return time.Time{}, ErrInvalidNotification
	}
	return parsed.UTC(), nil
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}
