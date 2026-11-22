package events

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestRefundUpgradeACLOnlyAddsExactOperatorSubject(t *testing.T) {
	files, _ := existingReceiptFixture(t)
	_, base, err := files.Identity.ExistingBrokerAuthorizationArtifacts(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	_, temporary, err := files.Identity.RefundUpgradeAuthorizationArtifacts(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var original, changed struct {
		Authorization struct {
			Users []struct {
				NKey        string          `json:"nkey"`
				Permissions json.RawMessage `json:"permissions"`
			} `json:"users"`
		} `json:"authorization"`
	}
	if json.Unmarshal(base.Configuration, &original) != nil || json.Unmarshal(temporary.Configuration, &changed) != nil {
		t.Fatal("invalid ACL")
	}
	raw, _, err := readNATSPublicArtifact(files.Identity.CurrentFile, files.Identity.CurrentSHA256)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := parseNATSIdentityArtifact(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(original.Authorization.Users) != len(changed.Authorization.Users) {
		t.Fatal("identity count changed")
	}
	const subject = "$JS.API.CONSUMER.CREATE.ACCOUNT_PLATFORM.billing-refund-executor-v1.billing.refund.requested.v1"
	hits := 0
	for i := range changed.Authorization.Users {
		user := &changed.Authorization.Users[i]
		if user.NKey == identity.keys["event-resource-operator"] {
			var permissions natsPermissions
			if json.Unmarshal(user.Permissions, &permissions) != nil {
				t.Fatal("operator ACL invalid")
			}
			for j, entry := range permissions.Publish {
				if entry == subject {
					hits++
					permissions.Publish = append(permissions.Publish[:j], permissions.Publish[j+1:]...)
					break
				}
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if enc.Encode(permissions) != nil {
				t.Fatal("encode failed")
			}
			user.Permissions = bytes.TrimSpace(buf.Bytes())
		}
	}
	if hits != 1 || !reflect.DeepEqual(original, changed) {
		t.Fatal("temporary ACL differs beyond exact operator subject")
	}
}
