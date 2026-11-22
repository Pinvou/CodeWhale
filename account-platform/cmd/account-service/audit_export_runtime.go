package main

import (
	"encoding/base64"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"strings"
)

// Missing configuration leaves audit export disabled for existing deployments.
// An explicitly supplied key must be valid; no generated or built-in fallback
// may make existing encrypted jobs unreadable after restart.
func configuredAuditExports(repository *account.PostgresRepository, environment, encodedKey string) (*account.AuditExportService, error) {
	if strings.TrimSpace(encodedKey) == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(encodedKey))
	defer clear(key)
	if err != nil || len(key) != 32 {
		return nil, errors.New("audit export key configuration is invalid")
	}
	return account.NewAuditExportService(repository, key, environment)
}
