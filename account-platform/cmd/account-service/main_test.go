package main

import (
	"testing"
	"time"
)

func TestWalletSLAConfigurationRejectsOverflowBeforeMultiplication(t *testing.T) {
	for _, raw := range []string{"307445750", "1441", "0", "-1", "not-an-integer"} {
		if _, err := configuredWalletSLA(raw); err == nil {
			t.Fatalf("invalid duration accepted: %s", raw)
		}
	}
	for raw, expected := range map[string]time.Duration{"": 15 * time.Minute, "1": time.Minute, "1440": 24 * time.Hour, " 15 ": 15 * time.Minute} {
		actual, err := configuredWalletSLA(raw)
		if err != nil || actual != expected {
			t.Fatalf("valid duration rejected: actual=%v expected=%v err=%v", actual, expected, err)
		}
	}
}
