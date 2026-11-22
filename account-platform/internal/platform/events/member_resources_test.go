package events

import (
	"slices"
	"testing"
)

func TestMembershipConsumerFixedAuthority(t *testing.T) {
	want := eventConsumerConfig("billing-member-lifecycle-v1", "account.membership.changed.v1", true)
	if !consumerOwnedBy("billing-command-executor", want) {
		t.Fatal("the production member receiver cannot consume its provisioned resource")
	}
	for _, role := range []string{"account-outbox-relay", "token-command-executor", "event-resource-operator"} {
		if consumerOwnedBy(role, want) {
			t.Fatal("another workload acquired member consumption authority")
		}
	}
	for _, changed := range []string{"account.membership.changed.v2", "account.>", "account.role-binding.changed.v1"} {
		if consumerOwnedBy("billing-command-executor", eventConsumerConfig(want.Name, changed, true)) {
			t.Fatal("an unregistered membership subject acquired production authority")
		}
	}
	if consumerOwnedBy("billing-command-executor", eventConsumerConfig(want.Name, want.FilterSubject, false)) {
		t.Fatal("bounded delivery would discard unresolved member authority")
	}
	permissions := fixedNATSPermissions("billing-command-executor")
	if !slices.Contains(permissions.Publish, "$JS.API.CONSUMER.MSG.NEXT."+platformStream+"."+want.Name) ||
		!slices.Contains(permissions.Publish, "$JS.ACK."+platformStream+"."+want.Name+".>") {
		t.Fatal("the billing workload lacks its exact durable pull/ACK permissions")
	}
	if !ordinaryConsumerNames()[want.Name] || ordinaryConsumerNames()["foreign-member-consumer"] {
		t.Fatal("approval fresh-install validation lost the finite ordinary consumer boundary")
	}
}
