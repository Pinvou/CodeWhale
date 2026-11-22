package main

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"strings"
)

type registrationBonusBootstrap interface {
	ConfigureRegistrationBonusFingerprint(string, []byte) error
	PrepareRegistrationBonusEligibility(context.Context) (account.RegistrationBonusEligibilityReadiness, error)
}

// Configure before accepting registrations. BLOCKED historical readiness stays
// visible and prevents policy activation; it does not invent mobile eligibility.
func configureRegistrationBonus(ctx context.Context, r registrationBonusBootstrap, mobileEnabled bool, keyID, encodedKey string) error {
	if !mobileEnabled {
		return nil
	}
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(encodedKey))
	if err != nil || len(key) != 32 {
		return errors.New("registration bonus fingerprint configuration is invalid")
	}
	defer clear(key)
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		keyID = "registration-bonus-v1"
	}
	if err = r.ConfigureRegistrationBonusFingerprint(keyID, key); err != nil {
		return err
	}
	_, err = r.PrepareRegistrationBonusEligibility(ctx)
	return err
}
