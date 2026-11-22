package main

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"testing"
)

func TestPasswordReferenceCapacityConfiguration(t *testing.T) {
	for _, raw := range []string{"0", "-1", "65537", "unbounded", "9999999999999999999999999"} {
		if _, err := configuredPasswordReferenceCapacity(raw); err == nil {
			t.Fatal("unsafe capacity accepted", raw)
		}
	}
	for raw, want := range map[string]int{"": identityadapter.DefaultPasswordReferenceCapacity, " 16 ": 16, "65536": 65536} {
		got, err := configuredPasswordReferenceCapacity(raw)
		if err != nil || got != want {
			t.Fatal("capacity configuration not preserved", raw, got, err)
		}
	}
}
