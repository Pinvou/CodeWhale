package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

type packageExpiryScanFunc func(context.Context, int) (int, error)

func (f packageExpiryScanFunc) ExpirePackages(ctx context.Context, limit int) (int, error) {
	return f(ctx, limit)
}

func packageExpiryProductionLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	previousLogger, previousWriter, previousFlags := slog.Default(), log.Writer(), log.Flags()
	var output bytes.Buffer
	slog.SetDefault(observability.NewLogger(&output, observability.Config{Service: "billing-service"}))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return &output
}

func TestPackageExpiryWorkerProductionStagesAndRecovery(t *testing.T) {
	output := packageExpiryProductionLogger(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	calls := 0
	worker := NewPackageExpiryWorker(nil, 0)
	if worker.interval != time.Minute {
		t.Fatal("constructor changed the minimum scan interval", worker.interval)
	}
	worker.repository = packageExpiryScanFunc(func(ctx context.Context, limit int) (int, error) {
		if limit != 100 {
			t.Fatal("scan batch limit changed", limit)
		}
		calls++
		if calls < 3 {
			return 0, errors.New("controlled-private-expiry-diagnostic")
		}
		cancel()
		return 0, nil
	})
	// Only the test clock is shortened; Run uses its real ticker and callbacks.
	worker.interval = time.Millisecond
	worker.Run(ctx)
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if calls < 3 || len(lines) != 2 || strings.Contains(output.String(), "controlled-private") {
		t.Fatal("scan failed to recover, reported cancellation or leaked a diagnostic", calls, output.String())
	}
	for i, stage := range []string{"startup", "periodic"} {
		var value map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &value); err != nil || value["level"] != "ERROR" || value["component"] != "package-expiry" || value["error_code"] != "PACKAGE_EXPIRY_SCAN_FAILED" || value["stage"] != stage {
			t.Fatal("production logger lost scan-stage identity", lines[i], err)
		}
	}
}

func TestPackageExpiryWorkerCancellationIsNotFailure(t *testing.T) {
	for _, failure := range []error{context.Canceled, fmt.Errorf("controlled wrapped cancellation: %w", context.Canceled)} {
		t.Run(failure.Error(), func(t *testing.T) {
			output := packageExpiryProductionLogger(t)
			worker := NewPackageExpiryWorker(nil, time.Minute)
			worker.repository = packageExpiryScanFunc(func(context.Context, int) (int, error) { return 0, failure })
			worker.scan(context.Background(), "startup")
			if output.Len() != 0 {
				t.Fatal("normal cancellation emitted an error", output.String())
			}
		})
	}
	t.Run("context-cancelled-during-scan", func(t *testing.T) {
		output := packageExpiryProductionLogger(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		worker := NewPackageExpiryWorker(nil, time.Minute)
		worker.repository = packageExpiryScanFunc(func(context.Context, int) (int, error) {
			cancel()
			return 0, errors.New("controlled query interruption")
		})
		worker.scan(ctx, "periodic")
		if output.Len() != 0 {
			t.Fatal("shutdown interruption emitted an error", output.String())
		}
	})
}

func TestPackageExpiryWorkerActiveContextDeadlineIsFailure(t *testing.T) {
	output := packageExpiryProductionLogger(t)
	worker := NewPackageExpiryWorker(nil, time.Minute)
	worker.repository = packageExpiryScanFunc(func(context.Context, int) (int, error) { return 0, context.DeadlineExceeded })
	worker.scan(context.Background(), "periodic")
	if !strings.Contains(output.String(), `"stage":"periodic"`) || !strings.Contains(output.String(), `"level":"ERROR"`) {
		t.Fatal("dependency deadline was silently classified as shutdown", output.String())
	}
}
