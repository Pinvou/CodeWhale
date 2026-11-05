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

type failedProductAdmissionMaintenance struct {
	calls   []string
	failure error
	limits  []int
}

func (r *failedProductAdmissionMaintenance) ExpireProductAdmissions(_ context.Context, limit int) (int, error) {
	r.calls = append(r.calls, "published-expiry")
	r.limits = append(r.limits, limit)
	return 0, r.failure
}
func (r *failedProductAdmissionMaintenance) ExpirePendingManualProductAdmissions(_ context.Context, limit int) (int, error) {
	r.calls = append(r.calls, "pending-manual-expiry")
	r.limits = append(r.limits, limit)
	return 0, r.failure
}
func (r *failedProductAdmissionMaintenance) purgeDueProductAdmissionMaterials(context.Context) (int, error) {
	r.calls = append(r.calls, "material-purge")
	return 0, r.failure
}

func productAdmissionWorkerWithProductionLogger(t *testing.T, output *bytes.Buffer) *ProductAdmissionExpiryWorker {
	t.Helper()
	previousLogger, previousWriter := slog.Default(), log.Writer()
	previousFlags := log.Flags()
	slog.SetDefault(observability.NewLogger(output, observability.Config{Service: "billing-service"}))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	// The production constructor captures the configured safe default logger.
	return NewProductAdmissionExpiryWorker(nil, time.Minute)
}

func TestProductAdmissionExpiryWorkerObservesFailuresAndContinuesNextTick(t *testing.T) {
	repository := &failedProductAdmissionMaintenance{failure: errors.New("controlled-private-diagnostic")}
	var output bytes.Buffer
	worker := productAdmissionWorkerWithProductionLogger(t, &output)
	worker.repository = repository
	worker.runOnce(context.Background())
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 || strings.Contains(output.String(), "controlled-private-diagnostic") {
		t.Fatal("missing stage failure or raw diagnostic disclosed", output.String())
	}
	for i, line := range lines {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil || value["level"] != "ERROR" || value["stage"] != repository.calls[i] || value["component"] != "product-admission-expiry" || value["error_code"] != "PRODUCT_ADMISSION_MAINTENANCE_RETRY_PENDING" {
			t.Fatal("stage failure is not independently identifiable", line, err)
		}
	}
	repository.failure = nil
	before := output.String()
	worker.runOnce(context.Background())
	if len(repository.calls) != 6 || output.String() != before {
		t.Fatal("a failed maintenance tick prevented recovery or successful calls logged failures", repository.calls, output.String())
	}
	for _, limit := range repository.limits {
		if limit != 100 {
			t.Fatal("maintenance limit changed", limit)
		}
	}
}

func TestProductAdmissionExpiryWorkerCancellationIsNotFailure(t *testing.T) {
	repository := &failedProductAdmissionMaintenance{failure: fmt.Errorf("controlled cancellation: %w", context.Canceled)}
	var output bytes.Buffer
	worker := productAdmissionWorkerWithProductionLogger(t, &output)
	worker.repository = repository
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker.Run(ctx)
	if len(repository.calls) != 3 || output.Len() != 0 {
		t.Fatal("cancelled worker changed lifecycle or reported a maintenance failure", repository.calls, output.String())
	}
}
