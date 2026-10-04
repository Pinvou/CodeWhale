package config

import (
	"testing"
	"time"
)

func TestLoadUsesSafeDefaults(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("APP_ENV", "")
	t.Setenv("SHUTDOWN_TIMEOUT", "")
	t.Setenv("RATE_LIMIT_RPS", "")
	t.Setenv("RATE_LIMIT_BURST", "")

	cfg, err := Load("account-service")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Address != ":8080" {
		t.Fatalf("Address = %q, want :8080", cfg.Address)
	}
	if cfg.Environment != "development" {
		t.Fatalf("Environment = %q, want development", cfg.Environment)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want 10s", cfg.ShutdownTimeout)
	}
	if cfg.RateLimitRPS != 100 || cfg.RateLimitBurst != 200 {
		t.Fatalf("rate limit = %v/%d, want 100/200", cfg.RateLimitRPS, cfg.RateLimitBurst)
	}
}

func TestLoadRejectsInvalidRateLimit(t *testing.T) {
	for name, values := range map[string][2]string{
		"zero rate":     {"0", "10"},
		"zero burst":    {"10", "0"},
		"invalid rate":  {"fast", "10"},
		"nan rate":      {"NaN", "10"},
		"infinite rate": {"+Inf", "10"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PORT", "")
			t.Setenv("SHUTDOWN_TIMEOUT", "")
			t.Setenv("RATE_LIMIT_RPS", values[0])
			t.Setenv("RATE_LIMIT_BURST", values[1])
			if _, err := Load("test"); err == nil {
				t.Fatal("invalid rate limit was accepted")
			}
		})
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		port    string
		timeout string
	}{
		{name: "nonnumeric port", port: "nope"},
		{name: "port too large", port: "65536"},
		{name: "negative timeout", port: "8080", timeout: "-1s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PORT", tt.port)
			t.Setenv("SHUTDOWN_TIMEOUT", tt.timeout)
			if _, err := Load("test-service"); err == nil {
				t.Fatal("Load succeeded, want error")
			}
		})
	}
}

func TestLoadRejectsEmptyServiceName(t *testing.T) {
	if _, err := Load("  "); err == nil {
		t.Fatal("Load succeeded, want error")
	}
}
