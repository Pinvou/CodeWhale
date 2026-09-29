package account

import (
	"bytes"
	"testing"
)

func TestRegistrationBonusFingerprintIsStableAndDomainSeparated(t *testing.T) {
	repository := &PostgresRepository{}
	key := bytes.Repeat([]byte{0x5a}, 32)
	if err := repository.ConfigureRegistrationBonusFingerprint("registration-bonus-v1", key); err != nil {
		t.Fatal(err)
	}
	key[0] = 0
	keyID, first, err := repository.registrationBonusFingerprint("+8613800138000")
	if err != nil {
		t.Fatal(err)
	}
	_, replay, err := repository.registrationBonusFingerprint("+8613800138000")
	if err != nil {
		t.Fatal(err)
	}
	_, different, err := repository.registrationBonusFingerprint("+8613800138001")
	if err != nil {
		t.Fatal(err)
	}
	if keyID != "registration-bonus-v1" || len(first) != 32 || !bytes.Equal(first, replay) || bytes.Equal(first, different) {
		t.Fatalf("keyID=%q first=%x replay=%x different=%x", keyID, first, replay, different)
	}
	if bytes.Contains(first, []byte("13800138000")) {
		t.Fatal("fingerprint leaked raw mobile content")
	}
}

func TestRegistrationBonusFingerprintRejectsInvalidConfiguration(t *testing.T) {
	repository := &PostgresRepository{}
	if err := repository.ConfigureRegistrationBonusFingerprint("", bytes.Repeat([]byte{1}, 32)); err == nil {
		t.Fatal("expected empty key id to fail")
	}
	if err := repository.ConfigureRegistrationBonusFingerprint("registration-bonus-v1", bytes.Repeat([]byte{1}, 31)); err == nil {
		t.Fatal("expected invalid key length to fail")
	}
	if _, _, err := repository.registrationBonusFingerprint("+8613800138000"); err == nil {
		t.Fatal("expected unconfigured repository to fail closed")
	}
}
