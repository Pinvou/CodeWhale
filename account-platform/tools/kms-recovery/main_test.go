package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func TestCustodyRoundTripAndWrongPassphrase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custody")
	pass := []byte("fixture-passphrase-never-production")
	if err := prepare(dir, pass); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CUSTODY_READY")); err != nil {
		t.Fatal(err)
	}
	identity, err := unlock(dir, pass)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unlock(dir, []byte("wrong-fixture-passphrase")); err == nil {
		t.Fatal("wrong password accepted")
	}
	plain := []byte(`{"test_only":true,"unseal":"fixture-canary"}`)
	encrypted, err := encrypt(plain, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, plain) {
		t.Fatal("plaintext appeared in bundle")
	}
	decoded, err := decrypt(encrypted, identity)
	if err != nil || !bytes.Equal(plain, decoded) {
		t.Fatal("roundtrip failed")
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, err := decrypt(encrypted, identity); err == nil {
		t.Fatal("corrupted bundle accepted")
	}
	if err := prepare(dir, pass); err == nil {
		t.Fatal("existing custody overwritten")
	}
	key, _ := os.ReadFile(filepath.Join(dir, "identity.age"))
	if bytes.Contains(key, []byte(identity.String())) || bytes.Contains(key, pass) {
		t.Fatal("identity or password persisted in plaintext")
	}
}

func TestCustodyRefusesUnsafeInputs(t *testing.T) {
	for _, pass := range []string{"", "short", "                "} {
		if _, _, err := wrapIdentity([]byte(pass)); err == nil {
			t.Fatal("weak password accepted")
		}
	}
	if err := prepare("relative-directory", []byte("long-fixture-passphrase")); err == nil {
		t.Fatal("relative path accepted")
	}
	path := filepath.Join(t.TempDir(), "existing.age")
	if err := writeNew(path, []byte("existing")); err != nil {
		t.Fatal(err)
	}
	if err := writeNew(path, []byte("replacement")); err == nil {
		t.Fatal("existing file overwritten")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "existing" {
		t.Fatal("original file changed")
	}
}

func TestCustodyRejectsMismatchedRecipientAndTruncation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custody")
	pass := []byte("fixture-long-passphrase-2")
	if err := prepare(dir, pass); err != nil {
		t.Fatal(err)
	}
	other, _ := age.GenerateX25519Identity()
	if err := os.WriteFile(filepath.Join(dir, "recipient.txt"), []byte(other.Recipient().String()), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := unlock(dir, pass); err == nil {
		t.Fatal("replaced recipient accepted")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "identity.age"))
	id, _ := age.NewScryptIdentity(string(pass))
	id.SetMaxWorkFactor(18)
	if _, err := decrypt(b[:len(b)-1], id); err == nil {
		t.Fatal("truncated identity accepted")
	}
}
