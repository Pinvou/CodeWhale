package h3ccallback

import (
	"fmt"
	"testing"
	"time"
)

func TestParsePaidNativeOrderSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	body := []byte(fmt.Sprintf(`{"id":1,"orderKey":"pv-1","merchantCustomerKey":"019c0000-0000-7000-8000-000000000001","mobile":"13800138000","skuBom":"0502AJ0Q","tradeOrderCode":"TRO-1","skuId":2,"needPayAmt":0.10,"paidAmt":0.10,"paymentStatus":"PAY_SUCCESS","payEndTime":"2026-08-25 16:00:00","skuAmt":0.10,"receiveAddressId":3,"shopId":4,"tradeContractId":5,"tradeContractCode":"TRC-1","payUniqueKey":"trade-1","paymentOrderId":6,"buyerId":"buyer-test","payChannel":"CHINAUMS_PAY_ALI_QR","status":"PAID","node":"支付完成","payTime":"2026-08-25 15:59:00","executeCostMs":7,"traceId":"trace-1","createdAt":"2026-08-25 15:58:00","updatedAt":"%s"}`, now.In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02 15:04:05")))
	notification, err := NewProfile().ParseNotification(body, now)
	if err != nil || notification.Provider.Status != "SUCCESS" || notification.Provider.ChannelTransactionID == nil || *notification.Provider.ChannelTransactionID != "trade-1" || notification.Provider.AmountFen != 10 || notification.Provider.AmountSource != "PROVIDER" || notification.Provider.ProviderOrderReference != "TRO-1" || notification.Provider.ExternalCustomerKey == "" || notification.Provider.ExternalSKU != "0502AJ0Q" {
		t.Fatalf("unexpected notification: %#v %v", notification, err)
	}
	repeated, err := NewProfile().ParseNotification(body, now.Add(time.Second))
	if err != nil || repeated.ID != notification.ID {
		t.Fatalf("duplicate Mall snapshot did not derive a stable notification ID: %#v %v", repeated, err)
	}
}

func TestFailedNativeSnapshotRequiresReviewAndUnknownFieldsAreRejected(t *testing.T) {
	now := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	body := []byte(`{"orderKey":"pv-2","merchantCustomerKey":"019c0000-0000-7000-8000-000000000002","mobile":"13800138000","skuBom":"0502AJ0Q","needPayAmt":0.10,"skuAmt":0.10,"status":"FAILED","errorMsg":"order failed","updatedAt":"2026-08-25 16:00:00"}`)
	profile := NewProfile()
	notification, err := profile.ParseNotification(body, now)
	if err != nil || notification.Provider.Status != "REVIEW_REQUIRED" {
		t.Fatalf("FAILED must remain non-terminal: %#v %v", notification, err)
	}
	if _, err := profile.ParseNotification(append(body[:len(body)-1], []byte(`,"unexpected":true}`)...), now); err == nil {
		t.Fatal("unknown callback field was accepted")
	}
}

func TestPaidNativeSnapshotRequiresProviderMoneyFacts(t *testing.T) {
	now := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	body := []byte(`{"orderKey":"pv-3","merchantCustomerKey":"019c0000-0000-7000-8000-000000000003","mobile":"13800138000","skuBom":"0502AJ0Q","status":"PAID","payUniqueKey":"trade-3","payTime":"2026-08-25 15:59:00","updatedAt":"2026-08-25 16:00:00"}`)
	if _, err := NewProfile().ParseNotification(body, now); err == nil {
		t.Fatal("paid callback without Mall amount facts was accepted")
	}
}
