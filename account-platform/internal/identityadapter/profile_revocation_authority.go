package identityadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/profilerevocation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

type HTTPProfileRevocationAuthority struct {
	endpoint   string
	credential string
	client     *http.Client
}

func NewHTTPProfileRevocationAuthority(rawURL, credential, caFile string) (*HTTPProfileRevocationAuthority, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" || parsed.Path != "" && parsed.Path != "/" || parsed.Scheme != "https" && parsed.Scheme != "http" || strings.TrimSpace(credential) == "" {
		return nil, profilerevocation.ErrUnconfirmed
	}
	client, err := serviceauth.NewWorkloadHTTPClient(caFile)
	if err != nil {
		return nil, err
	}
	return &HTTPProfileRevocationAuthority{endpoint: strings.TrimRight(parsed.String(), "/") + "/internal/v1/profile-revocation-receipts", credential: credential, client: client}, nil
}

func (v *HTTPProfileRevocationAuthority) ClaimProfileRevocation(ctx context.Context, command profilerevocation.Command) (profilerevocation.Result, error) {
	var result profilerevocation.Result
	err := v.request(ctx, "/claim", command, &result)
	if err == nil && result.State != "DISPATCH" && result.State != "CONFIRMED" {
		err = profilerevocation.ErrUnconfirmed
	}
	return result, err
}

func (v *HTTPProfileRevocationAuthority) ConfirmProfileRevocation(ctx context.Context, command profilerevocation.Command) error {
	return v.request(ctx, "/confirm", command, nil)
}

func (v *HTTPProfileRevocationAuthority) request(ctx context.Context, path string, command profilerevocation.Command, result *profilerevocation.Result) error {
	raw, _ := json.Marshal(command)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return profilerevocation.ErrUnconfirmed
	}
	request.Header.Set("Content-Type", "application/json")
	if err = serviceauth.SetAuthorization(request, v.credential); err != nil {
		return profilerevocation.ErrUnconfirmed
	}
	response, err := v.client.Do(request)
	if err != nil {
		return profilerevocation.ErrUnconfirmed
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return profilerevocation.ErrConflict
	}
	if result == nil {
		if response.StatusCode == http.StatusNoContent {
			return nil
		}
		return profilerevocation.ErrUnconfirmed
	}
	if response.StatusCode != http.StatusOK {
		return profilerevocation.ErrUnconfirmed
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	decoder.DisallowUnknownFields()
	if decoder.Decode(result) != nil || decoder.Decode(new(any)) != io.EOF {
		return profilerevocation.ErrUnconfirmed
	}
	return nil
}
