package identityadapter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/passwordpreparation"
	"github.com/google/uuid"
)

func BenchmarkPasswordExpiryReferenceLookup(b *testing.B) {
	for _, count := range []int{16, DefaultPasswordReferenceCapacity} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			v, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
			if err != nil {
				b.Fatal(err)
			}
			now := time.Now().UTC()
			v.now = func() time.Time { return now }
			var reference PasswordReference
			for range count {
				reference, err = v.Precheck("BenchmarkPassword9")
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for range b.N {
				if _, err := v.record(reference.Handle, reference.PolicyVersion); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkPasswordExpiryOperationFence(b *testing.B) {
	for _, count := range []int{16, DefaultPasswordReferenceCapacity} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			v, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
			if err != nil {
				b.Fatal(err)
			}
			now := time.Now().UTC()
			v.now = func() time.Time { return now }
			ref, err := v.Precheck("BenchmarkPassword9")
			if err != nil {
				b.Fatal(err)
			}
			var id uuid.UUID
			for range count {
				id = uuid.New()
				if err := v.ValidateReference(id, ref.Handle, ref.PolicyVersion, "benchmark", "+8613800138000"); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for range b.N {
				if err := v.registrationDispatchFence(id, ref.Handle, ref.PolicyVersion, "benchmark", "+8613800138000", false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkPasswordExpiryPublicReplay(b *testing.B) {
	b.Setenv("APP_ENV", "test")
	for _, name := range []string{"WORKLOAD_IDENTITY_ISSUER", "WORKLOAD_IDENTITY_JWKS_URL", "WORKLOAD_IDENTITY_AUDIENCE", "WORKLOAD_IDENTITY_RULES_JSON", "WORKLOAD_IDENTITY_CA_FILE"} {
		b.Setenv(name, "")
	}
	for _, count := range []int{16, DefaultPasswordReferenceCapacity} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			v, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
			if err != nil {
				b.Fatal(err)
			}
			now := time.Now().UTC()
			v.now = func() time.Time { return now }
			service, err := NewService(v, "controlled-benchmark-token")
			if err != nil {
				b.Fatal(err)
			}
			verifier := preparationClaimFunc(func(_ context.Context, claim passwordpreparation.Claim) (passwordpreparation.Facts, error) {
				id, err := uuid.Parse(strings.Split(claim.Token, ".")[0])
				return passwordpreparation.Facts{ID: id, Mobile: "+8613800138000", Purpose: "REGISTER", ExpiresAt: now.Add(time.Minute)}, err
			})
			service, err = service.WithPasswordPreparation(verifier, "")
			if err != nil {
				b.Fatal(err)
			}
			handler := service.Handler()
			request := func(token string) {
				r := httptest.NewRequest(http.MethodPost, "/v1/registration/password-prechecks", strings.NewReader(`{"password":"BenchmarkPassword9"}`))
				r.RemoteAddr = "192.0.2.10:4242"
				r.Header.Set("X-Device-ID", "fixture-device")
				r.Header.Set("X-Pinvou-Password-Preparation", token)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != http.StatusCreated {
					b.Fatal("public precheck rejected", w.Code)
				}
			}
			var token string
			for range count {
				token = uuid.New().String() + ".PUBLIC_SYNTHETIC_MAC"
				request(token)
			}
			b.ResetTimer()
			for range b.N {
				request(token)
			}
		})
	}
}
