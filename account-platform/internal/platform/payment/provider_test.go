package payment

import "testing"

func TestCapabilitiesRequireVersionedQueryableProvider(t *testing.T) {
	valid := Capabilities{ProviderID: "provider", Version: "v1", Channels: []string{"CARD_PAY"}, OrderQuerySupported: true, MinimumQueryIntervalSeconds: 5}
	if !valid.Valid() || !valid.SupportsChannel("card_pay") {
		t.Fatal("valid versioned capabilities were rejected")
	}
	invalid := valid
	invalid.MinimumQueryIntervalSeconds = 0
	if invalid.Valid() {
		t.Fatal("query capability without a minimum interval was accepted")
	}
	invalid = valid
	invalid.Channels = []string{"CARD_PAY", "card_pay"}
	if invalid.Valid() {
		t.Fatal("duplicate case-insensitive channels were accepted")
	}
	invalid = valid
	invalid.RefundSupported = true
	if invalid.Valid() {
		t.Fatal("refund capability without an idempotency guarantee was accepted")
	}
	invalid.RefundIdempotencySupported = true
	if !invalid.Valid() {
		t.Fatal("idempotent refund capability was rejected")
	}
}

func TestCapabilitiesRejectInvalidQRScanApps(t *testing.T) {
	valid := Capabilities{ProviderID: "provider", Version: "v1", Channels: []string{"CARD_PAY"}, QRScanApps: []string{"ALIPAY", "WECHAT"}}
	if !valid.Valid() {
		t.Fatal("valid QR scan applications were rejected")
	}
	for _, apps := range [][]string{{"ALIPAY", "ALIPAY"}, {"ALIPAY", "alipay"}, {"alipay"}, {" ALIPAY "}, {"BAD-APP"}, {""}} {
		candidate := valid
		candidate.QRScanApps = apps
		if candidate.Valid() {
			t.Fatalf("invalid QR scan applications were accepted: %#v", apps)
		}
	}
}

func TestFormalCollectionRequiresEvidenceOrBoundedAuthoritativeHistory(t *testing.T) {
	capabilities := Capabilities{NotificationSupported: true}
	if !capabilities.FormalCollectionEligible(0) {
		t.Fatal("notification-capable provider was rejected")
	}
	capabilities = Capabilities{OrderQuerySupported: true, MaximumPayableWindowSeconds: 900, MaximumLateConfirmationSeconds: 3600, OrderHistoryRetentionSeconds: 900 + 3600 + 7*24*60*60, AuthoritativeUnpaidTerminal: true}
	if !capabilities.FormalCollectionEligible(0) {
		t.Fatal("bounded authoritative history was rejected")
	}
	capabilities.OrderHistoryRetentionSeconds--
	if capabilities.FormalCollectionEligible(0) {
		t.Fatal("insufficient query retention was accepted")
	}
}
