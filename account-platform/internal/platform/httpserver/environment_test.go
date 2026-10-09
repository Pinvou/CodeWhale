package httpserver

import "testing"

func TestProcessLoggingRequiresExplicitEnvironment(t *testing.T) {
	for _, environment := range []string{"", " \t\n"} {
		t.Setenv("APP_ENV", environment)
		if err := ConfigureProcessLogging("fixture-service"); err == nil {
			t.Fatal("missing environment silently initialized development logging")
		}
	}
}
