package account

import (
	"encoding/hex"
	"testing"
)

func TestRegistrationBonusFingerprintKnownVectors(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	repository := &PostgresRepository{}
	if err := repository.ConfigureRegistrationBonusFingerprint("known-vector-v1", key); err != nil {
		t.Fatal(err)
	}
	key[0] = 0xff // Configuration must retain its own copy of the public test key.

	// Independently calculated with Python's standard HMAC-SHA256, key bytes
	// 00..1f, and the contract's NUL-separated mobile and verifier domains.
	// Expected values are literal: changing production domains cannot change them.
	t.Run("mobile", func(t *testing.T) {
		keyID, digest, err := repository.registrationBonusFingerprint("+8613800138000")
		if err != nil {
			t.Fatal(err)
		}
		if keyID != "known-vector-v1" || hex.EncodeToString(digest) != "a6cc67b296f382b293c87eb1385929ddb3991d1a9046d58209e6a4ff7404c03c" {
			t.Fatal("fingerprint differs from the independent contract vector")
		}
	})
	t.Run("key-verifier", func(t *testing.T) {
		digest, err := repository.registrationBonusFingerprintKeyVerifier()
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(digest) != "cb3cb411f885c7f86b33c4ee308c5270232bb55568736bed996934ddea09d115" {
			t.Fatal("key verifier differs from its independent contract vector")
		}
	})
}
