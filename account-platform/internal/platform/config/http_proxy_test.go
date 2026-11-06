package config

import (
	"strings"
	"testing"
)

func TestHTTPProxyConfigurationBoundary(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	for _, value := range []string{"edge", "10.0.0.0/8", "*", "192.0.2.1,", "fe80::1%eth0", "192.0.2.1,,192.0.2.2", "[2001:db8::1]"} {
		t.Setenv("HTTP_TRUSTED_PROXY_IPS", value)
		if _, err := Load("fixture"); err == nil {
			t.Fatalf("invalid proxy configuration accepted: %q", value)
		} else if strings.Contains(err.Error(), value) {
			t.Fatal("invalid proxy configuration echoed its input")
		}
	}
	for _, value := range []string{"", "192.0.2.1,2001:db8::1", "::ffff:192.0.2.1"} {
		t.Setenv("HTTP_TRUSTED_PROXY_IPS", value)
		if _, err := Load("fixture"); err != nil {
			t.Fatalf("valid proxy configuration rejected: %v", err)
		}
	}
}
