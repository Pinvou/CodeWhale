package billing

import (
	"crypto/sha256"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPaymentSourceLockKeyEncodesHashAsUTF8Text(t *testing.T) {
	hash := sha256.Sum256([]byte("203.0.113.19"))
	key := paymentSourceLockKey("CREATE", hash)

	if !utf8.ValidString(key) {
		t.Fatal("payment source advisory lock key must be valid UTF-8")
	}
	const prefix = "payment:CREATE:source:"
	encoded := strings.TrimPrefix(key, prefix)
	if encoded == key || len(encoded) != sha256.Size*2 {
		t.Fatalf("unexpected payment source lock key %q", key)
	}
	for _, character := range encoded {
		if !strings.ContainsRune("0123456789abcdef", character) {
			t.Fatalf("payment source hash must use hexadecimal text, got %q", encoded)
		}
	}
}
