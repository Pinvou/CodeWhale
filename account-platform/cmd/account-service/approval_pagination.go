package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
)

// Pagination remains hard-disabled unless an operator explicitly supplies a
// valid signing configuration. The DB gate is independent and starts closed.
func approvalPaginationConfigFromEnv(getenv func(string) string) (account.ApprovalPaginationConfig, bool, error) {
	var config account.ApprovalPaginationConfig
	enabled, err := parseApprovalPaginationBool(getenv("APPROVAL_PAGINATION_ENABLED"))
	if err != nil {
		return config, false, err
	}
	hardDisabled, err := parseApprovalPaginationBool(getenv("APPROVAL_PAGINATION_HARD_DISABLED"))
	if err != nil {
		return config, false, err
	}
	if hardDisabled {
		// Emergency containment must work even when the old signing material
		// or rotation metadata is corrupt. No cursor is read or signed.
		return account.ApprovalPaginationConfig{HardDisabled: true}, true, nil
	}
	config.Enabled = enabled
	config.CurrentKeyID = strings.TrimSpace(getenv("APPROVAL_PAGINATION_CURRENT_KID"))
	config.PreviousKeyID = strings.TrimSpace(getenv("APPROVAL_PAGINATION_PREVIOUS_KID"))
	currentEncoded := strings.TrimSpace(getenv("APPROVAL_PAGINATION_CURRENT_KEY_B64"))
	previousEncoded := strings.TrimSpace(getenv("APPROVAL_PAGINATION_PREVIOUS_KEY_B64"))
	previousUntil := strings.TrimSpace(getenv("APPROVAL_PAGINATION_PREVIOUS_RETAIN_UNTIL"))
	maximum := strings.TrimSpace(getenv("APPROVAL_PAGINATION_MAX_STORAGE_BYTES"))
	configured := enabled || hardDisabled || config.CurrentKeyID != "" || config.PreviousKeyID != "" || currentEncoded != "" || previousEncoded != "" || previousUntil != "" || maximum != ""
	if !configured {
		return config, false, nil
	}
	if currentEncoded != "" {
		config.CurrentKey, err = base64.StdEncoding.Strict().DecodeString(currentEncoded)
		if err != nil {
			return config, true, errors.New("invalid approval pagination current key encoding")
		}
	}
	if previousEncoded != "" {
		config.PreviousKey, err = base64.StdEncoding.Strict().DecodeString(previousEncoded)
		if err != nil {
			return config, true, errors.New("invalid approval pagination previous key encoding")
		}
	}
	if previousUntil != "" {
		config.PreviousRetainUntil, err = time.Parse(time.RFC3339, previousUntil)
		if err != nil {
			return config, true, errors.New("invalid approval pagination previous retention timestamp")
		}
	}
	if maximum != "" {
		config.MaximumStorageBytes, err = strconv.ParseInt(maximum, 10, 64)
		if err != nil || config.MaximumStorageBytes <= 0 {
			return config, true, errors.New("invalid approval pagination storage budget")
		}
	}
	return config, true, nil
}

func parseApprovalPaginationBool(value string) (bool, error) {
	switch strings.TrimSpace(value) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("approval pagination boolean must be true or false")
	}
}

func configureApprovalPagination(ctx context.Context, repository *account.PostgresRepository) error {
	config, configured, err := approvalPaginationConfigFromEnv(os.Getenv)
	if err != nil || !configured {
		return err
	}
	return repository.ConfigureApprovalPagination(ctx, config)
}
