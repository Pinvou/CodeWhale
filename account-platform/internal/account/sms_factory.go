package account

import (
	"errors"
	"strings"
)

type SMSProviderConfig struct {
	Mode          string
	Aliyun        AliyunSMSConfig
	HTTPGenerated HTTPGeneratedSMSConfig
}

func NewSMSProvider(config SMSProviderConfig) (SMSProvider, error) {
	switch strings.ToLower(strings.TrimSpace(config.Mode)) {
	case "aliyun-managed":
		return NewAliyunSMSProvider(config.Aliyun)
	case "http-generated":
		return NewHTTPGeneratedSMSProvider(config.HTTPGenerated)
	default:
		return nil, errors.New("SMS_PROVIDER must be aliyun-managed or http-generated")
	}
}
