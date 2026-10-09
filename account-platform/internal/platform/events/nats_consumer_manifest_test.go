package events

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
)

func consumerManifestHash(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func TestApprovalConsumerManifestAndExplicitRenderer(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	current, previous, keys, _ := natsV2Fixture(t, now, time.Hour)
	identityHash := consumerManifestHash(current)
	manifest, err := ApprovalConsumerManifest(identityHash)
	if err != nil {
		t.Fatal(err)
	}
	manifestHash := consumerManifestHash(manifest)
	base, err := renderNATSAuthorizationAt(current, previous, now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := renderNATSAuthorizationWithConsumersAt(current, previous, manifest, identityHash, manifestHash, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.IdentitySHA256 != identityHash || result.ManifestSHA256 != manifestHash || result.ConfigurationSHA256 != consumerManifestHash(result.Configuration) {
		t.Fatal("render binding not retained")
	}
	if bytes.Contains(result.Configuration, []byte(`\u003e`)) || bytes.Contains(result.Configuration, []byte(`\u003c`)) || bytes.Contains(result.Configuration, []byte(`\u0026`)) {
		t.Fatal("JSON HTML escapes are not valid NATS configuration")
	}
	read := func(raw []byte) map[string]json.RawMessage {
		t.Helper()
		var decoded struct {
			Authorization struct {
				Users []struct {
					NKey        string
					Permissions json.RawMessage
				}
			}
		}
		if json.Unmarshal(raw, &decoded) != nil {
			t.Fatal("invalid render")
		}
		users := map[string]json.RawMessage{}
		for _, user := range decoded.Authorization.Users {
			users[user.NKey] = user.Permissions
		}
		return users
	}
	before, after := read(base), read(result.Configuration)
	if len(before)+1 != len(after) {
		t.Fatal("identity set changed")
	}
	for key, old := range before {
		var left, right any
		_ = json.Unmarshal(old, &left)
		_ = json.Unmarshal(after[key], &right)
		if key != keys[approvalconsumer.BillingWorkload] && key != keys["event-resource-operator"] && !reflect.DeepEqual(left, right) {
			t.Fatal("unrelated or existing Token permission changed")
		}
	}
	var billing natsPermissions
	if json.Unmarshal(after[keys[approvalconsumer.BillingWorkload]], &billing) != nil {
		t.Fatal("billing permissions")
	}
	expected := fixedNATSPermissions(approvalconsumer.BillingWorkload)
	for _, descriptor := range approvalconsumer.BillingCommands() {
		expected.Publish = append(expected.Publish,
			"$JS.API.CONSUMER.INFO."+platformStream+"."+descriptor.Durable,
			"$JS.API.CONSUMER.MSG.NEXT."+platformStream+"."+descriptor.Durable,
			"$JS.ACK."+platformStream+"."+descriptor.Durable+".>")
	}
	if !reflect.DeepEqual(billing, expected) {
		t.Fatal("unexpected extra/missing ACL")
	}
	var token natsPermissions
	if json.Unmarshal(after[keys[approvalconsumer.TokenWorkload]], &token) != nil {
		t.Fatal("Token permission absent")
	}
	wantToken := fixedNATSPermissions(approvalconsumer.TokenWorkload)
	for _, d := range approvalconsumer.TokenCommands() {
		wantToken.Publish = append(wantToken.Publish, "$JS.API.CONSUMER.INFO."+platformStream+"."+d.Durable,
			"$JS.API.CONSUMER.MSG.NEXT."+platformStream+"."+d.Durable, "$JS.ACK."+platformStream+"."+d.Durable+".>")
	}
	if !reflect.DeepEqual(token, wantToken) {
		t.Fatal("Token ACL must be exactly its three read/ack durables")
	}
	if _, exists := after[keys["token-outbox-relay"]]; exists {
		t.Fatal("future ordinary Token publisher was issued")
	}
	old, parseErr := parseNATSIdentityArtifact(previous)
	if parseErr != nil || old.keys[approvalconsumer.TokenWorkload] != keys[approvalconsumer.TokenWorkload] || !bytes.Equal(after[old.keys[approvalconsumer.TokenWorkload]], after[keys[approvalconsumer.TokenWorkload]]) {
		t.Fatal("validated v1 overlap must preserve the exact Token executor key and ACL")
	}
	var operator natsPermissions
	if json.Unmarshal(after[keys["event-resource-operator"]], &operator) != nil {
		t.Fatal("operator permission absent")
	}
	wantOperator := fixedNATSPermissions("event-resource-operator")
	for _, d := range approvalconsumer.TokenCommands() {
		wantOperator.Publish = append(wantOperator.Publish,
			"$JS.API.CONSUMER.INFO."+platformStream+"."+d.Durable, "$JS.API.CONSUMER.CREATE."+platformStream+"."+d.Durable+"."+approvalconsumer.AuthorizationSubject)
	}
	if !reflect.DeepEqual(operator, wantOperator) {
		t.Fatal("operator ACL must add only three immutable creates/info")
	}
	again, err := renderNATSAuthorizationAt(current, previous, now)
	if err != nil || !bytes.Equal(base, again) {
		t.Fatal("legacy renderer drifted")
	}
}

func TestApprovalConsumerManifestFailsClosed(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	current, previous, _, _ := natsV2Fixture(t, now, time.Hour)
	identityHash := consumerManifestHash(current)
	manifest, _ := ApprovalConsumerManifest(identityHash)
	for name, altered := range map[string][]byte{
		"missing":         nil,
		"unknown-field":   append(append([]byte{}, manifest[:len(manifest)-1]...), []byte(`,"extra":true}`)...),
		"duplicate-field": bytes.Replace(manifest, []byte(`"stream":`), []byte(`"stream":"OTHER","stream":`), 1),
		"physical-limit":  bytes.ReplaceAll(manifest, []byte(`"max_deliver":-1`), []byte(`"max_deliver":12`)),
		"arbitrary-start": bytes.ReplaceAll(manifest, []byte(`"quiescing_high_watermark_plus_one"`), []byte(`"42"`)),
		"wrong-identity":  bytes.ReplaceAll(manifest, []byte(identityHash), []byte(strings.Repeat("0", 64))),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := renderNATSAuthorizationWithConsumersAt(current, previous, altered, identityHash, consumerManifestHash(altered), now); err == nil {
				t.Fatal("altered closed manifest accepted")
			}
		})
	}
	for _, identity := range []string{"", strings.ToUpper(identityHash), strings.Repeat("0", 64)} {
		if _, err := renderNATSAuthorizationWithConsumersAt(current, previous, manifest, identity, consumerManifestHash(manifest), now); err == nil {
			t.Fatal("untrusted identity accepted")
		}
	}
	if _, err := renderNATSAuthorizationWithConsumersAt(current, previous, manifest, identityHash, strings.Repeat("0", 64), now); err == nil {
		t.Fatal("wrong manifest digest accepted")
	}
	if _, err := renderNATSAuthorizationWithConsumersAt(current, previous, manifest, identityHash, consumerManifestHash(manifest), now.Add(time.Hour)); err == nil {
		t.Fatal("expired overlap accepted")
	}
	var changed map[string]any
	if json.Unmarshal(current, &changed) != nil {
		t.Fatal("fixture decode failed")
	}
	changed["public_keys"].(map[string]any)[approvalconsumer.TokenWorkload] = changed["public_keys"].(map[string]any)["account-approval-relay"]
	changedRaw, _ := json.Marshal(changed)
	changedManifest, _ := ApprovalConsumerManifest(consumerManifestHash(changedRaw))
	if _, err := renderNATSAuthorizationWithConsumersAt(changedRaw, previous, changedManifest, consumerManifestHash(changedRaw), consumerManifestHash(changedManifest), now); err == nil {
		t.Fatal("changed Token executor key was accepted during fixed-key v1 overlap")
	}
	v1Hash := consumerManifestHash(previous)
	v1Manifest, _ := ApprovalConsumerManifest(v1Hash)
	if _, err := renderNATSAuthorizationWithConsumersAt(previous, nil, v1Manifest, v1Hash, consumerManifestHash(v1Manifest), now); err == nil {
		t.Fatal("v1 silently extended")
	}
}
