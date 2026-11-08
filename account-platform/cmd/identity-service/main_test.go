package main

import "testing"

func TestProductionIdentityRequiresTokenAdmissionConfiguration(t *testing.T) {
	for _, raw := range []string{"", " \t\n"} {
		if value, err := configuredAuthRiskRedisURL(true, raw); value != "" || err == nil || err.Error() != "IDENTITY_AUTH_RISK_CONFIGURATION_REQUIRED" {
			t.Fatal("production identity must not become ready with the public token gate absent")
		}
	}
	const address = "rediss://risk-redis.example.test:6379/0"
	if value, err := configuredAuthRiskRedisURL(true, " "+address+" "); err != nil || value != address {
		t.Fatal("configured production gate must retain its connection target")
	}
}

func TestDevelopmentIdentityOptionalAdmissionRemainsAvailable(t *testing.T) {
	if value, err := configuredAuthRiskRedisURL(false, " "); err != nil || value != "" {
		t.Fatal("optional development admission changed")
	}
}
