package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

var (
	ErrRegistrationBonusEligibilityNotReady    = errors.New("registration bonus eligibility backfill is not ready")
	ErrRegistrationBonusEligibilityUnavailable = errors.New("registration bonus eligibility authority is unavailable")
)

type RegistrationBonusEligibilityVerifier interface {
	Readiness(context.Context) (RegistrationBonusEligibilityReadiness, error)
	RequireReady(context.Context) error
}

type RegistrationBonusEligibilityReadiness struct {
	Status                         string     `json:"status"`
	FingerprintKeyID               string     `json:"fingerprint_key_id,omitempty"`
	RecoverableRegistrationCount   int64      `json:"recoverable_registration_count"`
	UnrecoverableRegistrationCount int64      `json:"unrecoverable_registration_count"`
	CompletedAt                    *time.Time `json:"completed_at,omitempty"`
}

type HTTPRegistrationBonusEligibilityVerifier struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPRegistrationBonusEligibilityVerifier(rawURL, serviceToken string) (*HTTPRegistrationBonusEligibilityVerifier, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("registration bonus eligibility verifier requires an absolute Account URL and service token")
	}
	return &HTTPRegistrationBonusEligibilityVerifier{baseURL: parsed, serviceToken: strings.TrimSpace(serviceToken), client: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (v *HTTPRegistrationBonusEligibilityVerifier) RequireReady(ctx context.Context) error {
	readiness, err := v.Readiness(ctx)
	if err != nil {
		return err
	}
	if readiness.Status != "READY" || readiness.UnrecoverableRegistrationCount != 0 {
		return ErrRegistrationBonusEligibilityNotReady
	}
	return nil
}

func (v *HTTPRegistrationBonusEligibilityVerifier) Readiness(ctx context.Context) (RegistrationBonusEligibilityReadiness, error) {
	endpoint := *v.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/registration-bonus-eligibility/readiness"
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err := serviceauth.SetAuthorization(request, v.serviceToken); err != nil {
		return RegistrationBonusEligibilityReadiness{}, fmt.Errorf("%w: authorize readiness request: %v", ErrRegistrationBonusEligibilityUnavailable, err)
	}
	response, err := v.client.Do(request)
	if err != nil {
		return RegistrationBonusEligibilityReadiness{}, fmt.Errorf("%w: readiness request: %v", ErrRegistrationBonusEligibilityUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return RegistrationBonusEligibilityReadiness{}, ErrRegistrationBonusEligibilityUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return RegistrationBonusEligibilityReadiness{}, ErrRegistrationBonusEligibilityUnavailable
	}
	// Pointer counts distinguish actual zero from missing or null authority facts.
	var wire struct {
		Status                         string     `json:"status"`
		FingerprintKeyID               string     `json:"fingerprint_key_id"`
		RecoverableRegistrationCount   *int64     `json:"recoverable_registration_count"`
		UnrecoverableRegistrationCount *int64     `json:"unrecoverable_registration_count"`
		CompletedAt                    *time.Time `json:"completed_at"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&wire)
	var trailing any
	trailingErr := decoder.Decode(&trailing)
	if decodeErr != nil || !errors.Is(trailingErr, io.EOF) || wire.RecoverableRegistrationCount == nil || wire.UnrecoverableRegistrationCount == nil || *wire.RecoverableRegistrationCount < 0 || *wire.UnrecoverableRegistrationCount < 0 {
		return RegistrationBonusEligibilityReadiness{}, ErrRegistrationBonusEligibilityUnavailable
	}
	switch wire.Status {
	case "PENDING":
		if wire.FingerprintKeyID != "" || wire.CompletedAt != nil {
			return RegistrationBonusEligibilityReadiness{}, ErrRegistrationBonusEligibilityUnavailable
		}
	case "READY", "BLOCKED":
		if !registrationBonusTextLength(wire.FingerprintKeyID, 1, 100) || wire.CompletedAt == nil || (wire.Status == "READY") != (*wire.UnrecoverableRegistrationCount == 0) {
			return RegistrationBonusEligibilityReadiness{}, ErrRegistrationBonusEligibilityUnavailable
		}
	default:
		return RegistrationBonusEligibilityReadiness{}, ErrRegistrationBonusEligibilityUnavailable
	}
	readiness := RegistrationBonusEligibilityReadiness{Status: wire.Status, FingerprintKeyID: wire.FingerprintKeyID, RecoverableRegistrationCount: *wire.RecoverableRegistrationCount, UnrecoverableRegistrationCount: *wire.UnrecoverableRegistrationCount, CompletedAt: wire.CompletedAt}
	return readiness, nil
}
