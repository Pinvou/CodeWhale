package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type resetFloorProvider struct {
	smsProviderFake
	delay time.Duration
}

func (p *resetFloorProvider) VerifyCode(ctx context.Context, mobile string, id uuid.UUID, data SMSVerificationData, code string) (bool, error) {
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return p.smsProviderFake.VerifyCode(ctx, mobile, id, data, code)
}

func TestPasswordResetVerificationResponseFloor(t *testing.T) {
	for _, mode := range []string{"unsent-or-denied", "provider-rejected", "provider-error", "verified"} {
		t.Run(mode, func(t *testing.T) {
			r := &mobileAuthRepositoryFake{}
			p := &resetFloorProvider{}
			if mode == "unsent-or-denied" {
				r.readyErr = ErrSMSInvalid
			} else if mode == "provider-error" {
				p.verifyErr = errors.New("controlled provider unavailable")
			} else if mode == "verified" {
				p.verified = true
			}
			auth, err := NewMobileAuthService(r, p, &identityProvisionerFake{}, "https://identity.example.invalid", mustResetFloorSigner(t))
			if err != nil {
				t.Fatal(err)
			}
			if auth.loginResponseFloor != 750*time.Millisecond {
				t.Fatal("production floor changed")
			}
			start := time.Now()
			verified, err := auth.VerifyPasswordReset(context.Background(), "+8613800138000", uuid.New(), "123456")
			if elapsed := time.Since(start); elapsed < 740*time.Millisecond {
				t.Fatalf("existence-sensitive %s response escaped configured floor: %s", mode, elapsed)
			}
			if mode == "unsent-or-denied" && (!errors.Is(err, ErrSMSInvalid) || p.verifyCalls != 0) {
				t.Fatal("invalid challenge called provider or changed domain rejection", err, p.verifyCalls)
			}
			if mode == "provider-error" && !errors.Is(err, ErrSMSUnavailable) {
				t.Fatal("provider failure changed", err)
			}
			if verified != (mode == "verified") {
				t.Fatal("verification result changed", verified, err)
			}
		})
	}
	t.Run("cancellation-never-turns-verification-successful", func(t *testing.T) {
		auth := newMobileAuthForTest(t, &mobileAuthRepositoryFake{}, &smsProviderFake{verified: true}, &identityProvisionerFake{})
		auth.now = func() time.Time { return time.Now().UTC() }
		auth.loginResponseFloor = 750 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		defer cancel()
		verified, err := auth.VerifyPasswordReset(ctx, "+8613800138000", uuid.New(), "123456")
		if verified || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("cancelled request succeeded", verified, err)
		}
	})
	t.Run("floor-is-total-request-time-not-extra-provider-delay", func(t *testing.T) {
		p := &resetFloorProvider{delay: 500 * time.Millisecond}
		auth := newMobileAuthForTest(t, &mobileAuthRepositoryFake{}, p, &identityProvisionerFake{})
		auth.now = func() time.Time { return time.Now().UTC() }
		auth.loginResponseFloor = 300 * time.Millisecond
		start := time.Now()
		_, err := auth.VerifyPasswordReset(context.Background(), "+8613800138000", uuid.New(), "123456")
		elapsed := time.Since(start)
		if err != nil || elapsed < 490*time.Millisecond || elapsed > 700*time.Millisecond {
			t.Fatal("slow provider paid an extra response floor", elapsed, err)
		}
	})
}

func mustResetFloorSigner(t *testing.T) *RegistrationTokenSigner {
	t.Helper()
	signer, err := NewRegistrationTokenSigner(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
