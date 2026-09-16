package account

import (
	"bytes"
	"context"
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

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

var ErrApprovalRecoveryPlanUnavailable = errors.New("approval recovery plan service is unavailable")

// ApprovalRecoveryPlanClient reads the Account snapshot over the dedicated
// workload identity. It deliberately has no Account write or domain API.
type ApprovalRecoveryPlanClient struct {
	endpoint     *url.URL
	serviceToken string
	httpClient   *http.Client
}

func NewApprovalRecoveryPlanClient(baseURL, serviceToken string) (*ApprovalRecoveryPlanClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || strings.TrimSpace(serviceToken) == "" {
		return nil, ErrApprovalRecoveryPlanUnavailable
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/internal/v1/approval-recovery/plan"
	parsed.RawPath = ""
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ApprovalRecoveryPlanClient{endpoint: parsed, serviceToken: serviceToken, httpClient: client}, nil
}

func (c *ApprovalRecoveryPlanClient) PlanProofApprovalRecovery(ctx context.Context, limit int) (ApprovalRecoveryPlan, error) {
	if c == nil || c.endpoint == nil || c.httpClient == nil || limit < 1 || limit > 200 {
		return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanUnavailable
	}
	endpoint := *c.endpoint
	query := endpoint.Query()
	query.Set("limit", strconv.Itoa(limit))
	endpoint.RawQuery = query.Encode()
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(bounded, http.MethodGet, endpoint.String(), nil)
	if err != nil || serviceauth.SetAuthorization(request, c.serviceToken) != nil {
		return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanUnavailable
	}
	request.Header.Set("Cache-Control", "no-cache")
	startedAt := time.Now().UTC()
	response, err := c.httpClient.Do(request)
	if err != nil {
		return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanUnavailable
	}
	defer response.Body.Close()
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	cacheControl := response.Header.Values("Cache-Control")
	if response.StatusCode != http.StatusOK || mediaErr != nil || mediaType != "application/json" || len(cacheControl) != 1 || cacheControl[0] != "no-store" {
		return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanUnavailable
	}
	const maximumPlanBytes = 512 << 10
	raw, err := io.ReadAll(io.LimitReader(response.Body, maximumPlanBytes+1))
	if err != nil || len(raw) > maximumPlanBytes || !utf8.Valid(raw) {
		return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanUnavailable
	}
	if !uniqueRecoveryJSONKeys(raw) {
		return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanUnavailable
	}
	var view approvalRecoveryPlanView
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&view) != nil || decoder.Decode(new(any)) != io.EOF || view.SchemaVersion != 1 || len(view.Candidates) > limit {
		return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanUnavailable
	}
	control := AdminApprovalControl{
		ControlID: view.Control.ControlID, SchemaVersion: view.Control.SchemaVersion, Mode: view.Control.Mode,
		ApprovalWireVersion: view.Control.ApprovalWireVersion, ProofRequired: view.Control.ProofRequired,
		AuthorizationRelayState: view.Control.AuthorizationRelayState, ApprovalEpoch: view.Control.ApprovalEpoch,
		AuthorizationRelayEpoch: view.Control.AuthorizationRelayEpoch, Version: view.Control.Version, UpdatedAt: view.Control.UpdatedAt,
	}
	if validateAdminApprovalControl(control) != nil || view.ObservedAt.Before(startedAt.Add(-30*time.Second)) ||
		view.ObservedAt.After(time.Now().UTC().Add(30*time.Second)) {
		return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanUnavailable
	}
	for _, candidate := range view.Candidates {
		if !validRecoveryInspectionCandidate(candidate) || candidate.OriginalKey == "" || candidate.RequestVersion < 1 ||
			candidate.ApprovalEpoch < 1 || candidate.ExecutionDeadline.IsZero() || candidate.WindowElapsed != !candidate.ExecutionDeadline.After(view.ObservedAt) {
			return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanUnavailable
		}
	}
	return ApprovalRecoveryPlan{ObservedAt: view.ObservedAt, Control: control, Candidates: view.Candidates, MoreCandidates: view.MoreCandidates}, nil
}

func uniqueRecoveryJSONKeys(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if readUniqueRecoveryValue(decoder, 0) != nil {
		return false
	}
	return decoder.Decode(new(any)) == io.EOF
}

func readUniqueRecoveryValue(decoder *json.Decoder, depth int) error {
	if depth > 16 {
		return ErrApprovalRecoveryPlanUnavailable
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok {
				return ErrApprovalRecoveryPlanUnavailable
			}
			if _, duplicate := seen[key]; duplicate {
				return ErrApprovalRecoveryPlanUnavailable
			}
			seen[key] = struct{}{}
			if err := readUniqueRecoveryValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrApprovalRecoveryPlanUnavailable
		}
	case json.Delim('['):
		for decoder.More() {
			if err := readUniqueRecoveryValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrApprovalRecoveryPlanUnavailable
		}
	}
	return nil
}
