package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultShutdownTimeout = 10 * time.Second

type Config struct {
	ServiceName        string
	Environment        string
	Address            string
	ShutdownTimeout    time.Duration
	RateLimitRPS       float64
	RateLimitBurst     int
	TrustedTimeSources []string
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
		environment = "development"
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
		if err != nil || parsed <= 0 || parsed > 100000 {
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
	if strings.HasPrefix(strings.ToLower(environment), "production") && len(timeSources) < 3 {
		return Config{}, errors.New("TRUSTED_TIME_SOURCES requires at least three independent sources in production")
	}

	return Config{
		ServiceName:        serviceName,
		Environment:        environment,
		Address:            fmt.Sprintf(":%d", port),
		ShutdownTimeout:    shutdownTimeout,
		RateLimitRPS:       rateLimitRPS,
		RateLimitBurst:     rateLimitBurst,
		TrustedTimeSources: timeSources,
	}, nil
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
