// Package billinggovernance defines Billing's read-only activation contract.
// It owns neither the domain state nor authority to activate commands.
package billinggovernance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

var ErrUnavailable = errors.New("Billing approval activation is unavailable")

const maxResponseBytes = 16 << 10

type Activation struct {
	CommandType           string    `json:"command_type"`
	PolicyVersion         int64     `json:"policy_version"`
	SnapshotSchemaVersion int       `json:"snapshot_schema_version"`
	State                 string    `json:"state"`
	Version               int64     `json:"version"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type Snapshot struct {
	SchemaVersion int          `json:"schema_version"`
	Activations   []Activation `json:"activations"`
}

type Reader interface {
	Snapshot(context.Context) (Snapshot, error)
}

// Find never substitutes a current policy for the request's frozen version.
func (s Snapshot) Find(command string, policyVersion int64, snapshotSchemaVersion int) (Activation, bool) {
	for _, activation := range s.Activations {
		if activation.CommandType == command && activation.PolicyVersion == policyVersion && activation.SnapshotSchemaVersion == snapshotSchemaVersion {
			return activation, true
		}
	}
	return Activation{}, false
}

type activationVersion struct {
	policyVersion         int64
	snapshotSchemaVersion int
}

// This is the closed compatibility set, not a mutable execution allowlist.
// New policy versions require an explicit reader/owner-domain rollout: the
// entitlement correction command keeps exactly one v1 row as immutable history
// and may append a v2 row, so this reader accepts either pair and never rewrites
// the frozen version of an existing request. LEDGER_REPAIR keeps exactly one
// v1/s1 pair; its state is still selected explicitly by the operator.
var activationVersions = map[string][]activationVersion{
	"CHARGEBACK_FINANCIAL_REPAIR":      {{1, 1}},
	"ENTITLEMENT_COMPONENT_ADJUSTMENT": {{1, 1}, {2, 2}},
	"LEDGER_REPAIR":                    {{1, 1}},
	"PAYMENT_FINANCIAL_REPAIR":         {{1, 1}},
	"PAYMENT_PRODUCT_RESUME":           {{1, 1}},
	"PAYMENT_PROVIDER_RESUME":          {{2, 1}},
	"RECEIVABLE_WRITE_OFF":             {{1, 1}},
	"RECONCILIATION_FINANCIAL_REPAIR":  {{1, 1}},
	"REFUND_EXCEPTION_REPAIR":          {{1, 1}},
	"USAGE_SETTLEMENT_REPAIR":          {{1, 1}},
	"WALLET_ADJUSTMENT":                {{1, 1}},
}

func knownActivationVersion(command string, policyVersion int64, snapshotSchemaVersion int) bool {
	for _, version := range activationVersions[command] {
		if version.policyVersion == policyVersion && version.snapshotSchemaVersion == snapshotSchemaVersion {
			return true
		}
	}
	return false
}

// ValidateSnapshot also protects in-process readers. An INACTIVE existing
// command is valid authoritative data, not an unavailable transport.
func ValidateSnapshot(s Snapshot) error {
	if s.SchemaVersion != 1 || (len(s.Activations) != len(activationVersions) && len(s.Activations) != len(activationVersions)+1) {
		return ErrUnavailable
	}
	latest := time.Now().Add(time.Second)
	previous := ""
	previousPolicy := int64(0)
	seen := make(map[string]bool, len(activationVersions))
	for _, activation := range s.Activations {
		_, known := activationVersions[activation.CommandType]
		if !known || activation.CommandType < previous || (activation.CommandType == previous && (activation.CommandType != "ENTITLEMENT_COMPONENT_ADJUSTMENT" || previousPolicy != 1 || activation.PolicyVersion != 2)) || !knownActivationVersion(activation.CommandType, activation.PolicyVersion, activation.SnapshotSchemaVersion) ||
			activation.Version < 1 || activation.UpdatedAt.IsZero() || activation.UpdatedAt.After(latest) {
			return ErrUnavailable
		}
		if activation.State != "ACTIVE" && activation.State != "INACTIVE" {
			return ErrUnavailable
		}
		switch activation.CommandType {
		case "PAYMENT_FINANCIAL_REPAIR", "RECEIVABLE_WRITE_OFF", "LEDGER_REPAIR", "WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME":
		case "ENTITLEMENT_COMPONENT_ADJUSTMENT":
			if activation.PolicyVersion == 1 && activation.State != "INACTIVE" || activation.PolicyVersion == 2 && (!seen[activation.CommandType] || previousPolicy != 1) {
				return ErrUnavailable
			}
		default:
			if activation.State != "INACTIVE" {
				return ErrUnavailable
			}
		}
		previous = activation.CommandType
		previousPolicy = activation.PolicyVersion
		seen[activation.CommandType] = true
	}
	if len(seen) != len(activationVersions) {
		return ErrUnavailable
	}
	return nil
}

type Client struct {
	endpoint     string
	serviceToken string
	client       *http.Client
}

func NewClient(baseURL, serviceToken string) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || strings.TrimSpace(serviceToken) == "" {
		return nil, ErrUnavailable
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/internal/v1/approval-command-activations"
	u.RawPath = ""
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{endpoint: u.String(), serviceToken: serviceToken, client: client}, nil
}

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	if c == nil || c.client == nil {
		return Snapshot{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil || serviceauth.SetAuthorization(request, c.serviceToken) != nil {
		return Snapshot{}, ErrUnavailable
	}
	request.Header.Set("Cache-Control", "no-cache")
	response, err := c.client.Do(request)
	if err != nil {
		return Snapshot{}, ErrUnavailable
	}
	defer response.Body.Close()
	cacheControl := response.Header.Values("Cache-Control")
	if response.StatusCode != http.StatusOK || len(cacheControl) != 1 || cacheControl[0] != "no-store" {
		return Snapshot{}, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes || !utf8.Valid(raw) {
		return Snapshot{}, ErrUnavailable
	}
	root, err := closedObject(raw, "schema_version", "activations")
	if err != nil {
		return Snapshot{}, ErrUnavailable
	}
	var rows []json.RawMessage
	if json.Unmarshal(root["activations"], &rows) != nil || (len(rows) != len(activationVersions) && len(rows) != len(activationVersions)+1) {
		return Snapshot{}, ErrUnavailable
	}
	for _, row := range rows {
		if _, err := closedObject(row, "command_type", "policy_version", "snapshot_schema_version", "state", "version", "updated_at"); err != nil {
			return Snapshot{}, ErrUnavailable
		}
	}
	var snapshot Snapshot
	if json.Unmarshal(raw, &snapshot) != nil || ValidateSnapshot(snapshot) != nil {
		return Snapshot{}, ErrUnavailable
	}
	return snapshot, nil
}

// encoding/json alone accepts duplicate names and case aliases. Check exact
// keys and presence before decoding typed values, at every object level.
func closedObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, ErrUnavailable
	}
	values := make(map[string]json.RawMessage, len(keys))
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || values[key] != nil {
			return nil, ErrUnavailable
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrUnavailable
		}
		values[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || len(values) != len(keys) {
		return nil, ErrUnavailable
	}
	return values, nil
}
