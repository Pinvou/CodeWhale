package account

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAuditExportEnvelopeEncryptionRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x31}, 32)
	plaintext := []byte("# WATERMARK export_id=test\naudit_id,action\n1,read\n")
	aad := []byte("export_id=test;operator_id=actor;environment=test")
	nonce, ciphertext, err := encryptAEAD(key, plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decryptAEAD(key, nonce, ciphertext, aad)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, plaintext) {
		t.Fatal("encrypted audit payload did not round trip")
	}
	if _, err := decryptAEAD(key, nonce, ciphertext, []byte("different-watermark")); err == nil {
		t.Fatal("watermark was not authenticated as envelope metadata")
	}
}

func TestAuditExportRecentAuthenticationAndStableErrors(t *testing.T) {
	now := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	if !recentlyAuthenticated(now.Add(-15*time.Minute), now) {
		t.Fatal("15 minute boundary should be accepted")
	}
	if recentlyAuthenticated(now.Add(-15*time.Minute-time.Nanosecond), now) || recentlyAuthenticated(now.Add(2*time.Minute), now) {
		t.Fatal("stale or materially future authentication was accepted")
	}
	if got := stableAuditExportError(ErrAuditExportQuota); got != "LIMIT_EXCEEDED" {
		t.Fatalf("quota classification = %q", got)
	}
	if got := stableAuditExportError(context.Canceled); got != "WORKER_CANCELLED" {
		t.Fatalf("cancel classification = %q", got)
	}
	if got := stableAuditExportError(errors.New("secret provider detail")); got != "GENERATION_FAILED" {
		t.Fatalf("dependency detail leaked: %q", got)
	}
}

func TestAuditExportCSVFormulaAndFilenameProtection(t *testing.T) {
	for _, value := range []string{"=1+1", "+cmd", "-2", "@SUM(A1)"} {
		if protected := safeAuditCSVCell(value); protected != "'"+value {
			t.Fatalf("formula %q was not protected: %q", value, protected)
		}
	}
	if got := safeFilenamePart("production/../../secret"); got != "productionsecret" {
		t.Fatalf("unsafe filename part = %q", got)
	}
}
