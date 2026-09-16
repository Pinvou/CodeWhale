package main

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestApprovalPaginationEnvironmentFailsClosed(t *testing.T) {
	get := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	if _, configured, err := approvalPaginationConfigFromEnv(get(nil)); err != nil || configured {
		t.Fatalf("default must remain hard-disabled: configured=%v err=%v", configured, err)
	}
	for _, values := range []map[string]string{
		{"APPROVAL_PAGINATION_ENABLED": "TRUE"},
		{"APPROVAL_PAGINATION_CURRENT_KEY_B64": "not-base64"},
		{"APPROVAL_PAGINATION_PREVIOUS_RETAIN_UNTIL": "tomorrow"},
		{"APPROVAL_PAGINATION_MAX_STORAGE_BYTES": "-1"},
	} {
		if _, _, err := approvalPaginationConfigFromEnv(get(values)); err == nil {
			t.Fatalf("invalid settings accepted: %v", values)
		}
	}
	stopped, configured, err := approvalPaginationConfigFromEnv(get(map[string]string{
		"APPROVAL_PAGINATION_ENABLED": "true", "APPROVAL_PAGINATION_HARD_DISABLED": "true",
		"APPROVAL_PAGINATION_CURRENT_KEY_B64": "corrupt-secret", "APPROVAL_PAGINATION_PREVIOUS_RETAIN_UNTIL": "corrupt",
	}))
	if err != nil || !configured || !stopped.HardDisabled || stopped.Enabled || len(stopped.CurrentKey) != 0 {
		t.Fatalf("emergency hard-disable failed closed: %+v configured=%v err=%v", stopped, configured, err)
	}
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	until := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	config, configured, err := approvalPaginationConfigFromEnv(get(map[string]string{
		"APPROVAL_PAGINATION_ENABLED": "true", "APPROVAL_PAGINATION_CURRENT_KID": "current",
		"APPROVAL_PAGINATION_CURRENT_KEY_B64": secret, "APPROVAL_PAGINATION_PREVIOUS_KID": "previous",
		"APPROVAL_PAGINATION_PREVIOUS_KEY_B64": secret, "APPROVAL_PAGINATION_PREVIOUS_RETAIN_UNTIL": until,
	}))
	if err != nil || !configured || !config.Enabled || len(config.CurrentKey) != 32 || len(config.PreviousKey) != 32 || config.PreviousRetainUntil.IsZero() {
		t.Fatalf("valid rotation rejected: configured=%v enabled=%v err=%v", configured, config.Enabled, err)
	}
}
