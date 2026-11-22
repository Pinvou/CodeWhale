package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nkeys"
)

func natsV2Fixture(t *testing.T, now time.Time, window time.Duration) ([]byte, []byte, map[string]string, map[string]string) {
	t.Helper()
	previous, old := natsPublicFixture(t)
	keys := map[string]string{}
	for _, role := range []string{"billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"} {
		keys[role] = old[role]
	}
	for _, role := range []string{"account-outbox-relay", "account-approval-relay"} {
		pair, err := nkeys.CreateUser()
		if err != nil {
			t.Fatal(err)
		}
		keys[role], err = pair.PublicKey()
		pair.Wipe()
		if err != nil {
			t.Fatal(err)
		}
	}
	digest := sha256.Sum256(previous)
	current, err := json.Marshal(map[string]any{
		"version":            2,
		"purpose":            "pinvou-nats-service-identities",
		"public_keys":        keys,
		"previous_v1_sha256": hex.EncodeToString(digest[:]),
		"overlap_expires_at": now.Add(window).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	return current, previous, keys, old
}

func writeNATSPublicArtifactFixture(t *testing.T, directory, name string, raw []byte) (string, string) {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return path, hex.EncodeToString(digest[:])
}

func TestNATSAuthorizationArtifactFilesFailClosedAtConfiguredOverlapDeadline(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	current, previous, _, _ := natsV2Fixture(t, now, time.Hour)
	publicDirectory := t.TempDir()
	privateDirectory := t.TempDir()
	currentFile, currentDigest := writeNATSPublicArtifactFixture(t, publicDirectory, "current-v2.json", current)
	previousFile, previousDigest := writeNATSPublicArtifactFixture(t, publicDirectory, "previous-v1.json", previous)
	privateFile := filepath.Join(privateDirectory, "nkey.seed")
	files := NATSAuthorizationArtifactFiles{
		CurrentFile: currentFile, CurrentSHA256: currentDigest,
		PreviousFile: previousFile, PreviousSHA256: previousDigest,
		PrivateIdentityFiles: []string{privateFile},
	}
	if err := files.Validate(now); err != nil {
		t.Fatal(err)
	}
	if err := files.Validate(now.Add(time.Hour - time.Second)); err != nil {
		t.Fatal("overlap failed before its exact deadline", err)
	}
	if err := files.Validate(now.Add(time.Hour)); err != ErrNATSResources {
		t.Fatal("configured previous identity remained ready at its deadline")
	}
	files.PreviousFile, files.PreviousSHA256 = "", ""
	if err := files.Validate(now.Add(8 * 24 * time.Hour)); err != ErrNATSResources {
		t.Fatal("stale previous public projection remained ready after configuration removal")
	}
	if err := os.Remove(previousFile); err != nil {
		t.Fatal(err)
	}
	if err := files.Validate(now.Add(8 * 24 * time.Hour)); err != nil {
		t.Fatal("final v2 artifact did not recover readiness after previous removal", err)
	}
}

func TestNATSAuthorizationArtifactFilesRejectsUntrustedOrPrivateProjection(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	current, previous, _, _ := natsV2Fixture(t, now, time.Hour)
	publicDirectory := t.TempDir()
	privateDirectory := t.TempDir()
	currentFile, currentDigest := writeNATSPublicArtifactFixture(t, publicDirectory, "current-v2.json", current)
	previousFile, previousDigest := writeNATSPublicArtifactFixture(t, publicDirectory, "previous-v1.json", previous)
	privateFile := filepath.Join(privateDirectory, "client.creds")
	valid := NATSAuthorizationArtifactFiles{CurrentFile: currentFile, CurrentSHA256: currentDigest, PreviousFile: previousFile, PreviousSHA256: previousDigest, PrivateIdentityFiles: []string{privateFile}}
	if err := valid.Validate(now); err != nil {
		t.Fatal(err)
	}
	extraSeed := filepath.Join(publicDirectory, "unexpected.seed")
	if err := os.WriteFile(extraSeed, []byte("secret-must-not-be-visible"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := valid.Validate(now); err != ErrNATSResources {
		t.Fatal("public artifact projection accepted an unexpected seed file")
	}
	if err := os.Remove(extraSeed); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]NATSAuthorizationArtifactFiles{
		"half-previous":         {CurrentFile: currentFile, CurrentSHA256: currentDigest, PreviousFile: previousFile, PrivateIdentityFiles: []string{privateFile}},
		"untrusted-current":     {CurrentFile: currentFile, CurrentSHA256: strings.Repeat("0", 64), PreviousFile: previousFile, PreviousSHA256: previousDigest, PrivateIdentityFiles: []string{privateFile}},
		"public-in-private-dir": {CurrentFile: filepath.Join(privateDirectory, "public.json"), CurrentSHA256: currentDigest, PrivateIdentityFiles: []string{privateFile}},
		"same-current-previous": {CurrentFile: currentFile, CurrentSHA256: currentDigest, PreviousFile: currentFile, PreviousSHA256: currentDigest, PrivateIdentityFiles: []string{privateFile}},
	}
	if err := os.WriteFile(filepath.Join(privateDirectory, "public.json"), current, 0600); err != nil {
		t.Fatal(err)
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			if err := mutation.Validate(now); err != ErrNATSResources {
				t.Fatalf("unsafe artifact projection accepted: %v", err)
			}
		})
	}
	v1Directory := t.TempDir()
	v1File, v1Digest := writeNATSPublicArtifactFixture(t, v1Directory, "current-v1.json", previous)
	valid.CurrentFile, valid.CurrentSHA256, valid.PreviousFile, valid.PreviousSHA256 = v1File, v1Digest, "", ""
	if err := valid.Validate(now); err != ErrNATSResources {
		t.Fatal("v1 artifact accepted for the dedicated authorization relay")
	}
}

func decodedNATSUsers(t *testing.T, raw []byte) []struct {
	NKey        string          `json:"nkey"`
	Permissions json.RawMessage `json:"permissions"`
} {
	t.Helper()
	var decoded struct {
		Authorization struct {
			Users []struct {
				NKey        string          `json:"nkey"`
				Permissions json.RawMessage `json:"permissions"`
			} `json:"users"`
		} `json:"authorization"`
	}
	if json.Unmarshal(raw, &decoded) != nil {
		t.Fatal("authorization output")
	}
	return decoded.Authorization.Users
}

func sameJSON(t *testing.T, left, right []byte) bool {
	t.Helper()
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		t.Fatal("permission JSON")
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func TestNATSAuthorizationV2BoundOverlapAndRevocationProjection(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	current, previous, _, _ := natsV2Fixture(t, now, time.Hour)
	withOld, err := renderNATSAuthorizationAt(current, previous, now)
	if err != nil {
		t.Fatal(err)
	}
	var overlap struct {
		Authorization struct {
			Users []json.RawMessage `json:"users"`
		} `json:"authorization"`
	}
	if json.Unmarshal(withOld, &overlap) != nil || len(overlap.Authorization.Users) != 6 {
		t.Fatal("overlap must deduplicate five shared users")
	}
	withoutOld, err := renderNATSAuthorizationAt(current, nil, now.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(withoutOld, &overlap) != nil || len(overlap.Authorization.Users) != 5 {
		t.Fatal("v2 final user set")
	}
}

func TestNATSAuthorizationV2ExactKeysPermissionsAndOrder(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	current, previous, keys, old := natsV2Fixture(t, now, time.Hour)
	v1Raw, err := renderNATSAuthorizationAt(previous, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	v1Users := decodedNATSUsers(t, v1Raw)
	v1Permissions := map[string]json.RawMessage{}
	for _, user := range v1Users {
		v1Permissions[user.NKey] = user.Permissions
	}
	overlapRaw, err := renderNATSAuthorizationAt(current, previous, now)
	if err != nil {
		t.Fatal(err)
	}
	users := decodedNATSUsers(t, overlapRaw)
	expectedRoles := []string{"account-outbox-relay", "account-approval-relay", "billing-outbox-relay", "billing-command-executor", "event-resource-operator", "account-outbox-relay"}
	if len(users) != len(expectedRoles) {
		t.Fatal("closed overlap user count")
	}
	for index, role := range expectedRoles {
		expectedKey := keys[role]
		if index == len(expectedRoles)-1 {
			expectedKey = old[role]
		}
		if users[index].NKey != expectedKey {
			t.Fatalf("user order or key mismatch at %d", index)
		}
	}
	for _, role := range []string{"billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"} {
		for _, user := range users {
			if user.NKey == keys[role] && !sameJSON(t, user.Permissions, v1Permissions[old[role]]) {
				t.Fatalf("shared permissions changed: %s", role)
			}
		}
	}
	expectedGeneric, _ := json.Marshal(map[string]any{
		"publish": map[string]any{
			"allow": []string{"$JS.API.STREAM.INFO.ACCOUNT_PLATFORM", "account.>", "account-platform.dead-letter.account.>"},
			"deny":  []string{"account.approval-command.authorized.v1", "account-platform.dead-letter.account.approval-command.authorized.v1"},
		},
		"subscribe": map[string]any{"allow": []string{"_INBOX.account-outbox-relay.>"}},
	})
	expectedApproval, _ := json.Marshal(map[string]any{
		"publish": map[string]any{
			"allow": []string{"account.approval-command.authorized.v1", "account-platform.dead-letter.account.approval-command.authorized.v1"},
		},
		"subscribe": map[string]any{"allow": []string{"_INBOX.account-approval-relay.>"}},
	})
	if !sameJSON(t, users[0].Permissions, expectedGeneric) || !sameJSON(t, users[1].Permissions, expectedApproval) ||
		!sameJSON(t, users[len(users)-1].Permissions, v1Permissions[old["account-outbox-relay"]]) {
		t.Fatal("relay permission projection changed")
	}
}

func TestNATSAuthorizationV2RejectsWrongPreviousAndExpiredOverlap(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	_, previous, keys, _ := natsV2Fixture(t, now, time.Hour)
	for _, kind := range []string{"digest", "expired", "wrong-key"} {
		digest := sha256.Sum256(previous)
		digestText := hex.EncodeToString(digest[:])
		expiry := now.Add(time.Hour).Format(time.RFC3339)
		copyKeys := map[string]string{}
		for k, v := range keys {
			copyKeys[k] = v
		}
		if kind == "digest" {
			digestText = string(make([]byte, 64))
		}
		if kind == "expired" {
			expiry = now.Add(-time.Hour).Format(time.RFC3339)
		}
		if kind == "wrong-key" {
			pair, _ := nkeys.CreateUser()
			copyKeys["billing-outbox-relay"], _ = pair.PublicKey()
			pair.Wipe()
		}
		current, _ := json.Marshal(map[string]any{"version": 2, "purpose": "pinvou-nats-service-identities", "public_keys": copyKeys, "previous_v1_sha256": digestText, "overlap_expires_at": expiry})
		if _, err := renderNATSAuthorizationAt(current, previous, now); err != ErrNATSResources {
			t.Fatalf("accepted %s", kind)
		}
	}
}

func TestNATSAuthorizationV2OverlapBoundsAndClosedShape(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		window  time.Duration
		allowed bool
	}{
		{"expired", -time.Second, false},
		{"at-expiry", 0, false},
		{"one-second-before-expiry", time.Second, true},
		{"maximum", natsIdentityOverlapMaximum, true},
		{"above-maximum", natsIdentityOverlapMaximum + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current, previous, _, _ := natsV2Fixture(t, now, tc.window)
			_, err := renderNATSAuthorizationAt(current, previous, now)
			if tc.allowed && err != nil {
				t.Fatal(err)
			}
			if !tc.allowed && err != ErrNATSResources {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
	current, previous, _, _ := natsV2Fixture(t, now, time.Hour)
	var object map[string]any
	json.Unmarshal(current, &object)
	mutations := map[string][]byte{}
	for _, field := range []string{"version", "purpose", "public_keys", "previous_v1_sha256", "overlap_expires_at"} {
		copyObject := map[string]any{}
		for key, value := range object {
			copyObject[key] = value
		}
		delete(copyObject, field)
		mutations["missing-"+field], _ = json.Marshal(copyObject)
	}
	copyObject := map[string]any{}
	for key, value := range object {
		copyObject[key] = value
	}
	copyObject["unexpected"] = true
	mutations["unknown"], _ = json.Marshal(copyObject)
	mutations["trailing"] = append(append([]byte{}, current...), []byte(`{}`)...)
	mutations["null"] = []byte(`null`)
	mutations["duplicate"] = []byte(`{"version":2,"version":2}`)
	for name, raw := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := renderNATSAuthorizationAt(raw, previous, now); err != ErrNATSResources {
				t.Fatal("accepted non-closed artifact")
			}
		})
	}
}
