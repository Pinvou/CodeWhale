package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
)

// Authority responses must contain one complete bounded JSON value. Unknown
// object fields remain compatible; null and partial/trailing values are not ACKs.
func decodeAuthorityResponse(reader io.Reader, output any) error {
	raw, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("invalid authority response")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(output); err != nil {
		return errors.New("invalid authority response")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("invalid authority response")
	}
	return nil
}

func authorityEndpoint(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("authority endpoint must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	return parsed, nil
}
