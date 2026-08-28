package main

import (
	"testing"
	"time"
)

func TestTestAdmissionEvidenceTTLFromEnvironment(t *testing.T) {
	t.Run("defaults to thirty days", func(t *testing.T) {
		t.Setenv("BILLING_TEST_ADMISSION_EVIDENCE_TTL", "")
		got, err := testAdmissionEvidenceTTLFromEnvironment()
		if err != nil {
			t.Fatal(err)
		}
		if want := 30 * 24 * time.Hour; got != want {
			t.Fatalf("ttl = %s, want %s", got, want)
		}
	})

	t.Run("accepts configured duration", func(t *testing.T) {
		t.Setenv("BILLING_TEST_ADMISSION_EVIDENCE_TTL", "720h")
		got, err := testAdmissionEvidenceTTLFromEnvironment()
		if err != nil {
			t.Fatal(err)
		}
		if want := 720 * time.Hour; got != want {
			t.Fatalf("ttl = %s, want %s", got, want)
		}
	})

	for _, invalid := range []string{"invalid", "59m", "2161h"} {
		t.Run("rejects_"+invalid, func(t *testing.T) {
			t.Setenv("BILLING_TEST_ADMISSION_EVIDENCE_TTL", invalid)
			if _, err := testAdmissionEvidenceTTLFromEnvironment(); err == nil {
				t.Fatalf("expected %q to be rejected", invalid)
			}
		})
	}
}
