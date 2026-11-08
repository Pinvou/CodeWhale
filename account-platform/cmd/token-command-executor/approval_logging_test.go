package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func TestTokenApprovalConsumerStoppedStructuredExit(t *testing.T) {
	const childFlag = "PINVOU_TEST_APPROVAL_STOPPED_LOG_CHILD"
	const marker = "private-stopped-error-canary"
	if os.Getenv(childFlag) == "1" {
		if err := httpserver.ConfigureProcessLogging("token-command-executor"); err != nil {
			t.Fatal(err)
		}
		// Text resembling a known event must still use the hardened legacy bridge.
		log.Print("TOKEN_APPROVAL_CONSUMER_STOPPED ", marker)
		exitStoppedTokenApprovalConsumer()
		return
	}
	t.Setenv("APP_ENV", "test")
	t.Setenv("GIN_MODE", "test")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTokenApprovalConsumerStoppedStructuredExit$", "-test.count=1")
	command.Env = append(os.Environ(), childFlag+"=1")
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || ctx.Err() != nil {
		t.Fatalf("stopped consumer must exit 1: err=%v output=%s", err, output)
	}
	if bytes.Contains(output, []byte(marker)) {
		t.Fatalf("legacy error text leaked: %s", output)
	}
	lines := bytes.Split(bytes.TrimSpace(output), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected legacy and stopped records, got %s", output)
	}
	for index, expected := range []struct{ event, code string }{
		{"legacy_standard_log", "LEGACY_STANDARD_LOG"},
		{"token_approval_consumer_stopped", "CONSUMER_STOPPED"},
	} {
		var entry map[string]any
		if err := json.Unmarshal(lines[index], &entry); err != nil {
			t.Fatal(err)
		}
		if entry["event"] != expected.event || entry["msg"] != expected.event || entry["error_code"] != expected.code || entry["result"] != "failure" || entry["level"] != "ERROR" || entry["service"] != "token-command-executor" {
			t.Fatalf("incorrect structured failure: %#v", entry)
		}
	}
}
