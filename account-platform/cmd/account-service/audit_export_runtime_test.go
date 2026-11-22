package main

import (
	"encoding/base64"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"strings"
	"testing"
)

func TestAuditExportRuntimeConfigurationFailClosed(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	for _, tc := range []struct {
		name, key, environment string
		repository             *account.PostgresRepository
		allowed                bool
	}{
		{"valid", valid, "test", &account.PostgresRepository{}, true},
		{"trim-valid", "  " + valid + "  ", "test", &account.PostgresRepository{}, true},
		{"malformed", "not-base64", "test", &account.PostgresRepository{}, false},
		{"short", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 31))), "test", &account.PostgresRepository{}, false},
		{"long", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 33))), "test", &account.PostgresRepository{}, false},
		{"missing-repository", valid, "test", nil, false},
		{"missing-environment", valid, "", &account.PostgresRepository{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := configuredAuditExports(tc.repository, tc.environment, tc.key)
			if tc.allowed {
				if err != nil || s == nil {
					t.Fatal("valid owned key rejected")
				}
			} else if err == nil || s != nil {
				t.Fatal("invalid key/config accepted")
			}
			if err != nil && strings.Contains(err.Error(), valid) {
				t.Fatal("key leaked in error")
			}
		})
	}
}

// Missing optional export configuration must not require a database owner,
// allocate a key or construct a worker. Explicit invalid keys remain fatal.
func TestAuditExportMissingConfigurationLeavesFeatureDisabled(t *testing.T) {
	for _, environment := range []string{"production", "test", ""} {
		for _, key := range []string{"", "  "} {
			service, err := configuredAuditExports(nil, environment, key)
			if err != nil || service != nil {
				t.Fatal("absent audit export configuration enabled a worker or blocked existing startup")
			}
		}
	}
}
