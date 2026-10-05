package account

import (
	"errors"
	"strings"
)

type SMSProviderConfig struct {
	Mode string
	// Retain the published operator-owned delivery/verification adapter.
	Provider      string
	Endpoint      string
	ServiceToken  string
	AllowInsecure bool
	Aliyun        AliyunSMSConfig
	HTTPGenerated HTTPGeneratedSMSConfig
}

func NewSMSProvider(config SMSProviderConfig) (SMSProvider, error) {
	mode := strings.ToLower(strings.TrimSpace(config.Mode))
	legacy := strings.ToLower(strings.TrimSpace(config.Provider))
	if mode == "" {
		mode = legacy
	} else if legacy != "" && legacy != mode {
		return nil, errors.New("SMS provider configuration is ambiguous")
	}
	switch mode {
	case "aliyun", "aliyun-managed":
		return NewAliyunSMSProvider(config.Aliyun)
	case "http":
		return newHTTPManagedSMSProvider(config)
	case "http-generated":
		return NewHTTPGeneratedSMSProvider(config.HTTPGenerated)
	default:
		return nil, errors.New("SMS_PROVIDER must be http, aliyun, aliyun-managed, or http-generated")
	}
}
