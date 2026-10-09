package account

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
)

func (r *PostgresRepository) registrationBonusFingerprint(normalizedMobile string) (string, []byte, error) {
	if r.registrationBonusFingerprintKeyID == "" || len(r.registrationBonusFingerprintKey) != 32 {
		return "", nil, errors.New("registration bonus fingerprint is not configured")
	}
	digest := hmac.New(sha256.New, r.registrationBonusFingerprintKey)
	_, _ = digest.Write([]byte("pinvou-registration-bonus-v1\x00" + normalizedMobile))
	return r.registrationBonusFingerprintKeyID, digest.Sum(nil), nil
}

func (r *PostgresRepository) registrationBonusFingerprintKeyVerifier() ([]byte, error) {
	if r.registrationBonusFingerprintKeyID == "" || len(r.registrationBonusFingerprintKey) != 32 {
		return nil, errors.New("registration bonus fingerprint is not configured")
	}
	digest := hmac.New(sha256.New, r.registrationBonusFingerprintKey)
	_, _ = digest.Write([]byte("pinvou-registration-bonus-key-verifier-v1\x00"))
	return digest.Sum(nil), nil
}
