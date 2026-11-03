package billing

import (
	"strings"
	"testing"
)

func TestEntitlementCorrectionReasonCodeUsesUTF8ByteLimit(t *testing.T) {
	if !validEntitlementCorrectionReasonCode(strings.Repeat("A", 80)) {
		t.Fatal("80-byte ASCII reason code should be accepted")
	}
	if validEntitlementCorrectionReasonCode(strings.Repeat("A", 81)) {
		t.Fatal("81-byte reason code should be rejected")
	}
	if validEntitlementCorrectionReasonCode(strings.Repeat("中", 27)) {
		t.Fatal("27 Chinese characters exceed the 80-byte limit")
	}
	if !validEntitlementCorrectionReasonCode(strings.Repeat("中", 26)) {
		t.Fatal("26 Chinese characters fit within the 80-byte limit")
	}
}
