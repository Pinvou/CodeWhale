package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
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
	ProofSchemaVersion       string          `json:"proof_schema_version,omitempty"`
	CommandDigest            string          `json:"command_digest,omitempty"`
	SnapshotDigest           string          `json:"snapshot_digest,omitempty"`
	DomainRevision           string          `json:"domain_revision,omitempty"`
	ProofDigest              string          `json:"proof_digest,omitempty"`
	ApprovalControlEpoch     int64           `json:"approval_control_epoch,omitempty"`
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
	AuthorizationEventID          *uuid.UUID      `json:"authorization_event_id,omitempty"`
	AuthorizationDurable          string          `json:"authorization_durable,omitempty"`
	AuthorizationApprovedVersion  int64           `json:"authorization_approved_request_version,omitempty"`
	AuthorizationEventDigest      string          `json:"authorization_event_digest,omitempty"`
	AuthorizationDigestKind       string          `json:"authorization_digest_kind,omitempty"`
	AuthorizationSerialization    string          `json:"authorization_serialization_version,omitempty"`
	AuthorizationProvenance       string          `json:"authorization_provenance,omitempty"`
	AuthorizationApprovalEpoch    int64           `json:"authorization_approval_epoch,omitempty"`
}

// TokenApprovalConsumer holds shared HTTP execution configuration and transport.
// It owns no Token database credentials; TokenAuthorizationCommandConsumer owns
// delivery state, single-claim execution and acknowledgements.
type TokenApprovalConsumer struct {
	commandType, tokenURL, accountURL, serviceToken string
	activation                                      tokengovernance.Reader
	client                                          *http.Client
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
	return &TokenApprovalConsumer{commandType: commandType, tokenURL: strings.TrimRight(tokenURL, "/"), accountURL: strings.TrimRight(accountURL, "/"), serviceToken: serviceToken, activation: activation, client: client}, nil
}

func validTokenApprovalLease(event tokenAuthorizedEvent, lease tokenApprovalLease) bool {
	return lease.ApprovalRequestID == event.ApprovalRequestID && lease.CommandType == event.CommandType && lease.PolicyVersion == event.PolicyVersion && lease.CommandFingerprint == event.CommandFingerprint && lease.ExecutorIdentity == "token-command-executor" && lease.TargetResourceType == event.TargetResourceType && lease.TargetResourceID == event.TargetResourceID && lease.RequesterUserID != uuid.Nil && lease.ApproverUserID != uuid.Nil && lease.RequesterUserID != lease.ApproverUserID && lease.RequesterAuthorizationVersion > 0 && strings.TrimSpace(lease.RequesterReason) != "" && (lease.Status == "EXECUTING" || lease.Status == "EXECUTED" || lease.Status == "EXECUTION_FAILED") && lease.ExecutionIdempotencyKey == fmt.Sprintf("approval:%s:%s:%s", event.ApprovalRequestID, event.CommandType, event.CommandFingerprint) && equalTokenApprovalJSONObjects(lease.NormalizedCommand, event.NormalizedCommand) && equalTokenApprovalJSONObjects(lease.CriticalResourceVersions, event.CriticalResourceVersions) && equalTokenApprovalJSONObjects(lease.FinancialSnapshot, event.FinancialSnapshot)
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
