package securityadmin

import (
	"encoding/base64"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
)

func TestGenerateBootstrapCredentialHas256BitsAndBase64URL(t *testing.T) {
	secret, err := GenerateBootstrapCredential()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(decoded) != 32 || len(secret) != 43 {
		t.Fatalf("unexpected credential shape: length=%d decoded=%d err=%v", len(secret), len(decoded), err)
	}
	first := bootstrapVerifier("deployment-a", 1, secret)
	second := bootstrapVerifier("deployment-a", 2, secret)
	if first == [32]byte{} || first == second {
		t.Fatal("credential verifier must be purpose and generation bound")
	}
}

func TestPrivilegedPasswordPolicy(t *testing.T) {
	for _, value := range []string{"Admin0123456789", "Welcome0123456789", "short123A"} {
		if err := identityadapter.ValidatePrivilegedPassword(value, "admin"); err == nil {
			t.Fatalf("expected privileged password rejection for %q", value)
		}
	}
	if err := identityadapter.ValidatePrivilegedPassword("S3cure-Quartz-71", "admin"); err != nil {
		t.Fatalf("expected strong password acceptance: %v", err)
	}
}
