package main

import (
	"encoding/base64"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"strings"
)

// The deployment key is mandatory. No generated or built-in fallback may make
// encrypted jobs unreadable after a restart; the owner takes its own key copy.
func configuredAuditExports(repository *account.PostgresRepository, environment, encodedKey string) (*account.AuditExportService, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(encodedKey))
	defer clear(key)
	if err != nil || len(key) != 32 {
		return nil, errors.New("audit export key configuration is invalid")
	}
	return account.NewAuditExportService(repository, key, environment)
}
