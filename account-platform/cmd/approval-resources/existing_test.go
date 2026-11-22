package main

import "testing"

func TestExistingBrokerArgumentsRejectBeforeCredentials(t *testing.T) {
	for _, args := range [][]string{
		{"-expected-version", "-1"}, {"-expected-version", "0", "-action", "upgrade-token-consumers"}, {"-expected-version", "1", "-action", "drain"},
		{"-expected-version", "1", "-action", "status", "-resume"},
		{"-expected-version", "1", "-action", "prepare-token-upgrade"},
		{"-expected-version", "1", "-action", "prepare-token-upgrade", "-resume", "-operation-id", "replacement"},
		{"-expected-version", "1", "-action", "inspect-token-upgrade", "-resume"},
		{"-expected-version", "1", "-action", "upgrade-token-consumers", "-operation-id", "replacement"},
		{"-expected-version", "3", "-action", "prepare-refund-recovery", "-resume"},
		{"-expected-version", "3", "-action", "upgrade-refund-recovery", "-resume"},
		{"-expected-version", "3", "-action", "check-refund-recovery", "-resume"},
	} {
		_, err := runExistingBroker(args, func(key string) string {
			if key == "ACCOUNT_PLATFORM_PROFILE" {
				return "models"
			}
			t.Fatalf("invalid operation read credential/configuration %s", key)
			return ""
		})
		if err == nil {
			t.Fatal("invalid existing-broker command accepted")
		}
	}
}
