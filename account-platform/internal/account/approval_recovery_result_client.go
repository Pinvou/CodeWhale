package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

var ErrApprovalRecoveryResultUnavailable = errors.New("approval recovery result authority is unavailable")

type RecoveryResultObservation struct {
	State      string
	ObservedAt time.Time
}

type ApprovalRecoveryResultReader interface {
	ObserveRecoveryResult(context.Context, ApprovalRecoveryCandidate) (RecoveryResultObservation, error)
}

type ApprovalRecoveryResultClient struct {
	billingEndpoint *url.URL
	tokenEndpoint   *url.URL
	serviceToken    string
	httpClient      *http.Client
}

func NewApprovalRecoveryResultClient(billingBaseURL, tokenBaseURL, serviceToken string) (*ApprovalRecoveryResultClient, error) {
	billing, err := recoveryResultEndpoint(billingBaseURL)
	if err != nil {
		return nil, ErrApprovalRecoveryResultUnavailable
	}
	token, err := recoveryResultEndpoint(tokenBaseURL)
	if err != nil || strings.TrimSpace(serviceToken) == "" {
		return nil, ErrApprovalRecoveryResultUnavailable
	}
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ApprovalRecoveryResultClient{billingEndpoint: billing, tokenEndpoint: token, serviceToken: serviceToken, httpClient: client}, nil
}

func recoveryResultEndpoint(base string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, ErrApprovalRecoveryResultUnavailable
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/internal/v1/approval-recovery/results/"
	parsed.RawPath = ""
	return parsed, nil
}

type tokenRecoveryResultView struct {
	SchemaVersion      int       `json:"schema_version"`
	ApprovalRequestID  uuid.UUID `json:"approval_request_id"`
	CommandType        string    `json:"command_type"`
	CommandFingerprint string    `json:"command_fingerprint"`
	OriginalKeyDigest  string    `json:"original_key_digest"`
	State              string    `json:"state"`
	ObservedAt         time.Time `json:"observed_at"`
}

type billingRecoveryResultView struct {
	SchemaVersion     int       `json:"schema_version"`
	ApprovalRequestID uuid.UUID `json:"approval_request_id"`
	OriginalEventID   uuid.UUID `json:"original_event_id"`
	ApprovalEpoch     int64     `json:"approval_epoch"`
	CommandType       string    `json:"command_type"`
	State             string    `json:"state"`
	ObservedAt        time.Time `json:"observed_at"`
}

func (c *ApprovalRecoveryResultClient) ObserveRecoveryResult(ctx context.Context, candidate ApprovalRecoveryCandidate) (RecoveryResultObservation, error) {
	if c == nil || c.httpClient == nil || !validRecoveryInspectionCandidate(candidate) || candidate.OriginalKey == "" || candidate.ApprovalEpoch < 1 {
		return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
	}
	descriptor, ok := approvalconsumer.LookupDurable(candidate.Durable)
	if !ok {
		return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
	}
	base := c.billingEndpoint
	if descriptor.Workload == approvalconsumer.TokenWorkload {
		base = c.tokenEndpoint
	}
	if base == nil {
		return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
	}
	endpoint := *base
	endpoint.Path += candidate.RequestID.String()
	query := endpoint.Query()
	query.Set("command_type", candidate.CommandType)
	if descriptor.Workload == approvalconsumer.TokenWorkload {
		query.Set("command_fingerprint", candidate.CommandFingerprint)
		digest := sha256.Sum256([]byte(candidate.OriginalKey))
		query.Set("original_key_digest", hex.EncodeToString(digest[:]))
	} else {
		query.Set("original_event_id", candidate.OriginalEventID.String())
		query.Set("approval_epoch", strconv.FormatInt(candidate.ApprovalEpoch, 10))
	}
	endpoint.RawQuery = query.Encode()
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(bounded, http.MethodGet, endpoint.String(), nil)
	if err != nil || serviceauth.SetAuthorization(request, c.serviceToken) != nil {
		return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
	}
	request.Header.Set("Cache-Control", "no-cache")
	startedAt := time.Now().UTC()
	response, err := c.httpClient.Do(request)
	if err != nil {
		return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
	}
	defer response.Body.Close()
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	cacheControl := response.Header.Values("Cache-Control")
	if response.StatusCode != http.StatusOK || mediaErr != nil || mediaType != "application/json" || len(cacheControl) != 1 || cacheControl[0] != "no-store" {
		return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
	}
	const maximumResultBytes = 16 << 10
	raw, err := io.ReadAll(io.LimitReader(response.Body, maximumResultBytes+1))
	if err != nil || len(raw) > maximumResultBytes || !utf8.Valid(raw) || !uniqueRecoveryJSONKeys(raw) {
		return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var state string
	var observedAt time.Time
	if descriptor.Workload == approvalconsumer.TokenWorkload {
		var view tokenRecoveryResultView
		if decoder.Decode(&view) != nil || decoder.Decode(new(any)) != io.EOF || view.SchemaVersion != 1 ||
			view.ApprovalRequestID != candidate.RequestID || view.CommandType != candidate.CommandType ||
			view.CommandFingerprint != candidate.CommandFingerprint || view.OriginalKeyDigest != query.Get("original_key_digest") {
			return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
		}
		state, observedAt = view.State, view.ObservedAt
		if state != "UNCONFIRMED" && state != "PENDING" && state != "SUCCEEDED" && state != "REJECTED" && state != "FAILED" && state != "UNKNOWN" {
			return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
		}
	} else {
		var view billingRecoveryResultView
		if decoder.Decode(&view) != nil || decoder.Decode(new(any)) != io.EOF || view.SchemaVersion != 1 ||
			view.ApprovalRequestID != candidate.RequestID || view.OriginalEventID != candidate.OriginalEventID ||
			view.ApprovalEpoch != candidate.ApprovalEpoch || view.CommandType != candidate.CommandType {
			return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
		}
		state, observedAt = view.State, view.ObservedAt
		if state != "UNCONFIRMED" && state != "SUCCEEDED" {
			return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
		}
	}
	if observedAt.Before(startedAt.Add(-30*time.Second)) || observedAt.After(time.Now().UTC().Add(30*time.Second)) {
		return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
	}
	return RecoveryResultObservation{State: state, ObservedAt: observedAt}, nil
}
