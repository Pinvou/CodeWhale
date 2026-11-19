package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/redisconnection"
)

func TestCapacityInventoryHelpAndInvalidInputDoNotConnect(t *testing.T) {
	t.Setenv("REDIS_URL", "invalid-synthetic-private-URL")
	var output bytes.Buffer
	if err := runCapacityInventory([]string{"--help"}, &output); err != nil || !strings.Contains(output.String(), "--apply") {
		t.Fatal("help requires Redis configuration or omits explicit apply", err)
	}
	for _, args := range [][]string{{"--timeout=0"}, {"--timeout=31m"}, {"--unexpected=synthetic-private-input"}, {"unexpected"}} {
		output.Reset()
		if err := runCapacityInventory(args, &output); !errors.Is(err, errCapacityInventoryArguments) || output.Len() != 0 || strings.Contains(err.Error(), "synthetic-private") {
			t.Fatal("invalid arguments reached Redis or exposed input", args)
		}
	}
	if err := runCapacityInventory(nil, &output); !errors.Is(err, redisconnection.ErrConfiguration) || output.Len() != 0 {
		t.Fatal("read-only invocation did not retain safe configuration failure", err)
	}
}

func TestPortalRedisLoggerDropsLibraryArgumentsAndKeepsProductionEvent(t *testing.T) {
	var output bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(observability.NewLogger(&output, observability.Config{Service: "token-portal", Environment: "test"}))
	t.Cleanup(func() { slog.SetDefault(prior) })
	logger := &portalRedisLogger{}
	for i := 0; i < 3; i++ {
		logger.Printf(context.Background(), "synthetic-private-format %s %v", "synthetic-private-url", errors.New("synthetic-private-error"))
	}
	if strings.Contains(output.String(), "synthetic-private") || strings.Count(output.String(), "\n") != 1 {
		t.Fatal("library diagnostics leaked arguments or flooded logs")
	}
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil || entry["event"] != "portal_redis_client_event" || entry["error_code"] != "PORTAL_REDIS_CLIENT_EVENT" {
		t.Fatal("library safe diagnostic was dropped by production logger", entry, err)
	}
	now := time.Unix(1000, 0)
	clock := &portalRedisLogger{}
	if !clock.shouldReport(now) || clock.shouldReport(now.Add(time.Second)) || !clock.shouldReport(now.Add(time.Minute)) {
		t.Fatal("library diagnostic report interval is not bounded")
	}
}
