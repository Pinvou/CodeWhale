package account

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

type HTTPGeneratedSMSConfig struct {
	Endpoint        string
	BearerToken     string
	VerificationKey []byte
	AllowInsecure   bool
}

type HTTPGeneratedSMSProvider struct {
	endpoint        *url.URL
	bearerToken     string
	verificationKey []byte
	client          *http.Client
}

func (*HTTPGeneratedSMSProvider) Descriptor() SMSProviderDescriptor {
	return SMSProviderDescriptor{ID: "http-generated", AdapterVersion: "v1"}
}

func (*HTTPGeneratedSMSProvider) DeliveryRetryPolicy() SMSDeliveryRetryPolicy {
	return SMSDeliveryIdempotent
}

func (p *HTTPGeneratedSMSProvider) PrepareVerification(challengeID uuid.UUID) ([]byte, error) {
	return p.digest(challengeID, p.codeForChallenge(challengeID)), nil
}

func NewHTTPGeneratedSMSProvider(config HTTPGeneratedSMSConfig) (*HTTPGeneratedSMSProvider, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && !(config.AllowInsecure && parsed.Scheme == "http")) {
		return nil, errors.New("SMS_HTTP_ENDPOINT must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	if len(config.VerificationKey) < 32 {
		return nil, errors.New("SMS verification key must contain at least 32 bytes")
	}
	return &HTTPGeneratedSMSProvider{endpoint: parsed, bearerToken: strings.TrimSpace(config.BearerToken), verificationKey: append([]byte(nil), config.VerificationKey...), client: observability.NewHTTPClient(10 * time.Second)}, nil
}

func (p *HTTPGeneratedSMSProvider) SendCode(ctx context.Context, mobile string, purpose SMSPurpose, challengeID uuid.UUID) (SMSDelivery, error) {
	code := p.codeForChallenge(challengeID)
	body, _ := json.Marshal(map[string]string{"mobile": mobile, "purpose": string(purpose), "code": code, "challenge_id": challengeID.String()})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Idempotency-Key", challengeID.String())
	if p.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+p.bearerToken)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return SMSDelivery{}, fmt.Errorf("send generated SMS: %w", err)
	}
	defer response.Body.Close()
	var result struct {
		MessageID string `json:"message_id"`
	}
	if response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout && response.StatusCode != http.StatusConflict && response.StatusCode != http.StatusTooManyRequests {
		return SMSDelivery{}, &SMSDeliveryRejectedError{Cause: fmt.Errorf("generated SMS adapter rejected the request with HTTP %d", response.StatusCode)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result) != nil || strings.TrimSpace(result.MessageID) == "" {
		return SMSDelivery{}, fmt.Errorf("generated SMS adapter returned HTTP %d or an invalid response", response.StatusCode)
	}
	return SMSDelivery{MessageID: result.MessageID, VerificationData: p.digest(challengeID, code)}, nil
}

func (p *HTTPGeneratedSMSProvider) codeForChallenge(challengeID uuid.UUID) string {
	mac := hmac.New(sha256.New, p.verificationKey)
	_, _ = mac.Write([]byte("sms-code-v1\x00"))
	_, _ = mac.Write([]byte(challengeID.String()))
	sum := mac.Sum(nil)
	value := uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
	return fmt.Sprintf("%06d", value%1000000)
}

func (p *HTTPGeneratedSMSProvider) VerifyCode(_ context.Context, _ string, challengeID uuid.UUID, verification SMSVerificationData, code string) (bool, error) {
	if verification.Provider != p.Descriptor() {
		return false, errors.New("SMS challenge belongs to a different provider adapter")
	}
	if len(verification.Opaque) != sha256.Size {
		return false, errors.New("generated SMS verification data is unavailable")
	}
	return hmac.Equal(verification.Opaque, p.digest(challengeID, strings.TrimSpace(code))), nil
}

func (p *HTTPGeneratedSMSProvider) digest(challengeID uuid.UUID, code string) []byte {
	mac := hmac.New(sha256.New, p.verificationKey)
	_, _ = mac.Write([]byte(challengeID.String()))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(code))
	return mac.Sum(nil)
}
