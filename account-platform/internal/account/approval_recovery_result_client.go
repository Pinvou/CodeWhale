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
	State            string
	ObservedAt       time.Time
	CommandReference *uuid.UUID
	DomainResult     json.RawMessage
	LedgerReferences json.RawMessage
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

func NewApprovalRecoveryResultClient(billingBaseURL, serviceToken string) (*ApprovalRecoveryResultClient, error) {
	billing, err := recoveryResultEndpoint(billingBaseURL)
	if err != nil {
		return nil, ErrApprovalRecoveryResultUnavailable
	}
	if strings.TrimSpace(serviceToken) == "" {
		return nil, ErrApprovalRecoveryResultUnavailable
	}
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ApprovalRecoveryResultClient{billingEndpoint: billing, serviceToken: serviceToken, httpClient: client}, nil
}

func recoveryResultEndpoint(base string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, ErrApprovalRecoveryResultUnavailable
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/internal/v1/approval-recovery/results/"
	parsed.RawPath = ""
	return parsed, nil
}

type tokenRecoveryResultView struct {
	SchemaVersion      int        `json:"schema_version"`
	ApprovalRequestID  uuid.UUID  `json:"approval_request_id"`
	CommandType        string     `json:"command_type"`
	CommandFingerprint string     `json:"command_fingerprint"`
	OriginalKeyDigest  string     `json:"original_key_digest"`
	State              string     `json:"state"`
	CommandReference   *uuid.UUID `json:"command_reference,omitempty"`
	ObservedAt         time.Time  `json:"observed_at"`
}

type billingRecoveryResultView struct {
	SchemaVersion     int             `json:"schema_version"`
	ApprovalRequestID uuid.UUID       `json:"approval_request_id"`
	OriginalEventID   uuid.UUID       `json:"original_event_id"`
	ApprovalEpoch     int64           `json:"approval_epoch"`
	CommandType       string          `json:"command_type"`
	State             string          `json:"state"`
	DomainResult      json.RawMessage `json:"domain_result,omitempty"`
	LedgerReferences  json.RawMessage `json:"ledger_references,omitempty"`
	ObservedAt        time.Time       `json:"observed_at"`
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
	var reference *uuid.UUID
	var domainResult, ledgerReferences json.RawMessage
	if descriptor.Workload == approvalconsumer.TokenWorkload {
		var view tokenRecoveryResultView
		if decoder.Decode(&view) != nil || decoder.Decode(new(any)) != io.EOF || view.SchemaVersion != 1 ||
			view.ApprovalRequestID != candidate.RequestID || view.CommandType != candidate.CommandType ||
			view.CommandFingerprint != candidate.CommandFingerprint || view.OriginalKeyDigest != query.Get("original_key_digest") {
			return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
		}
		state, observedAt = view.State, view.ObservedAt
		reference = view.CommandReference
		if (state == "SUCCEEDED" && (reference == nil || *reference == uuid.Nil)) || (state != "SUCCEEDED" && reference != nil) {
			return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
		}
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
		if state == "SUCCEEDED" {
			if !validBillingRecoveryPayload(candidate.CommandType, view.DomainResult, view.LedgerReferences) {
				return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
			}
			domainResult, ledgerReferences = view.DomainResult, view.LedgerReferences
		} else if len(view.DomainResult) != 0 || len(view.LedgerReferences) != 0 {
			return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
		}
	}
	if observedAt.Before(startedAt.Add(-30*time.Second)) || observedAt.After(time.Now().UTC().Add(30*time.Second)) {
		return RecoveryResultObservation{}, ErrApprovalRecoveryResultUnavailable
	}
	return RecoveryResultObservation{State: state, ObservedAt: observedAt, CommandReference: reference,
		DomainResult: domainResult, LedgerReferences: ledgerReferences}, nil
}

func validBillingRecoveryPayload(command string, domainResult, ledgerReferences json.RawMessage) bool {
	if len(domainResult) == 0 || len(ledgerReferences) == 0 {
		return false
	}
	decode := func(raw json.RawMessage, target any) bool {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		return decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
	}
	switch command {
	case "WALLET_ADJUSTMENT":
		var result struct {
			AdjustmentID uuid.UUID `json:"adjustment_id"`
			WalletID     uuid.UUID `json:"wallet_id"`
			Status       string    `json:"status"`
			Version      int64     `json:"version"`
		}
		var ledger struct {
			JournalEntryID uuid.UUID `json:"journal_entry_id"`
		}
		return decode(domainResult, &result) && decode(ledgerReferences, &ledger) && result.AdjustmentID != uuid.Nil &&
			result.WalletID != uuid.Nil && result.Status == "POSTED" && result.Version > 0 && ledger.JournalEntryID != uuid.Nil
	case "PAYMENT_PROVIDER_RESUME":
		var result struct {
			ExecutionID uuid.UUID `json:"execution_id"`
			ProviderID  string    `json:"provider_id"`
			Status      string    `json:"status"`
		}
		var ledger map[string]any
		return decode(domainResult, &result) && decode(ledgerReferences, &ledger) && ledger != nil && len(ledger) == 0 &&
			result.ExecutionID != uuid.Nil && result.ProviderID != "" && result.Status == "RESUMED"
	case "LEDGER_REPAIR":
		var result struct {
			CaseID                 uuid.UUID `json:"case_id"`
			TargetJournalEntryID   uuid.UUID `json:"target_journal_entry_id"`
			TargetPostingID        uuid.UUID `json:"target_posting_id"`
			WrongLedgerAccountID   uuid.UUID `json:"wrong_ledger_account_id"`
			CorrectLedgerAccountID uuid.UUID `json:"correct_ledger_account_id"`
			EntryType              string    `json:"entry_type"`
			AmountMicrocredits     int64     `json:"amount_microcredits"`
			PostingSetDigest       string    `json:"posting_set_digest"`
			JournalEntryID         uuid.UUID `json:"journal_entry_id"`
			Status                 string    `json:"status"`
		}
		var ledger struct {
			JournalEntryID uuid.UUID `json:"journal_entry_id"`
		}
		return decode(domainResult, &result) && decode(ledgerReferences, &ledger) &&
			result.CaseID != uuid.Nil && result.TargetJournalEntryID != uuid.Nil && result.TargetPostingID != uuid.Nil &&
			result.WrongLedgerAccountID != uuid.Nil && result.CorrectLedgerAccountID != uuid.Nil &&
			result.WrongLedgerAccountID != result.CorrectLedgerAccountID && result.EntryType != "" &&
			result.AmountMicrocredits > 0 && lowerHexDigest(result.PostingSetDigest) &&
			result.JournalEntryID != uuid.Nil && result.Status == "SUCCEEDED" &&
			ledger.JournalEntryID == result.JournalEntryID
	case "ENTITLEMENT_COMPONENT_ADJUSTMENT":
		var result struct {
			CorrectionID       uuid.UUID `json:"correction_id"`
			PackageID          uuid.UUID `json:"package_id"`
			WalletID           uuid.UUID `json:"wallet_id"`
			SourceComponentID  uuid.UUID `json:"source_component_id"`
			SourceLotID        uuid.UUID `json:"source_lot_id"`
			TargetComponentID  uuid.UUID `json:"target_component_id"`
			TargetLotID        uuid.UUID `json:"target_lot_id"`
			SourceType         string    `json:"source_type"`
			TargetType         string    `json:"target_type"`
			AmountMicrocredits int64     `json:"amount_microcredits"`
			JournalEntryID     uuid.UUID `json:"journal_entry_id"`
			Status             string    `json:"status"`
		}
		var ledger struct {
			JournalEntryID uuid.UUID `json:"journal_entry_id"`
		}
		return decode(domainResult, &result) && decode(ledgerReferences, &ledger) && result.CorrectionID != uuid.Nil &&
			result.PackageID != uuid.Nil && result.WalletID != uuid.Nil && result.SourceComponentID != uuid.Nil &&
			result.SourceLotID != uuid.Nil && result.TargetComponentID != uuid.Nil && result.TargetLotID != uuid.Nil &&
			((result.SourceType == "BASE" && result.TargetType == "BONUS") ||
				(result.SourceType == "BONUS" && result.TargetType == "BASE")) &&
			result.AmountMicrocredits > 0 && result.JournalEntryID != uuid.Nil && result.Status == "SUCCEEDED" &&
			ledger.JournalEntryID == result.JournalEntryID
	case "PAYMENT_PRODUCT_RESUME":
		var result struct {
			ExecutionID uuid.UUID `json:"execution_id"`
			ProviderID  string    `json:"provider_id"`
			ProductID   string    `json:"product_id"`
			Status      string    `json:"status"`
		}
		var ledger map[string]any
		return decode(domainResult, &result) && decode(ledgerReferences, &ledger) && ledger != nil && len(ledger) == 0 &&
			result.ExecutionID != uuid.Nil && result.ProviderID != "" && result.ProductID != "" && result.Status == "RESUMED"

	case "RECEIVABLE_WRITE_OFF":
		var result struct {
			CaseID                  uuid.UUID `json:"case_id"`
			ReceivableID            uuid.UUID `json:"receivable_id"`
			WalletID                uuid.UUID `json:"wallet_id"`
			AmountMicrocredits      int64     `json:"amount_microcredits"`
			ReceivableVersionBefore int64     `json:"receivable_version_before"`
			ReceivableVersionAfter  int64     `json:"receivable_version_after"`
			StatusAfter             string    `json:"status_after"`
			FactsDigest             string    `json:"facts_digest"`
			JournalEntryID          uuid.UUID `json:"journal_entry_id"`
			AllocationID            uuid.UUID `json:"allocation_id"`
			Status                  string    `json:"status"`
		}
		var ledger struct {
			JournalEntryID uuid.UUID `json:"journal_entry_id"`
		}
		return decode(domainResult, &result) && decode(ledgerReferences, &ledger) &&
			result.CaseID != uuid.Nil && result.ReceivableID != uuid.Nil && result.WalletID != uuid.Nil &&
			result.AmountMicrocredits > 0 && result.ReceivableVersionBefore > 0 &&
			result.ReceivableVersionAfter > result.ReceivableVersionBefore &&
			result.ReceivableVersionAfter == result.ReceivableVersionBefore+1 &&
			(result.StatusAfter == "PARTIALLY_SETTLED" || result.StatusAfter == "WRITTEN_OFF") &&
			lowerHexDigest(result.FactsDigest) && result.JournalEntryID != uuid.Nil && result.AllocationID != uuid.Nil &&
			result.Status == "SUCCEEDED" && ledger.JournalEntryID == result.JournalEntryID
	}
	return false
}

func NewApprovalRecoveryResultClientWithToken(billingURL, tokenURL, credential string) (*ApprovalRecoveryResultClient, error) {
	c, err := NewApprovalRecoveryResultClient(billingURL, credential)
	if err != nil {
		return nil, err
	}
	token, err := recoveryResultEndpoint(tokenURL)
	if err != nil {
		return nil, err
	}
	c.tokenEndpoint = token
	return c, nil
}
