package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
)

// loadSystemProbes preserves the original configured probe kinds and URL
// boundary while using the current startup configuration error contract.
func loadSystemProbes(raw string) ([]portal.SystemProbe, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var configured []struct {
		Key              string `json:"key"`
		Kind             string `json:"kind"`
		URL              string `json:"url"`
		JSONBooleanField string `json:"json_boolean_field"`
	}
	if err := json.Unmarshal([]byte(raw), &configured); err != nil {
		return nil, errors.New("SYSTEM_PROBES_JSON must be a JSON array")
	}
	seen := map[string]struct{}{}
	result := make([]portal.SystemProbe, 0, len(configured))
	for _, item := range configured {
		item.Key = strings.TrimSpace(item.Key)
		item.Kind = strings.ToLower(strings.TrimSpace(item.Kind))
		if item.Key == "" || (item.Kind != "http" && item.Kind != "relay" && item.Kind != "local") {
			return nil, errors.New("SYSTEM_PROBES_JSON contains an invalid key or kind")
		}
		if _, duplicate := seen[item.Key]; duplicate {
			return nil, errors.New("SYSTEM_PROBES_JSON contains duplicate key")
		}
		seen[item.Key] = struct{}{}
		var target *url.URL
		if item.Kind != "local" {
			parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(item.URL), "/"))
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
				return nil, errors.New("SYSTEM_PROBES_JSON url must be an absolute URL without credentials or fragment")
			}
			target = parsed
		}
		result = append(result, portal.SystemProbe{Key: item.Key, Kind: item.Kind, Target: target, JSONBooleanField: strings.TrimSpace(item.JSONBooleanField)})
	}
	return result, nil
}
