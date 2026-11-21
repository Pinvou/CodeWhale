package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/redisconnection"
)

type inventoryServerError string

func (e inventoryServerError) Error() string { return string(e) }
func (inventoryServerError) RedisError()     {}

func TestInventoryDiagnosticsClassifyWithoutPrivateErrorText(t *testing.T) {
	const marker = "synthetic-private-connection-or-ticket"
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"operations", ErrCapacityInventoryRequired, "PORTAL_INVENTORY_OPERATIONS_REQUIRED"},
		{"preparing", ErrCapacityPreparing, "PORTAL_INVENTORY_PREPARING"},
		{"deadline", context.DeadlineExceeded, "PORTAL_INVENTORY_TIMEOUT"},
		{"canceled", context.Canceled, "PORTAL_INVENTORY_CANCELED"},
		{"configuration", redisconnection.ErrConfiguration, "PORTAL_INVENTORY_CONFIGURATION_INVALID"},
		{"ttl", inventoryServerError("ERR user_script:12: PORTAL_TRANSIENT_TTL_REQUIRED " + marker), "PORTAL_TRANSIENT_TTL_REQUIRED"},
		{"state", inventoryServerError("ERR user_script:5: PORTAL_ADMISSION_STATE_INVALID " + marker), "PORTAL_ADMISSION_STATE_INVALID"},
		{"page", inventoryServerError("ERR user_script:20: PORTAL_INVENTORY_PAGE_TOO_LARGE " + marker), "PORTAL_INVENTORY_PAGE_TOO_LARGE"},
		{"commands", inventoryServerError("ERR user_script:3: PORTAL_ADMISSION_COMMANDS_REQUIRED " + marker), "PORTAL_ADMISSION_COMMANDS_REQUIRED"},
		{"acl", inventoryServerError("NOPERM " + marker), "PORTAL_ADMISSION_COMMANDS_REQUIRED"},
		{"auth", inventoryServerError("WRONGPASS " + marker), "PORTAL_INVENTORY_AUTHENTICATION_FAILED"},
		{"noauth", inventoryServerError("NOAUTH " + marker), "PORTAL_INVENTORY_AUTHENTICATION_FAILED"},
		{"oom", inventoryServerError("OOM " + marker), "PORTAL_INVENTORY_REDIS_OOM"},
		{"wrongtype", inventoryServerError("WRONGTYPE " + marker), "PORTAL_ADMISSION_STATE_INVALID"},
		{"network", &net.OpError{Op: "dial", Err: errors.New(marker)}, "PORTAL_INVENTORY_REDIS_UNAVAILABLE"},
		{"unknown-server", inventoryServerError(marker), "PORTAL_CAPACITY_INVENTORY_FAILED"},
		{"unknown", errors.New(marker), "PORTAL_CAPACITY_INVENTORY_FAILED"},
		{"non-server-token", errors.New(marker + " PORTAL_TRANSIENT_TTL_REQUIRED"), "PORTAL_CAPACITY_INVENTORY_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &capacityInventoryError{kind: handoffTransient, stage: "prepare", err: fmt.Errorf("%s: %w", marker, tc.err)}
			d := DescribeCapacityInventoryError(err)
			if d.Code != tc.code || d.Component != "session-handoff" || d.Stage != "prepare" || strings.Contains(fmt.Sprint(d), marker) || strings.Contains(err.Error(), marker) || !errors.Is(err, tc.err) {
				t.Fatal("diagnostic lost safe cause/context or original error identity", d)
			}
		})
	}
}

func TestInventoryFailureLoggingIsBoundedAndResetsAfterRecovery(t *testing.T) {
	var output bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(observability.NewLogger(&output, observability.Config{Service: "token-portal", Environment: "test"}))
	t.Cleanup(func() { slog.SetDefault(prior) })
	ctx, now := context.Background(), time.Unix(1000, 0)
	var reported inventoryFailureReporter
	err := &capacityInventoryError{kind: flowTransient, stage: "prepare", err: inventoryServerError("ERR user_script:3: PORTAL_TRANSIENT_TTL_REQUIRED synthetic-private-marker")}
	other := &capacityInventoryError{kind: handoffTransient, stage: "inspect", err: ErrCapacityInventoryRequired}
	reported.report(ctx, err, now)
	reported.report(ctx, err, now.Add(time.Second))
	reported.report(ctx, other, now.Add(2*time.Second))
	reported.report(ctx, err, now.Add(3*time.Second)) // Alternating failures must not defeat deduplication.
	reported.report(ctx, err, now.Add(time.Minute))
	reported.report(ctx, nil, now.Add(61*time.Second))
	reported.report(ctx, err, now.Add(62*time.Second)) // Recovery followed by a new incident is visible immediately.
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 || strings.Contains(output.String(), "synthetic-private") || len(reported) != 1 {
		t.Fatal("repeated failure logging was unbounded or recovery/private handling failed", len(lines), len(reported))
	}
	for i, line := range lines {
		var entry map[string]any
		if e := json.Unmarshal([]byte(line), &entry); e != nil {
			t.Fatal(e)
		}
		code, component, stage := "PORTAL_TRANSIENT_TTL_REQUIRED", "flow", "prepare"
		if i == 1 {
			code, component, stage = "PORTAL_INVENTORY_OPERATIONS_REQUIRED", "session-handoff", "inspect"
		}
		if entry["event"] != "portal_capacity_inventory_failed" || entry["error_code"] != code || entry["component"] != component || entry["stage"] != stage {
			t.Fatal("production logger dropped actionable diagnostic", entry)
		}
	}
}

func TestTransientInventoryFailurePreservesKindAndFollowingPass(t *testing.T) {
	root := errors.New("synthetic-private-backend")
	first := true
	prepare := func(_ context.Context, kind transientKind) error {
		if first && kind == handoffTransient {
			first = false
			return root
		}
		return nil
	}
	ready, err := transientInventoryPass(context.Background(), prepare)
	d := DescribeCapacityInventoryError(err)
	if ready || !errors.Is(err, root) || d.Component != "session-handoff" || d.Stage != "prepare" {
		t.Fatal("actual pass lost failure context", ready, d)
	}
	if ready, err = transientInventoryPass(context.Background(), prepare); !ready || err != nil {
		t.Fatal("next pass did not recover", ready, err)
	}
}
