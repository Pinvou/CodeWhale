package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/google/uuid"
)

var ErrApprovalResultUnconfirmed = errors.New("TOKEN_APPROVAL_RESULT_UNCONFIRMED")

var approvedCommandPaths = map[string]string{
	"TOKEN_MODEL_LIFECYCLE_CHANGE": "model-lifecycle",
	"TOKEN_MODEL_CATALOG_ROLLBACK": "catalog-rollback",
	"TOKEN_CREDENTIAL_REVOKE":      "credential-revoke",
}

type tokenAuthorizedEvent struct {
	ApprovalRequestID        uuid.UUID       `json:"approval_request_id"`
	CommandType              string          `json:"command_type"`
	CommandFingerprint       string          `json:"command_fingerprint"`
	PolicyVersion            int64           `json:"policy_version"`
	ExecutorIdentity         string          `json:"executor_identity"`
	ExecutionDeadline        time.Time       `json:"execution_deadline"`
	NormalizedCommand        json.RawMessage `json:"normalized_command"`
	TargetResourceType       string          `json:"target_resource_type"`
	TargetResourceID         string          `json:"target_resource_id"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
}

type tokenApprovalLease struct {
	ApprovalRequestID             uuid.UUID       `json:"approval_request_id"`
	CommandType                   string          `json:"command_type"`
	PolicyVersion                 int64           `json:"policy_version"`
	CommandFingerprint            string          `json:"command_fingerprint"`
	NormalizedCommand             json.RawMessage `json:"normalized_command"`
	TargetResourceType            string          `json:"target_resource_type"`
	TargetResourceID              string          `json:"target_resource_id"`
	CriticalResourceVersions      json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot             json.RawMessage `json:"financial_snapshot"`
	RequesterUserID               uuid.UUID       `json:"requester_user_id"`
	RequesterAuthorizationVersion int64           `json:"requester_authorization_version"`
	RequesterReason               string          `json:"requester_reason"`
	FirstClaim                    bool            `json:"first_claim"`
	ApproverUserID                uuid.UUID       `json:"approver_user_id"`
	DecisionReason                string          `json:"decision_reason"`
	ExecutionIdempotencyKey       string          `json:"execution_idempotency_key"`
	ExecutorIdentity              string          `json:"executor_identity"`
	Status                        string          `json:"status"`
}

// TokenApprovalConsumer is an HTTP-only executor. It owns no Token database
// credentials and never turns missing/uncertain results into a second POST.
type TokenApprovalConsumer struct {
	commandType, tokenURL, accountURL, serviceToken string
	activation                                      tokengovernance.Reader
	client                                          *http.Client
	alert                                           func(uuid.UUID)
}

func NewTokenApprovalConsumer(commandType, tokenURL, accountURL, serviceToken string, activation tokengovernance.Reader) (*TokenApprovalConsumer, error) {
	if approvedCommandPaths[commandType] == "" || activation == nil || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("approval consumer dependencies are incomplete")
	}
	for _, raw := range []string{tokenURL, accountURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, errors.New("approval consumer requires absolute service URLs")
		}
	}
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &TokenApprovalConsumer{commandType: commandType, tokenURL: strings.TrimRight(tokenURL, "/"), accountURL: strings.TrimRight(accountURL, "/"), serviceToken: serviceToken, activation: activation, client: client, alert: func(id uuid.UUID) { log.Printf("TOKEN_APPROVAL_RESULT_UNCONFIRMED approval_request_id=%s", id) }}, nil
}

func (c *TokenApprovalConsumer) Handle(ctx context.Context, event events.Event) (resultErr error) {
	var selected tokenAuthorizedEvent
	if event.Validate() != nil || event.Type != "account.approval-command.authorized.v1" || decodeApprovalTransport(event.Payload, &selected) != nil || selected.ApprovalRequestID != event.AggregateID || selected.ApprovalRequestID == uuid.Nil || !validImpactFingerprint(selected.CommandFingerprint) || selected.PolicyVersion < 1 || selected.ExecutionDeadline.IsZero() || selected.TargetResourceType == "" || selected.TargetResourceID == "" || !validJSONObject(selected.NormalizedCommand) || !validJSONObject(selected.CriticalResourceVersions) || !validJSONObject(selected.FinancialSnapshot) {
		return events.ErrInvalidEvent
	}
	policy, err := approvalpolicy.Lookup(selected.CommandType)
	if err != nil || !approvalpolicy.Executable(selected.CommandType) || policy.Mode != approvalpolicy.ApprovalRequired || policy.PolicyVersion != selected.PolicyVersion || policy.ExecutorIdentity != selected.ExecutorIdentity {
		return events.ErrInvalidEvent
	}
	if selected.CommandType != c.commandType {
		return nil
	}
	defer func() {
		if resultErr != nil && c.alert != nil {
			c.alert(selected.ApprovalRequestID)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	active, err := c.activation.Snapshot(ctx)
	if err != nil || active.State != "ACTIVE" || active.Epoch < 1 {
		return ErrApprovalResultUnconfirmed
	}
	path := "/internal/v1/commands/" + approvedCommandPaths[c.commandType]
	lookup := path + "/result?approval_request_id=" + selected.ApprovalRequestID.String() + "&command_fingerprint=" + selected.CommandFingerprint
	var domain TokenCommandResult
	status, err := c.request(ctx, c.tokenURL, http.MethodGet, lookup, nil, &domain)
	if err != nil || status != 200 && status != 404 {
		return ErrApprovalResultUnconfirmed
	}
	found := status == 200
	var projection ApprovalSnapshotProjection
	if found {
		projection.CriticalResourceVersions = selected.CriticalResourceVersions
		projection.FinancialSnapshot = selected.FinancialSnapshot
	} else {
		// Decode into an independent projection. RawMessage.UnmarshalJSON reuses
		// its slice: aliasing the event here would overwrite its frozen facts when
		// PostgreSQL JSONB and the HTTP projection use different encodings.
		if !selected.ExecutionDeadline.After(time.Now()) {
			return ErrApprovalResultUnconfirmed
		}
		status, err = c.request(ctx, c.tokenURL, http.MethodPost, path+"/precheck", ApprovalSnapshotRequest{CommandType: selected.CommandType, TargetResourceType: selected.TargetResourceType, TargetResourceID: selected.TargetResourceID, NormalizedCommand: selected.NormalizedCommand}, &projection)
		if err != nil || status != 200 || projection.CommandType != selected.CommandType || projection.TargetResourceType != selected.TargetResourceType || projection.TargetResourceID != selected.TargetResourceID || !validImpactFingerprint(projection.PrecheckFingerprint) {
			return ErrApprovalResultUnconfirmed
		}
	}
	jobID := "token-approval:" + selected.ApprovalRequestID.String()
	leasePath := "/internal/v1/approval-requests/" + selected.ApprovalRequestID.String() + "/executions"
	var lease tokenApprovalLease
	status, err = c.request(ctx, c.accountURL, http.MethodPost, leasePath, map[string]any{"job_run_id": jobID, "observed_versions": projection.CriticalResourceVersions, "observed_financial_snapshot": projection.FinancialSnapshot}, &lease)
	if err != nil || status != 200 {
		return fmt.Errorf("%w: APPROVAL_LEASE_TRANSPORT_INVALID", ErrApprovalResultUnconfirmed)
	}
	if !validTokenApprovalLease(selected, lease) {
		return fmt.Errorf("%w: APPROVAL_LEASE_BINDING_INVALID", ErrApprovalResultUnconfirmed)
	}
	if !found && lease.FirstClaim {
		// The authoritative Account lease was genuinely INSERTed in this call.
		// No branch after this single request can resend the domain command.
		if !equalTokenApprovalJSONObjects(projection.CriticalResourceVersions, lease.CriticalResourceVersions) || !sameTokenApprovalFinancial(c.commandType, projection.FinancialSnapshot, lease.FinancialSnapshot) {
			return fmt.Errorf("%w: APPROVAL_LEASE_FACTS_CHANGED", ErrApprovalResultUnconfirmed)
		}
		envelope := TokenCommandEnvelope{CommandType: lease.CommandType, Reason: lease.RequesterReason, TargetResourceType: lease.TargetResourceType, TargetResourceID: lease.TargetResourceID, ExpectedVersions: lease.CriticalResourceVersions, PrecheckFingerprint: projection.PrecheckFingerprint, Command: lease.NormalizedCommand}
		if _, _, _, _, err := normalizeTokenCommand(envelope, lease.ExecutionIdempotencyKey, lease.CommandFingerprint); err != nil {
			return fmt.Errorf("%w: APPROVAL_COMMAND_ENVELOPE_INVALID", ErrApprovalResultUnconfirmed)
		}
		current, readErr := c.activation.Snapshot(ctx)
		if readErr != nil || current.State != "ACTIVE" || current.Epoch != active.Epoch {
			return fmt.Errorf("%w: APPROVAL_ACTIVATION_CHANGED", ErrApprovalResultUnconfirmed)
		}
		status, err = c.request(ctx, c.tokenURL, http.MethodPost, path+"/execute", approvedTokenCommandBody{ActivationEpoch: active.Epoch, ApprovalRequestID: lease.ApprovalRequestID, RequesterUserID: lease.RequesterUserID, AuthorizationVersion: lease.RequesterAuthorizationVersion, CommandFingerprint: lease.CommandFingerprint, ExecutionIdempotencyKey: lease.ExecutionIdempotencyKey, CriticalResourceVersions: lease.CriticalResourceVersions, FinancialSnapshot: lease.FinancialSnapshot, Envelope: envelope}, &domain)
		found = err == nil && (status == 200 || status == 202)
	}
	if !found {
		status, err = c.request(ctx, c.tokenURL, http.MethodGet, lookup, nil, &domain)
		if err != nil || status != 200 {
			return ErrApprovalResultUnconfirmed
		}
	}
	if domain.CommandReference == uuid.Nil || domain.ApprovalRequestID == nil || *domain.ApprovalRequestID != lease.ApprovalRequestID || domain.CommandType != lease.CommandType || domain.PolicyVersion != lease.PolicyVersion || domain.ActorUserID != lease.RequesterUserID || domain.AuthorizationVersion != lease.RequesterAuthorizationVersion || domain.TargetResourceType != lease.TargetResourceType || domain.TargetResourceID != lease.TargetResourceID || domain.CompletedAt == nil || domain.CompletedAt.IsZero() || (domain.Status != "SUCCEEDED" && domain.Status != "FAILED" && domain.Status != "REJECTED") {
		return ErrApprovalResultUnconfirmed
	}
	current, err := c.activation.Snapshot(ctx)
	if err != nil || current.State != "ACTIVE" || current.Epoch != active.Epoch {
		return ErrApprovalResultUnconfirmed
	}
	completion, errorClass := "EXECUTED", ""
	if domain.Status != "SUCCEEDED" {
		completion, errorClass = "FAILED", "DOMAIN_EXECUTION_FAILED"
	}
	// Account receives only a stable safe reference, not raw Token evidence.
	status, err = c.request(ctx, c.accountURL, http.MethodPost, leasePath+"/complete", map[string]any{"job_run_id": jobID, "result": completion, "domain_result": map[string]any{"command_reference": domain.CommandReference, "status": domain.Status}, "ledger_references": map[string]any{}, "error_class": errorClass}, nil)
	if err != nil || status != http.StatusNoContent {
		return ErrApprovalResultUnconfirmed
	}
	return nil
}

func validTokenApprovalLease(event tokenAuthorizedEvent, lease tokenApprovalLease) bool {
	return lease.ApprovalRequestID == event.ApprovalRequestID && lease.CommandType == event.CommandType && lease.PolicyVersion == event.PolicyVersion && lease.CommandFingerprint == event.CommandFingerprint && lease.ExecutorIdentity == "token-command-executor" && lease.TargetResourceType == event.TargetResourceType && lease.TargetResourceID == event.TargetResourceID && lease.RequesterUserID != uuid.Nil && lease.ApproverUserID != uuid.Nil && lease.RequesterUserID != lease.ApproverUserID && lease.RequesterAuthorizationVersion > 0 && strings.TrimSpace(lease.RequesterReason) != "" && (lease.Status == "EXECUTING" || lease.Status == "EXECUTED") && lease.ExecutionIdempotencyKey == fmt.Sprintf("approval:%s:%s:%s", event.ApprovalRequestID, event.CommandType, event.CommandFingerprint) && equalTokenApprovalJSONObjects(lease.NormalizedCommand, event.NormalizedCommand) && equalTokenApprovalJSONObjects(lease.CriticalResourceVersions, event.CriticalResourceVersions) && equalTokenApprovalJSONObjects(lease.FinancialSnapshot, event.FinancialSnapshot)
}

// Frozen execution facts contain int64 versions. Converting JSON numbers to
// float64 would collapse adjacent integers above 2^53 and authorize a different
// version. This comparison is local to the consumer, not a domain parser change.
func equalTokenApprovalJSONObjects(left, right json.RawMessage) bool {
	canonical := func(raw json.RawMessage) ([]byte, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var object map[string]any
		if err := decoder.Decode(&object); err != nil || object == nil {
			return nil, ErrApprovalResultUnconfirmed
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, ErrApprovalResultUnconfirmed
		}
		return json.Marshal(object)
	}
	a, leftErr := canonical(left)
	b, rightErr := canonical(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(a, b)
}

func sameTokenApprovalFinancial(command string, left, right json.RawMessage) bool {
	if command == "TOKEN_CREDENTIAL_REVOKE" {
		return exactEmptyFinancialSnapshot(left) && exactEmptyFinancialSnapshot(right)
	}
	return approvalpolicy.SameTokenModelFinancialFacts(left, right)
}

func (c *TokenApprovalConsumer) request(ctx context.Context, base, method, path string, payload, output any) (int, error) {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return 0, ErrApprovalResultUnconfirmed
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil || serviceauth.SetAuthorization(req, c.serviceToken) != nil {
		return 0, ErrApprovalResultUnconfirmed
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	response, err := c.client.Do(req)
	if err != nil {
		return 0, ErrApprovalResultUnconfirmed
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, nil
	}
	if output != nil {
		raw, err := io.ReadAll(io.LimitReader(response.Body, (128<<10)+1))
		if err != nil || len(raw) > 128<<10 || decodeApprovalTransport(raw, output) != nil {
			return response.StatusCode, ErrApprovalResultUnconfirmed
		}
	}
	return response.StatusCode, nil
}

// Reject duplicate keys recursively before ordinary strict struct decoding;
// a map/DisallowUnknownFields alone silently accepts the last duplicate.
func decodeApprovalTransport(raw []byte, target any) error {
	// encoding/json accepts case-insensitive aliases; authority-bearing fields
	// such as first_claim must instead match their exact wire tag and be present.
	shape := reflect.TypeOf(target)
	if shape == nil || shape.Kind() != reflect.Pointer || shape.Elem().Kind() != reflect.Struct {
		return ErrApprovalResultUnconfirmed
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return ErrApprovalResultUnconfirmed
	}
	known := map[string]bool{}
	for index := 0; index < shape.Elem().NumField(); index++ {
		field := shape.Elem().Field(index)
		tag := strings.Split(field.Tag.Get("json"), ",")
		if tag[0] == "" || tag[0] == "-" {
			return ErrApprovalResultUnconfirmed
		}
		known[tag[0]] = true
		value, present := object[tag[0]]
		optional := len(tag) > 1 && tag[1] == "omitempty"
		if !optional && (!present || bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return ErrApprovalResultUnconfirmed
		}
	}
	for key := range object {
		if !known[key] {
			return ErrApprovalResultUnconfirmed
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || seen[key] {
					return ErrApprovalResultUnconfirmed
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return ErrApprovalResultUnconfirmed
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return ErrApprovalResultUnconfirmed
			}
		default:
			return ErrApprovalResultUnconfirmed
		}
		return nil
	}
	if err := walk(); err != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrApprovalResultUnconfirmed
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrApprovalResultUnconfirmed
	}
	return nil
}
