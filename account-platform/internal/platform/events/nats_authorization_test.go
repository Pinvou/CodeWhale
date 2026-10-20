package events

import (
	"bytes"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"reflect"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

func natsPublicFixture(t *testing.T) ([]byte, map[string]string) {
	t.Helper()
	keys := map[string]string{}
	for _, role := range fixedNATSRoleNames() {
		pair, err := nkeys.CreateUser()
		if err != nil {
			t.Fatal(err)
		}
		pub, err := pair.PublicKey()
		pair.Wipe()
		if err != nil {
			t.Fatal(err)
		}
		keys[role] = pub
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "purpose": "pinvou-nats-service-identities", "public_keys": keys})
	return b, keys
}

func TestNATSFixedAuthorizationAndOverlap(t *testing.T) {
	current, keys := natsPublicFixture(t)
	previous, oldKeys := natsPublicFixture(t)
	for _, old := range [][]byte{nil, previous} {
		raw, err := RenderNATSAuthorization(current, old)
		if err != nil {
			t.Fatal("render")
		}
		var cfg struct {
			Authorization natsAuthorization `json:"authorization"`
		}
		if json.Unmarshal(raw, &cfg) != nil {
			t.Fatal("rendered JSON")
		}
		sets := []map[string]string{keys}
		if old != nil {
			sets = append(sets, oldKeys)
		}
		if len(cfg.Authorization.Users) != 4*len(sets) {
			t.Fatal("unexpected identities")
		}
		for group, set := range sets {
			for i, role := range []string{"account-outbox-relay", "billing-outbox-relay", "billing-command-executor", "event-resource-operator"} {
				user := cfg.Authorization.Users[group*4+i]
				if user.NKey != set[role] || !reflect.DeepEqual(user.Permissions, fixedNATSPermissions(role)) {
					t.Fatal("wrong role ACL")
				}
			}
		}
		if bytes.Contains(raw, []byte(`\u003e`)) || bytes.Contains(raw, []byte(`"password"`)) || bytes.Contains(raw, []byte(`"user"`)) || bytes.Contains(raw, []byte(`"seed"`)) {
			t.Fatal("unsafe NATS policy representation")
		}
	}
	operator := fixedNATSPermissions("event-resource-operator")
	if len(operator.Publish) != 3+2*len(provisionedConsumers())+2*len(approvalconsumer.BillingCommands()) || !reflect.DeepEqual(operator.Subscribe, []string{"_INBOX.event-resource-operator.>"}) {
		t.Fatal("operator permissions")
	}
	// F11 and F12 add exactly INFO and CREATE for the write-off and payment
	// repair durables. The other grants remain fixed and wildcard-free.
	for _, want := range []string{
		"$JS.API.CONSUMER.INFO.ACCOUNT_PLATFORM.billing-payment-financial-repair-executor-p1-v1",
		"$JS.API.CONSUMER.CREATE.ACCOUNT_PLATFORM.billing-payment-financial-repair-executor-p1-v1.account.approval-command.authorized.v1",
		"$JS.API.CONSUMER.INFO.ACCOUNT_PLATFORM.billing-receivable-write-off-executor-p1-v1",
		"$JS.API.CONSUMER.CREATE.ACCOUNT_PLATFORM.billing-receivable-write-off-executor-p1-v1.account.approval-command.authorized.v1",
	} {
		count := 0
		for _, subject := range operator.Publish {
			if subject == want {
				count++
			}
		}
		if count != 1 {
			t.Fatal("own durable exact grant missing or duplicated")
		}
	}
	for _, subject := range operator.Publish {
		if strings.Contains(subject, ">") || strings.Contains(subject, "*") || strings.Contains(subject, ".UPDATE.") || strings.Contains(subject, ".DELETE.") || strings.Contains(subject, ".MSG.GET.") {
			t.Fatal("operator broadened")
		}
	}
	for _, consumer := range provisionedConsumers() {
		want := "$JS.API.CONSUMER.CREATE." + platformStream + "." + consumer.Name + "." + consumer.FilterSubject
		found := false
		for _, subject := range operator.Publish {
			found = found || subject == want
		}
		if !found {
			t.Fatal("consumer specification not reused")
		}
	}
	for _, descriptor := range approvalconsumer.BillingCommands() {
		for _, want := range []string{"$JS.API.CONSUMER.INFO." + platformStream + "." + descriptor.Durable, "$JS.API.CONSUMER.CREATE." + platformStream + "." + descriptor.Durable + "." + approvalconsumer.AuthorizationSubject} {
			found := false
			for _, subject := range operator.Publish {
				if subject == want {
					if found {
						t.Fatal("duplicate governed consumer permission")
					}
					found = true
				}
			}
			if !found {
				t.Fatal("exact governed consumer permission missing")
			}
		}
	}
	if _, err := RenderNATSAuthorization(current, current); err != ErrNATSResources {
		t.Fatal("duplicate overlap identities accepted")
	}
}

func TestNATSPublicIdentityClosedSchema(t *testing.T) {
	valid, keys := natsPublicFixture(t)
	var decoded map[string]any
	json.Unmarshal(valid, &decoded)
	for _, kind := range []string{"version", "purpose", "missing", "extra", "duplicate-public", "non-user", "unknown-field", "wrong-type"} {
		b, _ := json.Marshal(decoded)
		var object map[string]any
		json.Unmarshal(b, &object)
		mapped := object["public_keys"].(map[string]any)
		switch kind {
		case "version":
			object["version"] = 2
		case "purpose":
			object["purpose"] = "other"
		case "missing":
			delete(mapped, "event-resource-operator")
		case "extra":
			mapped["extra"] = keys["event-resource-operator"]
		case "duplicate-public":
			mapped["event-resource-operator"] = keys["account-outbox-relay"]
		case "non-user":
			pair, _ := nkeys.CreateOperator()
			public, _ := pair.PublicKey()
			pair.Wipe()
			mapped["event-resource-operator"] = public
		case "unknown-field":
			object["permissions"] = map[string]any{}
		case "wrong-type":
			mapped["event-resource-operator"] = nil
		}
		bad, _ := json.Marshal(object)
		if _, err := parseNATSPublicKeys(bad); err != ErrNATSResources {
			t.Fatal("invalid schema:", kind)
		}
	}
	duplicateRole := bytes.Replace(valid, []byte(`"public_keys":{`), []byte(`"public_keys":{"event-resource-operator":"`+keys["event-resource-operator"]+`",`), 1)
	for _, bad := range [][]byte{nil, []byte("null"), append(append([]byte{}, valid...), []byte("{}")...), bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), duplicateRole, bytes.Repeat([]byte(" "), (32<<10)+1)} {
		if _, err := parseNATSPublicKeys(bad); err != ErrNATSResources {
			t.Fatal("unclosed/duplicate JSON accepted")
		}
		if _, err := RenderNATSAuthorization(valid, bad); len(bad) > 0 && err != ErrNATSResources {
			t.Fatal("invalid previous artifact accepted")
		}
	}
}
