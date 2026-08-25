package h3ccallback

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cpayment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcallback"
	"github.com/google/uuid"
)

var (
	ErrInvalidNotification = errors.New("invalid H3C payment callback")
	notificationNamespace  = uuid.MustParse("20f1afca-8d3a-4d74-9fb6-36aacdd952b0")
)

type Profile struct{}

func NewProfile() *Profile { return &Profile{} }

func (*Profile) ProviderID() string { return "h3c_mall" }

func (*Profile) ProviderVersion() string { return "h3c-mall-v1" }

// ParseNotification accepts the Mall's native order snapshot. It is the same
// object returned by queryOrder, so callback and active query convergence do
// not depend on two vendor payload definitions drifting apart.
func (*Profile) ParseNotification(body []byte, now time.Time) (paymentcallback.Notification, error) {
	if len(body) == 0 || len(body) > 1<<20 {
		return paymentcallback.Notification{}, ErrInvalidNotification
	}
	var order h3cmall.Order
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&order) != nil || decoder.Decode(&struct{}{}) == nil {
		return paymentcallback.Notification{}, ErrInvalidNotification
	}
	order.OrderKey = strings.TrimSpace(order.OrderKey)
	order.MerchantCustomerKey = strings.TrimSpace(order.MerchantCustomerKey)
	order.Mobile = strings.TrimSpace(order.Mobile)
	order.SKU = strings.TrimSpace(order.SKU)
	order.Status = strings.ToUpper(strings.TrimSpace(order.Status))
	customerID, customerErr := uuid.Parse(order.MerchantCustomerKey)
	eventAt, eventErr := h3cmall.ParseTimeValue(order.UpdatedAt)
	if order.OrderKey == "" || len(order.OrderKey) > 128 || customerErr != nil || customerID.String() != strings.ToLower(order.MerchantCustomerKey) || !validMainlandMobile(order.Mobile) || order.SKU == "" || len(order.SKU) > 128 || order.Status == "" || eventErr != nil || eventAt.After(now.Add(5*time.Minute)) {
		return paymentcallback.Notification{}, ErrInvalidNotification
	}
	provider, err := h3cpayment.MapMallOrder(order, nil)
	if err != nil || provider.ProviderEventAt == nil || provider.ExternalCustomerKey != order.MerchantCustomerKey || provider.ExternalSKU != order.SKU {
		return paymentcallback.Notification{}, ErrInvalidNotification
	}
	transactionID := ""
	if provider.ChannelTransactionID != nil {
		transactionID = *provider.ChannelTransactionID
	}
	identity := strings.Join([]string{
		order.OrderKey, order.MerchantCustomerKey, order.SKU, order.Status,
		transactionID, eventAt.UTC().Format(time.RFC3339Nano),
	}, "\x00")
	return paymentcallback.Notification{
		ID:       uuid.NewSHA1(notificationNamespace, []byte(identity)),
		SentAt:   now.UTC(),
		Provider: provider,
	}, nil
}

func validMainlandMobile(value string) bool {
	value = strings.TrimPrefix(strings.TrimSpace(value), "+86")
	if len(value) != 11 || value[0] != '1' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
