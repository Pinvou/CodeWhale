package identityadapter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/redisconnection"
	"github.com/redis/go-redis/v9"
)

type tokenRiskDimensions struct {
	action, source, device, identifier, session string
}

type tokenAdmission interface {
	Admit(context.Context, tokenRiskDimensions) (time.Duration, error)
	Complete(context.Context, tokenRiskDimensions, bool, string) (time.Duration, error)
}

type RedisTokenAdmission struct {
	client *redis.Client
	key    []byte
}

func NewRedisTokenAdmission(rawURL, rawKey string) (*RedisTokenAdmission, error) {
	return NewRedisTokenAdmissionWithCA(rawURL, rawKey, "")
}

func NewRedisTokenAdmissionWithCA(rawURL, rawKey, caFile string) (*RedisTokenAdmission, error) {
	options, err := redisconnection.Options(rawURL, caFile)
	if err != nil {
		return nil, errors.New("authentication risk Redis URL is invalid")
	}
	key, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(rawKey))
	if err != nil || len(key) < 32 {
		return nil, errors.New("authentication risk HMAC key must be unpadded Base64 with at least 32 bytes")
	}
	return &RedisTokenAdmission{client: redis.NewClient(options), key: key}, nil
}

var admissionScript = redis.NewScript(`
local retry = 0
for i = 1, #KEYS do
  local value = redis.call('INCR', KEYS[i])
  local window = tonumber(ARGV[(i-1)*2+2])
  if value == 1 then redis.call('EXPIRE', KEYS[i], window) end
  if value > tonumber(ARGV[(i-1)*2+1]) then retry = math.max(retry, redis.call('TTL', KEYS[i])) end
end
return retry`)

func (a *RedisTokenAdmission) digest(value string) string {
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:18])
}

func (a *RedisTokenAdmission) Admit(ctx context.Context, d tokenRiskDimensions) (time.Duration, error) {
	specs := []struct {
		kind, value   string
		limit, window int
	}{
		{"source-minute", d.source, 20, 60}, {"source-day", d.source, 200, 86400}, {"device-minute", d.device, 10, 60},
		{"session-minute", d.session, 30, 60}, {"provider-minute", "keycloak", 1000, 60},
	}
	keys, args := make([]string, 0, len(specs)), make([]any, 0, len(specs)*2)
	for _, spec := range specs {
		if strings.TrimSpace(spec.value) == "" {
			continue
		}
		keys = append(keys, "pinvou:auth-admission:"+d.action+":"+spec.kind+":"+a.digest(spec.value))
		args = append(args, spec.limit, spec.window)
	}
	retry, err := admissionScript.Run(ctx, a.client, keys, args...).Int64()
	if err != nil {
		return 0, err
	}
	if retry > 0 {
		return time.Duration(max(retry, 1)) * time.Second, nil
	}
	if d.action == "password" {
		identifierKey := "pinvou:auth-failure:password:" + a.digest(d.identifier)
		identifierCount, err := a.client.Get(ctx, identifierKey).Int64()
		if err != nil && !errors.Is(err, redis.Nil) {
			return 0, err
		}
		if identifierCount >= 5 {
			ttl, ttlErr := a.client.TTL(ctx, identifierKey).Result()
			if ttlErr != nil {
				return 0, ttlErr
			}
			return max(ttl, time.Second), nil
		}
		stableDigest, err := a.client.Get(ctx, "pinvou:auth-alias:"+a.digest(d.identifier)).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return 0, err
		}
		if stableDigest != "" {
			stableKey := "pinvou:auth-failure:user:" + stableDigest
			stableCount, countErr := a.client.Get(ctx, stableKey).Int64()
			if countErr != nil && !errors.Is(countErr, redis.Nil) {
				return 0, countErr
			}
			if stableCount >= 10 {
				ttl, ttlErr := a.client.TTL(ctx, stableKey).Result()
				if ttlErr != nil {
					return 0, ttlErr
				}
				return max(ttl, time.Second), nil
			}
		}
	}
	return 0, nil
}

func (a *RedisTokenAdmission) Complete(ctx context.Context, d tokenRiskDimensions, success bool, stableUser string) (time.Duration, error) {
	if success {
		if d.action == "password" && stableUser != "" {
			return 0, a.client.Set(ctx, "pinvou:auth-alias:"+a.digest(d.identifier), a.digest(stableUser), 0).Err()
		}
		return 0, nil
	}
	failureDimension := d.identifier
	if d.action == "refresh" {
		failureDimension = d.session
	}
	key := "pinvou:auth-failure:" + d.action + ":" + a.digest(failureDimension)
	count, err := incrementFailure(ctx, a.client, key)
	if err != nil {
		return 0, err
	}
	stableDigest, aliasErr := a.client.Get(ctx, "pinvou:auth-alias:"+a.digest(d.identifier)).Result()
	if aliasErr != nil && !errors.Is(aliasErr, redis.Nil) {
		return 0, aliasErr
	}
	if stableDigest != "" {
		if _, err := incrementFailure(ctx, a.client, "pinvou:auth-failure:user:"+stableDigest); err != nil {
			return 0, err
		}
	}
	delay := time.Duration(1<<min(count-1, 5)) * time.Second
	return min(delay, 30*time.Second), nil
}

func incrementFailure(ctx context.Context, client *redis.Client, key string) (int64, error) {
	count, err := client.Incr(ctx, key).Result()
	if err == nil && count == 1 {
		err = client.Expire(ctx, key, 15*time.Minute).Err()
	}
	return count, err
}

type TokenProxy struct {
	endpoint  string
	client    *http.Client
	admission tokenAdmission
	now       func() time.Time
	floor     time.Duration
}

const maxUpstreamRetryAfterSeconds = 300

func NewTokenProxy(endpoint string, admission tokenAdmission) (*TokenProxy, error) {
	return NewTokenProxyWithTrust(endpoint, admission, "", false)
}

// NewTokenProxyWithTrust shares only Keycloak trust, never provisioner credentials.
func NewTokenProxyWithTrust(endpoint string, admission tokenAdmission, caFile string, production bool) (*TokenProxy, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || admission == nil {
		return nil, errors.New("token proxy is not fully configured")
	}
	client, err := keycloakHTTPClient(parsed.String(), caFile, production, 15*time.Second)
	if err != nil {
		return nil, err
	}
	return &TokenProxy{endpoint: parsed.String(), client: client, admission: admission, now: time.Now, floor: 750 * time.Millisecond}, nil
}

func (p *TokenProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := p.now()
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", 0)
		return
	}
	grant := strings.TrimSpace(r.Form.Get("grant_type"))
	action := map[string]string{"password": "password", "refresh_token": "refresh"}[grant]
	if action == "" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", 0)
		return
	}
	d := tokenRiskDimensions{action: action, source: tokenClientSource(r), device: boundedDimension(r.Header.Get("X-Device-ID"), "device:absent"), identifier: strings.ToLower(strings.TrimSpace(r.Form.Get("username")))}
	if grant == "refresh_token" {
		d.session = r.Form.Get("refresh_token")
		d.identifier = "refresh-session"
	}
	retry, err := p.admission.Admit(r.Context(), d)
	if err != nil {
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", 1)
		return
	}
	if retry > 0 {
		waitTokenFloor(r.Context(), started, p.floor)
		writeOAuthError(w, http.StatusTooManyRequests, "temporarily_unavailable", int(retry.Seconds()))
		return
	}
	request, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, p.endpoint, strings.NewReader(r.Form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Pinvou-Client-Source", d.source)
	// Keycloak resolves risk source through its exact trusted-proxy policy.
	// Send only the admitted source, never the caller's forwarding chain.
	request.Header.Set("X-Forwarded-For", d.source)
	request.Header.Set("X-Device-ID", d.device)
	response, err := p.client.Do(request)
	if err != nil {
		waitTokenFloor(r.Context(), started, p.floor)
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", 1)
		return
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", 1)
		return
	}
	success := response.StatusCode >= 200 && response.StatusCode < 300
	upstreamError := tokenOAuthError(body)
	if !success && (upstreamError == "invalid_client" || upstreamError == "unauthorized_client") {
		waitTokenFloor(r.Context(), started, p.floor)
		writeOAuthError(w, http.StatusUnauthorized, "unauthorized_client", 0)
		return
	}
	if !success && response.StatusCode == http.StatusTooManyRequests {
		waitTokenFloor(r.Context(), started, p.floor)
		writeOAuthError(w, http.StatusTooManyRequests, "temporarily_unavailable", upstreamRetryAfter(response.Header.Get("Retry-After"), started))
		return
	}
	if !success && (response.StatusCode >= 500 || upstreamError == "temporarily_unavailable") {
		waitTokenFloor(r.Context(), started, p.floor)
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", 1)
		return
	}
	delay, completeErr := p.admission.Complete(r.Context(), d, success, tokenSubject(body))
	if completeErr != nil && !success {
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", 1)
		return
	}
	waitTokenFloor(r.Context(), started, max(p.floor, delay))
	if !success {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_grant", 0)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}

func tokenOAuthError(body []byte) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Error)
}

func upstreamRetryAfter(raw string, now time.Time) int {
	raw = strings.TrimSpace(raw)
	if seconds, err := strconv.Atoi(raw); err == nil {
		return min(max(seconds, 1), maxUpstreamRetryAfterSeconds)
	}
	if deadline, err := http.ParseTime(raw); err == nil {
		remaining := deadline.Sub(now)
		if remaining <= 0 {
			return 1
		}
		if remaining >= maxUpstreamRetryAfterSeconds*time.Second {
			return maxUpstreamRetryAfterSeconds
		}
		seconds := int(remaining / time.Second)
		if remaining%time.Second != 0 {
			seconds++
		}
		return max(seconds, 1)
	}
	return 1
}

func tokenSubject(body []byte) string {
	var envelope struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	parts := strings.Split(envelope.AccessToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return boundedDimension(claims.Subject, "")
}

func tokenClientSource(r *http.Request) string {
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for index := len(forwarded) - 1; index >= 0; index-- {
		if value := strings.TrimSpace(forwarded[index]); net.ParseIP(value) != nil {
			return value
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return boundedDimension(r.RemoteAddr, "source:unknown")
}

func boundedDimension(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fallback
	}
	return value
}

func waitTokenFloor(ctx context.Context, started time.Time, floor time.Duration) {
	remaining := floor - time.Since(started)
	if remaining <= 0 {
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func writeOAuthError(w http.ResponseWriter, status int, code string, retry int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func (p *TokenProxy) Ping(ctx context.Context) error {
	if admission, ok := p.admission.(*RedisTokenAdmission); ok {
		return admission.client.Ping(ctx).Err()
	}
	return fmt.Errorf("token admission readiness is unavailable")
}
