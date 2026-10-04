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
	ServiceName     string
	Environment     string
	Address         string
	ShutdownTimeout time.Duration
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

	return Config{
		ServiceName:     serviceName,
		Environment:     environment,
		Address:         fmt.Sprintf(":%d", port),
		ShutdownTimeout: shutdownTimeout,
	}, nil
}
