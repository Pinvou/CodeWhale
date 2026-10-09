package config

import (
	"testing"
	"time"
)

func TestLoadUsesSafeDefaults(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("APP_ENV", "development")
	t.Setenv("SHUTDOWN_TIMEOUT", "")
	t.Setenv("RATE_LIMIT_RPS", "")
	t.Setenv("RATE_LIMIT_BURST", "")
	t.Setenv("TRUSTED_TIME_SOURCES", "")

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

func TestLoadRequiresThreeTrustedTimeSourcesInProduction(t *testing.T) {
	t.Setenv("APP_ENV", "production-new")
	t.Setenv("TRUSTED_TIME_SOURCES", "time-a.example:123,time-b.example:123")
	if _, err := Load("test"); err == nil {
		t.Fatal("production accepted fewer than three trusted time sources")
	}
	t.Setenv("TRUSTED_TIME_SOURCES", "time-a.example:123,time-b.example:123,time-c.example:123")
	if cfg, err := Load("test"); err != nil || len(cfg.TrustedTimeSources) != 3 {
		t.Fatalf("production time sources = %v, %v", cfg.TrustedTimeSources, err)
	}
}

func TestLoadRejectsInvalidRateLimit(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	for name, values := range map[string][2]string{
		"zero rate":              {"0", "10"},
		"zero burst":             {"10", "0"},
		"invalid rate":           {"fast", "10"},
		"NaN rate":               {"NaN", "10"},
		"positive infinity rate": {"+Inf", "10"},
		"negative infinity rate": {"-Inf", "10"},
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
	t.Setenv("APP_ENV", "test")
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

func TestLoadRequiresExplicitEnvironment(t *testing.T) {
	for _, environment := range []string{"", " \t\n"} {
		t.Setenv("APP_ENV", environment)
		if _, err := Load("account-service"); err == nil {
			t.Fatal("missing environment silently selected a development configuration")
		}
	}
}
