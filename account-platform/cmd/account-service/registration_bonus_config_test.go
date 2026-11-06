package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"testing"
)

type bonusBootstrapFake struct {
	configured, prepared int
	key                  []byte
	keyID                string
	err                  error
}

func (f *bonusBootstrapFake) ConfigureRegistrationBonusFingerprint(id string, key []byte) error {
	f.configured++
	f.keyID = id
	f.key = append([]byte(nil), key...)
	return nil
}
func (f *bonusBootstrapFake) PrepareRegistrationBonusEligibility(context.Context) (account.RegistrationBonusEligibilityReadiness, error) {
	f.prepared++
	return account.RegistrationBonusEligibilityReadiness{Status: "BLOCKED"}, f.err
}
func TestRegistrationBonusBootstrapConfiguration(t *testing.T) {
	t.Run("mobile-disabled-does-not-configure", func(t *testing.T) {
		f := &bonusBootstrapFake{}
		if configureRegistrationBonus(context.Background(), f, false, "", "") != nil || f.configured != 0 || f.prepared != 0 {
			t.Fatal("disabled mobile touched eligibility")
		}
	})
	t.Run("invalid-secret-before-repository", func(t *testing.T) {
		for _, key := range []string{"", "invalid", base64.StdEncoding.EncodeToString(make([]byte, 31))} {
			f := &bonusBootstrapFake{}
			if configureRegistrationBonus(context.Background(), f, true, "", key) == nil || f.configured != 0 {
				t.Fatal("bad key reached repository")
			}
		}
	})
	t.Run("strict-key-before-readiness-blocked-remains-visible", func(t *testing.T) {
		f := &bonusBootstrapFake{}
		key := bytes.Repeat([]byte{7}, 32)
		if configureRegistrationBonus(context.Background(), f, true, "", base64.StdEncoding.EncodeToString(key)) != nil || f.configured != 1 || f.prepared != 1 || f.keyID != "registration-bonus-v1" || !bytes.Equal(key, f.key) {
			t.Fatal("configuration/prepare order differs")
		}
	})
	t.Run("prepare-error-prevents-service-start", func(t *testing.T) {
		f := &bonusBootstrapFake{err: errors.New("controlled unavailable")}
		if configureRegistrationBonus(context.Background(), f, true, "named-key", base64.StdEncoding.EncodeToString(make([]byte, 32))) == nil || f.prepared != 1 {
			t.Fatal("failed prepare accepted")
		}
	})
}
