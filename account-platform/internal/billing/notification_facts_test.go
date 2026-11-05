package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
)

type notificationBoundaryProvider struct{ payment.Provider }

func (notificationBoundaryProvider) Capabilities() payment.Capabilities {
	return payment.Capabilities{ProviderID: "fixture-provider", Version: "v1", Channels: []string{"ALIPAY"}, NotificationSupported: true}
}

func TestNotificationRejectsUnconfiguredSecretBeforeStorage(t *testing.T) {
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	p := &PaymentCoordinator{provider: notificationBoundaryProvider{}, merchantID: "fixture-merchant", now: func() time.Time { return now }}
	n := paymentNotification{NotificationID: uuid.New(), MerchantID: "fixture-merchant", MerchantOrderNo: "fixture-order", Channel: "ALIPAY", Status: "SUCCESS", AmountFen: 4900, Currency: "CNY", PaidAt: now, Timestamp: now.Unix()}
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() != nil {
			t.Fatal("notification with an unconfigured secret reached storage")
		}
	}()
	_, _, err = p.PersistNotification(context.Background(), "ALIPAY", paymentSignature(nil, raw), raw, "source:fixture")
	if !errors.Is(err, ErrForbiddenPayment) {
		t.Fatal("unconfigured notification secret was accepted")
	}
}

func TestNotificationRetryRetainsAllPaymentFactsExceptSigningTime(t *testing.T) {
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	base := paymentNotification{NotificationID: uuid.New(), MerchantID: "fixture-merchant", MerchantOrderNo: "fixture-order", ChannelTransaction: "fixture-transaction", Channel: "ALIPAY", Status: "SUCCESS", AmountFen: 9007199254740993, Currency: "CNY", PaidAt: now, Timestamp: now.Unix(), ProviderOrderReference: "provider-order", ProviderStatus: "PAID", ProviderTraceID: "provider-trace", ExternalCustomerKey: uuid.NewString(), ExternalSKU: "fixture-sku", ProviderEventAt: &now, ProviderPayableUntil: &now, PaymentInstructionExpiresAt: &now, ProviderPayload: json.RawMessage(`{"fixture_fact":"immutable"}`)}
	retry := base
	retry.Timestamp += 600
	if !sameNotificationPaymentFacts(base, retry) {
		t.Fatal("refreshing the retry signing time changed payment identity")
	}
	changes := map[string]func(*paymentNotification){
		"notification-id": func(n *paymentNotification) { n.NotificationID = uuid.New() },
		"merchant":        func(n *paymentNotification) { n.MerchantID += "-other" },
		"order":           func(n *paymentNotification) { n.MerchantOrderNo += "-other" },
		"transaction":     func(n *paymentNotification) { n.ChannelTransaction += "-other" },
		"channel":         func(n *paymentNotification) { n.Channel = "WECHAT" },
		"status":          func(n *paymentNotification) { n.Status = "FAILED" },
		"amount":          func(n *paymentNotification) { n.AmountFen++ },
		"currency":        func(n *paymentNotification) { n.Currency = "USD" },
		"paid-time":       func(n *paymentNotification) { n.PaidAt = now.Add(time.Second) },
		"provider-order":  func(n *paymentNotification) { n.ProviderOrderReference += "-other" },
		"provider-status": func(n *paymentNotification) { n.ProviderStatus = "REFUNDED" },
		"provider-trace":  func(n *paymentNotification) { n.ProviderTraceID += "-other" },
		"customer":        func(n *paymentNotification) { n.ExternalCustomerKey = uuid.NewString() },
		"sku":             func(n *paymentNotification) { n.ExternalSKU += "-other" },
		"provider-time": func(n *paymentNotification) {
			v := now.Add(time.Second)
			n.ProviderEventAt = &v
		},
		"payable-until": func(n *paymentNotification) {
			v := now.Add(time.Second)
			n.ProviderPayableUntil = &v
		},
		"instruction-until": func(n *paymentNotification) {
			v := now.Add(time.Second)
			n.PaymentInstructionExpiresAt = &v
		},
		"provider-payload": func(n *paymentNotification) { n.ProviderPayload = json.RawMessage(`{"fixture_fact":"different"}`) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			different := retry
			change(&different)
			if sameNotificationPaymentFacts(base, different) {
				t.Fatal("a changed payment fact reused the original notification identity")
			}
		})
	}
	if strings.Contains(string(base.ProviderPayload), "different") || base.AmountFen != 9007199254740993 || base.Timestamp != now.Unix() {
		t.Fatal("comparison mutated the original persisted payment facts")
	}
}
