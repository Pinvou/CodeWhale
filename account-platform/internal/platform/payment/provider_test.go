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
}
