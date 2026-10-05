package account

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
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

func NewHTTPGeneratedSMSProvider(config HTTPGeneratedSMSConfig) (*HTTPGeneratedSMSProvider, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && !(config.AllowInsecure && parsed.Scheme == "http")) {
		return nil, errors.New("SMS_HTTP_ENDPOINT must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	if len(config.VerificationKey) < 32 {
		return nil, errors.New("SMS verification key must contain at least 32 bytes")
	}
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPGeneratedSMSProvider{endpoint: parsed, bearerToken: strings.TrimSpace(config.BearerToken), verificationKey: append([]byte(nil), config.VerificationKey...), client: client}, nil
}

func (p *HTTPGeneratedSMSProvider) SendCode(ctx context.Context, mobile string, purpose SMSPurpose, challengeID uuid.UUID) (SMSDelivery, error) {
	random, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return SMSDelivery{}, err
	}
	code := fmt.Sprintf("%06d", random.Int64())
	body, _ := json.Marshal(map[string]string{"mobile": mobile, "purpose": string(purpose), "code": code, "challenge_id": challengeID.String()})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint.String(), bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if p.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+p.bearerToken)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return SMSDelivery{}, ErrSMSUnavailable
	}
	defer response.Body.Close()
	var result struct {
		MessageID string `json:"message_id"`
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if readErr != nil || len(data) > 64<<10 || response.StatusCode < 200 || response.StatusCode >= 300 || json.Unmarshal(data, &result) != nil || strings.TrimSpace(result.MessageID) == "" || len(result.MessageID) > 128 {
		// The delivery protocol provides no authoritative non-acceptance proof.
		// Unknown delivery must continue counting towards the existing cooldown.
		return SMSDelivery{}, ErrSMSUnavailable
	}
	return SMSDelivery{MessageID: result.MessageID, VerificationData: p.digest(challengeID, code)}, nil
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
