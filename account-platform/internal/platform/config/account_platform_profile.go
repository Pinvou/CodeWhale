package config

import (
	"errors"
	"strings"
)

type AccountPlatformProfile string

const (
	AccountPlatformProfilePersonal AccountPlatformProfile = "personal"
	AccountPlatformProfileModels   AccountPlatformProfile = "models"
)

// An omitted selector preserves the existing models deployment. Missing model
// dependencies never select the personal facade implicitly.
func ParseAccountPlatformProfile(value string) (AccountPlatformProfile, error) {
	switch strings.TrimSpace(value) {
	case "", string(AccountPlatformProfileModels):
		return AccountPlatformProfileModels, nil
	case string(AccountPlatformProfilePersonal):
		return AccountPlatformProfilePersonal, nil
	default:
		return "", errors.New("ACCOUNT_PLATFORM_PROFILE must be personal or models")
	}
}
