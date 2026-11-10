package events

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTokenOutboxAuthorizationPreservesExistingIdentities(t *testing.T) {
	now := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	current, previous, keys, _ := natsV2Fixture(t, now, time.Hour)
	public := t.TempDir()
	currentFile, currentSHA := writeNATSPublicArtifactFixture(t, public, "current.json", current)
	previousFile, previousSHA := writeNATSPublicArtifactFixture(t, public, "previous.json", previous)
	files := NATSAuthorizationArtifactFiles{CurrentFile: currentFile, CurrentSHA256: currentSHA, PreviousFile: previousFile, PreviousSHA256: previousSHA}
	if publicKey, err := files.TokenOutboxPublicKey(now); err != nil || publicKey != keys["token-outbox-relay"] {
		t.Fatal("Token runtime did not obtain its dedicated trusted identity", err)
	}
	read := func(raw []byte) map[string]json.RawMessage {
		t.Helper()
		var decoded struct {
			Authorization struct {
				Users []struct {
					NKey        string          `json:"nkey"`
					Permissions json.RawMessage `json:"permissions"`
				} `json:"users"`
			} `json:"authorization"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		result := map[string]json.RawMessage{}
		for _, user := range decoded.Authorization.Users {
			if _, duplicate := result[user.NKey]; duplicate {
				t.Fatal("duplicate rendered identity")
			}
			result[user.NKey] = user.Permissions
		}
		return result
	}
	for _, kind := range []TokenOutboxAuthorizationKind{TokenOutboxAuthorizationFresh, TokenOutboxAuthorizationExistingBroker, TokenOutboxAuthorizationRefundUpgrade} {
		t.Run(string(kind), func(t *testing.T) {
			var oldManifest []byte
			var before NATSConsumerAuthorizationRender
			var err error
			switch kind {
			case TokenOutboxAuthorizationFresh:
				oldManifest, before, err = files.ConsumerAuthorizationArtifacts(now)
			case TokenOutboxAuthorizationExistingBroker:
				oldManifest, before, err = files.ExistingBrokerAuthorizationArtifacts(now)
			case TokenOutboxAuthorizationRefundUpgrade:
				oldManifest, before, err = files.RefundUpgradeAuthorizationArtifacts(now)
			}
			if err != nil {
				t.Fatal(err)
			}
			manifest, after, err := files.TokenOutboxAuthorizationArtifacts(now, kind)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(oldManifest, manifest) || after.IdentitySHA256 != before.IdentitySHA256 || after.ManifestSHA256 != before.ManifestSHA256 || after.ConfigurationSHA256 != consumerManifestHash(after.Configuration) {
				t.Fatal("existing public bindings changed")
			}
			oldUsers, newUsers := read(before.Configuration), read(after.Configuration)
			if len(newUsers) != len(oldUsers)+1 || oldUsers[keys["token-outbox-relay"]] != nil {
				t.Fatal("legacy renderer issued Token publisher or new identity set differs")
			}
			for key, permission := range oldUsers {
				if !bytes.Equal(permission, newUsers[key]) {
					t.Fatal("existing identity permissions changed", key)
				}
			}
			var token natsPermissions
			if err := json.Unmarshal(newUsers[keys["token-outbox-relay"]], &token); err != nil {
				t.Fatal(err)
			}
			expected := natsPermissions{Publish: []string{"$JS.API.STREAM.INFO.ACCOUNT_PLATFORM", "token.>", "account-platform.dead-letter.token.>"}, Subscribe: []string{"_INBOX.token-outbox-relay.>"}}
			if !reflect.DeepEqual(token, expected) {
				t.Fatal("Token publisher acquired unexpected capabilities")
			}
			if bytes.Contains(after.Configuration, []byte(`\u003e`)) || bytes.Contains(after.Configuration, []byte(`\u003c`)) {
				t.Fatal("configuration contains unsupported HTML escapes")
			}
		})
	}
	if _, _, err := files.TokenOutboxAuthorizationArtifacts(now, "caller-selected"); err == nil {
		t.Fatal("unknown authorization carrier accepted")
	}
	if _, _, err := files.TokenOutboxAuthorizationArtifacts(now.Add(2*time.Hour), TokenOutboxAuthorizationFresh); err == nil {
		t.Fatal("expired overlap acquired Token publisher")
	}
	files.CurrentSHA256 = previousSHA
	if _, _, err := files.TokenOutboxAuthorizationArtifacts(now, TokenOutboxAuthorizationFresh); err == nil {
		t.Fatal("mismatching public identity digest accepted")
	}
}

func TestNATSInitialFileIdentityMatchesTrustedKey(t *testing.T) {
	private := t.TempDir()
	seed, public := natsFixtureSeed(t, private, "token")
	_, other := natsFixtureSeed(t, private, "other-domain")
	if _, err := natsAuthenticationWithExpectedIdentity("tls://localhost:4222", seed, "", public); err != nil {
		t.Fatal("matching seed refused", err)
	}
	if _, err := natsAuthenticationWithExpectedIdentity("tls://localhost:4222", seed, "", other); err == nil {
		t.Fatal("wrong initial seed identity accepted")
	}
	if _, err := natsAuthenticationWithExpectedIdentity("tls://user:password@localhost:4222", "", "", public); err == nil {
		t.Fatal("trusted file identity fell back to URL credentials")
	}
	body, credentialPublic := stagedCredentialsFixture(t, private)
	defer clear(body)
	credentials := filepath.Join(private, "token.creds")
	if err := os.WriteFile(credentials, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := natsAuthenticationWithExpectedIdentity("tls://localhost:4222", "", credentials, credentialPublic); err != nil {
		t.Fatal("matching credential key refused", err)
	}
	if _, err := natsAuthenticationWithExpectedIdentity("tls://localhost:4222", "", credentials, other); err == nil {
		t.Fatal("wrong initial credential identity accepted")
	}
}

func TestTokenNATSSeedInitKeepsTypeAndIdentityBoundary(t *testing.T) {
	root := t.TempDir()
	source, expected := natsFixtureSeed(t, root, "controlled-token")
	destination := filepath.Join(root, "private", "token-event-relay.seed")
	if err := StageNATSSeed(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := natsAuthenticationWithExpectedIdentity("tls://localhost:4222", destination, "", expected); err != nil {
		t.Fatal("staged seed cannot use its pinned identity", err)
	}
	if err := StageNATSSeed(source, destination); err != nil {
		t.Fatal("same seed retry failed", err)
	}
	other, _ := natsFixtureSeed(t, root, "other-role")
	if err := StageNATSSeed(other, destination); err == nil {
		t.Fatal("init replaced the accepted runtime identity")
	}
	if _, err := natsAuthenticationWithExpectedIdentity("tls://localhost:4222", destination, "", expected); err != nil {
		t.Fatal("rejected replacement changed the existing identity", err)
	}
	credential, _ := stagedCredentialsFixture(t, root)
	defer clear(credential)
	credentialsFile := filepath.Join(root, "token.creds")
	if err := os.WriteFile(credentialsFile, credential, 0600); err != nil {
		t.Fatal(err)
	}
	if err := StageNATSSeed(credentialsFile, filepath.Join(root, "private", "invalid.seed")); err == nil {
		t.Fatal("seed initializer accepted another credential document type")
	}
	for _, client := range []string{"account-event-relay", "../other"} {
		if handled, err := PrepareNATSSeed([]string{"prepare-nats-seed", client}, "token-event-relay"); !handled || err == nil {
			t.Fatal("init accepted an unregistered client")
		}
	}
}
