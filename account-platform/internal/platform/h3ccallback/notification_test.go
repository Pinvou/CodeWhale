package h3ccallback

import (
	"fmt"
	"testing"
	"time"
)

func TestParsePaidNotification(t *testing.T) {
	now := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	body := []byte(fmt.Sprintf(`{"notificationId":"0198d894-fc00-7000-8000-000000000001","merchantOrderNo":"pv-1","providerOrderNo":"mall-1","providerTransactionId":"trade-1","channel":"ALIPAY","providerStatus":"PAID","amountFen":100,"currency":"CNY","paidAt":"2026-08-24 15:59:00","occurredAt":"2026-08-24T07:59:00Z","sentAt":"%s","traceId":"trace-1"}`, now.Format(time.RFC3339)))
	notification, err := NewProfile().ParseNotification(body, now)
	if err != nil || notification.Provider.Status != "SUCCESS" || notification.Provider.ChannelTransactionID == nil || *notification.Provider.ChannelTransactionID != "trade-1" || notification.Provider.AmountSource != "PROVIDER" || notification.Provider.ProviderOrderReference != "mall-1" {
		t.Fatalf("unexpected notification: %#v %v", notification, err)
	}
}

func TestFailedNotificationRequiresReviewAndUnknownFieldsAreRejected(t *testing.T) {
	now := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	body := []byte(fmt.Sprintf(`{"notificationId":"0198d894-fc00-7000-8000-000000000002","merchantOrderNo":"pv-2","channel":"ALIPAY","providerStatus":"FAILED","amountFen":100,"currency":"CNY","occurredAt":"%s","sentAt":"%s"}`, now.Format(time.RFC3339), now.Format(time.RFC3339)))
	profile := NewProfile()
	notification, err := profile.ParseNotification(body, now)
	if err != nil || notification.Provider.Status != "REVIEW_REQUIRED" {
		t.Fatalf("FAILED must remain non-terminal: %#v %v", notification, err)
	}
	if _, err := profile.ParseNotification(append(body[:len(body)-1], []byte(`,"unexpected":true}`)...), now); err == nil {
		t.Fatal("unknown callback field was accepted")
	}
}
