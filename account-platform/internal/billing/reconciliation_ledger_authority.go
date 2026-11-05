package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

// ErrLedgerRepairAuthorityUnavailable is distinct from an invalid financial
// fact: an unavailable authority must not manufacture a ledger discrepancy.
var ErrLedgerRepairAuthorityUnavailable = errors.New("ledger repair authorization authority unavailable")

type ledgerRepairProofIdentity struct {
	ApprovalRequestID    uuid.UUID `json:"approval_request_id"`
	AuthorizationEventID uuid.UUID `json:"authorization_event_id"`
	ApprovalEpoch        int64     `json:"approval_epoch"`
}

type ledgerRepairAuthorizationProof struct {
	ledgerRepairProofIdentity
	Status                   string          `json:"status"`
	CommandType              string          `json:"command_type,omitempty"`
	PolicyVersion            int64           `json:"policy_version,omitempty"`
	SnapshotSchemaVersion    int64           `json:"snapshot_schema_version,omitempty"`
	ExecutionKeyDigest       string          `json:"execution_key_digest,omitempty"`
	CommandDigest            string          `json:"command_digest,omitempty"`
	SnapshotDigest           string          `json:"snapshot_digest,omitempty"`
	DomainRevision           string          `json:"domain_revision,omitempty"`
	NormalizedCommand        json.RawMessage `json:"normalized_command,omitempty"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions,omitempty"`
	FinancialSnapshot        json.RawMessage `json:"financial_snapshot,omitempty"`
}

type ledgerRepairProofAuthority interface {
	ReadLedgerRepairProofs(context.Context, []ledgerRepairProofIdentity) ([]ledgerRepairAuthorizationProof, error)
}

type httpLedgerRepairProofAuthority struct {
	baseURL    *url.URL
	credential string
	client     *http.Client
}

// NewHTTPLedgerRepairProofAuthority uses the configured Billing workload
// credential; it does not expose or persist Account's execution key.
func NewHTTPLedgerRepairProofAuthority(rawURL, credential, environment string) (ledgerRepairProofAuthority, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" || strings.TrimSpace(credential) == "" || strings.TrimSpace(environment) == "" || !ledgerRepairTrustedAuthorityURL(parsed, environment) {
		return nil, errors.New("ledger repair proof authority requires Account URL and service identity")
	}
	client, err := serviceauth.NewWorkloadHTTPClient(strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_CA_FILE")))
	if err != nil {
		return nil, errors.New("ledger repair proof authority trust configuration invalid")
	}
	client.Timeout = 5 * time.Second
	return &httpLedgerRepairProofAuthority{baseURL: parsed, credential: strings.TrimSpace(credential), client: client}, nil
}

var ledgerRepairHelmAccountHost = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?-account-platform-account-service$`)

func ledgerRepairTrustedAuthorityURL(endpoint *url.URL, environment string) bool {
	if endpoint.Scheme == "https" {
		return true
	}
	if endpoint.Scheme != "http" || endpoint.Path != "" {
		return false
	}
	host := strings.ToLower(endpoint.Hostname())
	if endpoint.Port() == "8080" && (host == "account-service" || ledgerRepairHelmAccountHost.MatchString(host)) {
		return true
	}
	// The project's only explicitly nonproduction execution modes are
	// development and test; never infer a safe mode from a missing value.
	env := strings.ToLower(strings.TrimSpace(environment))
	if env != "development" && env != "test" {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

// Account limits a frozen event to 64 KiB and accepts at most 64 identities.
// A smaller outbound chunk keeps even worst-case proofs below our 2 MiB bound.
const ledgerRepairProofBatchLimit = 16

func (v *httpLedgerRepairProofAuthority) ReadLedgerRepairProofs(ctx context.Context, items []ledgerRepairProofIdentity) ([]ledgerRepairAuthorizationProof, error) {
	if len(items) == 0 {
		return nil, nil
	}
	seen := make(map[ledgerRepairProofIdentity]bool, len(items))
	for _, item := range items {
		if item.ApprovalRequestID == uuid.Nil || item.AuthorizationEventID == uuid.Nil || item.ApprovalEpoch <= 0 || seen[item] {
			return nil, ErrLedgerRepairAuthorityUnavailable
		}
		seen[item] = true
	}
	result := make([]ledgerRepairAuthorizationProof, 0, len(items))
	for first := 0; first < len(items); first += ledgerRepairProofBatchLimit {
		last := min(first+ledgerRepairProofBatchLimit, len(items))
		chunk, err := v.readProofBatch(ctx, items[first:last])
		if err != nil {
			// Preserve earlier authenticated chunks; callers mark only missing
			// identities inconclusive and do not turn this into a ledger error.
			return result, err
		}
		result = append(result, chunk...)
	}
	return result, nil
}

func (v *httpLedgerRepairProofAuthority) readProofBatch(ctx context.Context, items []ledgerRepairProofIdentity) ([]ledgerRepairAuthorizationProof, error) {
	body, err := json.Marshal(struct {
		SchemaVersion int                         `json:"schema_version"`
		Items         []ledgerRepairProofIdentity `json:"items"`
	}{SchemaVersion: 1, Items: items})
	if err != nil {
		return nil, ErrLedgerRepairAuthorityUnavailable
	}
	endpoint := *v.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/ledger-repair/authorization-proofs:batch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil || serviceauth.SetAuthorization(req, v.credential) != nil {
		return nil, ErrLedgerRepairAuthorityUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := v.client.Do(req)
	if err != nil {
		return nil, ErrLedgerRepairAuthorityUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, ErrLedgerRepairAuthorityUnavailable
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(encoded) > 2<<20 {
		return nil, ErrLedgerRepairAuthorityUnavailable
	}
	var result struct {
		SchemaVersion int                              `json:"schema_version"`
		Items         []ledgerRepairAuthorizationProof `json:"items"`
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.SchemaVersion != 1 || len(result.Items) != len(items) {
		return nil, ErrLedgerRepairAuthorityUnavailable
	}
	var trailing any
	if !errors.Is(decoder.Decode(&trailing), io.EOF) {
		return nil, ErrLedgerRepairAuthorityUnavailable
	}
	for i, proof := range result.Items {
		if proof.ledgerRepairProofIdentity != items[i] || (proof.Status != "VERIFIED" && proof.Status != "UNAVAILABLE") {
			return nil, ErrLedgerRepairAuthorityUnavailable
		}
	}
	return result.Items, nil
}
