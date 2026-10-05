package portal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

type serviceStatus struct {
	Key       string `json:"key"`
	Status    string `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Message   string `json:"message,omitempty"`
}

type relayStatus struct {
	Key                 string  `json:"key"`
	Status              string  `json:"status"`
	PendingEvents       int64   `json:"pending_events"`
	DeadLetteredEvents  int64   `json:"dead_lettered_events"`
	OldestPendingSecond float64 `json:"oldest_pending_seconds"`
	LatencyMS           int64   `json:"latency_ms"`
	Message             string  `json:"message,omitempty"`
}

type reconciliationStatus struct {
	Pending      int64         `json:"pending"`
	Stopped      int64         `json:"stopped"`
	RecentErrors []recentError `json:"recent_errors"`
}

type recentError struct {
	Source    string    `json:"source"`
	Resource  string    `json:"resource,omitempty"`
	Message   string    `json:"message"`
	UpdatedAt time.Time `json:"updated_at"`
}

type systemStatusResponse struct {
	CheckedAt      time.Time            `json:"checked_at"`
	Services       []serviceStatus      `json:"services"`
	Relays         []relayStatus        `json:"relays"`
	Reconciliation reconciliationStatus `json:"reconciliation"`
	RecentErrors   []recentError        `json:"recent_errors"`
}

func (s *Service) adminSystemStatus(w http.ResponseWriter, r *http.Request) {
	session, err := s.authenticatedSession(r.Context(), r)
	if err != nil {
		s.writeSessionError(w, r, err)
		return
	}
	authorizationContext, cancelAuthorization := context.WithTimeout(r.Context(), 5*time.Second)
	authorized, available := s.hasSystemStatusPermission(authorizationContext, session.AccessToken)
	cancelAuthorization()
	if !available {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_AUTH_UNAVAILABLE", "administrator authorization could not be verified", httpserver.RequestID(r.Context()))
		return
	}
	if !authorized {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "platform system status permission is required", httpserver.RequestID(r.Context()))
		return
	}
	statusContext, cancelStatus := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancelStatus()
	result := s.collectSystemStatus(statusContext, session.AccessToken)
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) hasSystemStatusPermission(ctx context.Context, accessToken string) (bool, bool) {
	targets := []*url.URL{s.config.AccountURL, s.config.BillingURL, s.config.TokenGatewayURL}
	type result struct{ status int }
	results := make(chan result, len(targets))
	for _, target := range targets {
		go func(base *url.URL) {
			response, err := s.get(ctx, endpoint(base, "/admin/v1/overview"), accessToken)
			if err != nil {
				results <- result{}
				return
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			results <- result{status: response.StatusCode}
		}(target)
	}
	denied := false
	for range targets {
		result := <-results
		if result.status >= 200 && result.status < 300 {
			return true, true
		}
		if result.status == http.StatusUnauthorized || result.status == http.StatusForbidden {
			denied = true
		}
	}
	return false, denied
}

func (s *Service) collectSystemStatus(ctx context.Context, accessToken string) systemStatusResponse {
	result := systemStatusResponse{CheckedAt: s.now(), Services: make([]serviceStatus, 0, 8), Relays: make([]relayStatus, 0, 3), RecentErrors: make([]recentError, 0)}
	serviceTargets := []struct {
		key    string
		target *url.URL
	}{
		{key: "account", target: endpoint(s.config.AccountURL, "/health/ready")},
		{key: "billing", target: endpoint(s.config.BillingURL, "/health/ready")},
		{key: "token-gateway", target: endpoint(s.config.TokenGatewayURL, "/health/ready")},
		{key: "keycloak", target: s.config.KeycloakStatusURL},
		{key: "newapi", target: endpoint(s.config.TokenGatewayURL, "/admin/v1/system/newapi-status")},
		{key: "nats", target: s.config.NATSStatusURL},
	}
	statuses := make(chan serviceStatus, len(serviceTargets))
	var wait sync.WaitGroup
	for _, target := range serviceTargets {
		wait.Add(1)
		go func(key string, target *url.URL) {
			defer wait.Done()
			if key == "newapi" {
				statuses <- s.probeAuthenticated(ctx, key, target, accessToken)
			} else {
				statuses <- s.probe(ctx, key, target)
			}
		}(target.key, target.target)
	}
	relays := make(chan relayStatus, len(s.config.RelayURLs))
	for key, target := range s.config.RelayURLs {
		wait.Add(1)
		go func(key string, target *url.URL) {
			defer wait.Done()
			relays <- s.probeRelay(ctx, key, target)
		}(key, target)
	}
	wait.Wait()
	close(statuses)
	close(relays)
	result.Services = append(result.Services, serviceStatus{Key: "portal", Status: "HEALTHY"})
	for status := range statuses {
		result.Services = append(result.Services, status)
		if status.Status != "HEALTHY" {
			result.RecentErrors = append(result.RecentErrors, recentError{Source: status.Key, Message: status.Message, UpdatedAt: result.CheckedAt})
		}
	}
	for relay := range relays {
		result.Relays = append(result.Relays, relay)
		if relay.Status != "HEALTHY" || relay.DeadLetteredEvents > 0 {
			message := relay.Message
			if message == "" {
				message = fmt.Sprintf("%d dead-lettered events", relay.DeadLetteredEvents)
			}
			result.RecentErrors = append(result.RecentErrors, recentError{Source: relay.Key, Message: message, UpdatedAt: result.CheckedAt})
		}
	}
	result.Reconciliation = s.reconciliationStatus(ctx, accessToken)
	result.RecentErrors = append(result.RecentErrors, result.Reconciliation.RecentErrors...)
	sort.Slice(result.Services, func(i, j int) bool { return result.Services[i].Key < result.Services[j].Key })
	sort.Slice(result.Relays, func(i, j int) bool { return result.Relays[i].Key < result.Relays[j].Key })
	sort.Slice(result.RecentErrors, func(i, j int) bool { return result.RecentErrors[i].UpdatedAt.After(result.RecentErrors[j].UpdatedAt) })
	if len(result.RecentErrors) > 20 {
		result.RecentErrors = result.RecentErrors[:20]
	}
	return result
}

func (s *Service) probe(ctx context.Context, key string, target *url.URL) serviceStatus {
	return s.probeAuthenticated(ctx, key, target, "")
}

func (s *Service) probeAuthenticated(ctx context.Context, key string, target *url.URL, accessToken string) serviceStatus {
	if target == nil {
		return serviceStatus{Key: key, Status: "UNKNOWN", Message: "status endpoint is not configured"}
	}
	started := time.Now()
	response, err := s.get(ctx, target, accessToken)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return serviceStatus{Key: key, Status: "UNHEALTHY", LatencyMS: latency, Message: safeProbeError(err)}
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return serviceStatus{Key: key, Status: "UNHEALTHY", LatencyMS: latency, Message: fmt.Sprintf("HTTP %d", response.StatusCode)}
	}
	if key == "newapi" && len(body) > 0 {
		var payload map[string]any
		if json.Unmarshal(body, &payload) == nil {
			if success, present := payload["success"].(bool); present && !success {
				return serviceStatus{Key: key, Status: "UNHEALTHY", LatencyMS: latency, Message: "NewAPI status check failed"}
			}
		}
	}
	return serviceStatus{Key: key, Status: "HEALTHY", LatencyMS: latency}
}

func (s *Service) probeRelay(ctx context.Context, key string, base *url.URL) relayStatus {
	started := time.Now()
	health := s.probe(ctx, key, endpoint(base, "/health/live"))
	result := relayStatus{Key: key, Status: health.Status, Message: health.Message}
	if health.Status != "HEALTHY" {
		result.LatencyMS = time.Since(started).Milliseconds()
		return result
	}
	response, err := s.get(ctx, endpoint(base, "/metrics"), "")
	if err != nil {
		result.Status, result.Message = "DEGRADED", safeProbeError(err)
		result.LatencyMS = time.Since(started).Milliseconds()
		return result
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.Status, result.Message = "DEGRADED", fmt.Sprintf("metrics HTTP %d", response.StatusCode)
		result.LatencyMS = time.Since(started).Milliseconds()
		return result
	}
	metrics, err := parseRelayMetrics(io.LimitReader(response.Body, 256<<10))
	if err != nil {
		result.Status, result.Message = "DEGRADED", "relay metrics are invalid"
	} else {
		result.PendingEvents = int64(metrics["pinvou_outbox_pending_events"])
		result.DeadLetteredEvents = int64(metrics["pinvou_outbox_dead_lettered_events"])
		result.OldestPendingSecond = metrics["pinvou_outbox_oldest_pending_age_seconds"]
		if result.DeadLetteredEvents > 0 {
			result.Status = "DEGRADED"
		}
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	return result
}

func parseRelayMetrics(reader io.Reader) (map[string]float64, error) {
	result := map[string]float64{}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.SplitN(fields[0], "{", 2)[0]
		if !strings.HasPrefix(name, "pinvou_outbox_") {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, errors.New("invalid relay metric value")
		}
		result[name] = value
	}
	for _, name := range []string{"pinvou_outbox_pending_events", "pinvou_outbox_dead_lettered_events", "pinvou_outbox_oldest_pending_age_seconds"} {
		if _, ok := result[name]; !ok {
			return nil, errors.New("required relay metric is missing")
		}
	}
	return result, scanner.Err()
}

func (s *Service) reconciliationStatus(ctx context.Context, accessToken string) reconciliationStatus {
	result := reconciliationStatus{RecentErrors: make([]recentError, 0)}
	var overview struct {
		Pending *int64 `json:"reconciliation_pending"`
		Stopped *int64 `json:"reconciliation_stopped"`
	}
	if err := s.getJSON(ctx, endpoint(s.config.TokenGatewayURL, "/admin/v1/overview"), accessToken, &overview); err == nil && overview.Pending != nil && overview.Stopped != nil && *overview.Pending >= 0 && *overview.Stopped >= 0 {
		result.Pending, result.Stopped = *overview.Pending, *overview.Stopped
	} else {
		result.RecentErrors = append(result.RecentErrors, recentError{Source: "token-reconciliation", Message: "reconciliation counters could not be read", UpdatedAt: s.now()})
	}
	var usage struct {
		Items []struct {
			ID        string    `json:"usage_request_id"`
			LastError string    `json:"reconciliation_last_error"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"items"`
	}
	target := endpoint(s.config.TokenGatewayURL, "/admin/v1/usage-requests")
	if target != nil {
		target.RawQuery = "limit=20"
	}
	if err := s.getJSON(ctx, target, accessToken, &usage); err == nil && usage.Items != nil {
		for _, item := range usage.Items {
			if strings.TrimSpace(item.LastError) != "" {
				result.RecentErrors = append(result.RecentErrors, recentError{Source: "token-reconciliation", Resource: item.ID, Message: item.LastError, UpdatedAt: item.UpdatedAt})
			}
		}
	} else {
		result.RecentErrors = append(result.RecentErrors, recentError{Source: "token-reconciliation", Message: "recent reconciliation errors could not be read", UpdatedAt: s.now()})
	}
	return result
}

func (s *Service) getJSON(ctx context.Context, target *url.URL, accessToken string, output any) error {
	if target == nil {
		return errors.New("status endpoint is not configured")
	}
	response, err := s.get(ctx, target, accessToken)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return errors.New("invalid status response")
	}
	return json.Unmarshal(body, output)
}

func (s *Service) get(ctx context.Context, target *url.URL, accessToken string) (*http.Response, error) {
	if target == nil {
		return nil, errors.New("status endpoint is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	return s.client.Do(request)
}

func endpoint(base *url.URL, resource string) *url.URL {
	if base == nil {
		return nil
	}
	target := *base
	target.Path = strings.TrimRight(target.Path, "/") + resource
	target.RawQuery = ""
	target.Fragment = ""
	return &target
}

func safeProbeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request timed out"
	}
	return "connection failed"
}
