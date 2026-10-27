package config

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultShutdownTimeout = 10 * time.Second

// IsProduction keeps security gates consistent for production environment variants.
func IsProduction(environment string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(environment)), "production")
}

type Config struct {
	ServiceName         string
	Environment         string
	Address             string
	ShutdownTimeout     time.Duration
	RateLimitRPS        float64
	RateLimitBurst      int
	HTTPTrustedProxyIPs []netip.Addr
	TrustedTimeSources  []string
}

func Load(serviceName string) (Config, error) {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		return Config{}, errors.New("service name is required")
	}

	port := 8080
	if raw := strings.TrimSpace(os.Getenv("PORT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 65535 {
			return Config{}, fmt.Errorf("invalid PORT %q", raw)
		}
		port = parsed
	}

	environment := strings.TrimSpace(os.Getenv("APP_ENV"))
	if environment == "" {
		return Config{}, errors.New("APP_ENV must be explicitly configured")
	}

	shutdownTimeout := defaultShutdownTimeout
	if raw := strings.TrimSpace(os.Getenv("SHUTDOWN_TIMEOUT")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("invalid SHUTDOWN_TIMEOUT %q", raw)
		}
		shutdownTimeout = parsed
	}
	rateLimitRPS := 100.0
	if raw := strings.TrimSpace(os.Getenv("RATE_LIMIT_RPS")); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 || parsed > 100000 {
			return Config{}, fmt.Errorf("invalid RATE_LIMIT_RPS %q", raw)
		}
		rateLimitRPS = parsed
	}
	rateLimitBurst := 200
	if raw := strings.TrimSpace(os.Getenv("RATE_LIMIT_BURST")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000000 {
			return Config{}, fmt.Errorf("invalid RATE_LIMIT_BURST %q", raw)
		}
		rateLimitBurst = parsed
	}
	timeSources := splitUnique(os.Getenv("TRUSTED_TIME_SOURCES"))
	trustedProxies, err := ParseHTTPTrustedProxyIPs(os.Getenv("HTTP_TRUSTED_PROXY_IPS"))
	if err != nil {
		return Config{}, err
	}
	if IsProduction(environment) && len(timeSources) < 3 {
		return Config{}, errors.New("TRUSTED_TIME_SOURCES requires at least three independent sources in production")
	}

	return Config{
		ServiceName:         serviceName,
		Environment:         environment,
		Address:             fmt.Sprintf(":%d", port),
		ShutdownTimeout:     shutdownTimeout,
		RateLimitRPS:        rateLimitRPS,
		RateLimitBurst:      rateLimitBurst,
		HTTPTrustedProxyIPs: trustedProxies,
		TrustedTimeSources:  timeSources,
	}, nil
}

// ParseHTTPTrustedProxyIPs trusts only explicitly configured immediate peers.
// It does not accept networks, zones, names or empty entries in a nonempty list.
func ParseHTTPTrustedProxyIPs(raw string) ([]netip.Addr, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var peers []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, item := range strings.Split(raw, ",") {
		peer, err := netip.ParseAddr(strings.TrimSpace(item))
		if err != nil || peer.Zone() != "" {
			return nil, errors.New("HTTP_TRUSTED_PROXY_IPS requires comma-separated exact IP addresses")
		}
		peer = peer.Unmap()
		if !seen[peer] {
			peers = append(peers, peer)
			seen[peer] = true
		}
	}
	return peers, nil
}

func splitUnique(raw string) []string {
	seen := map[string]bool{}
	values := []string{}
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			values = append(values, value)
		}
	}
	return values
}
