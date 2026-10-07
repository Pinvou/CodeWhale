package billing

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"
)

type failedScheduledBill struct {
	channels []string
	dates    []time.Time
}

func (f *failedScheduledBill) Capabilities() PaymentCapabilities {
	return PaymentCapabilities{BillSupported: true, Channels: []string{"ALIPAY"}}
}
func (f *failedScheduledBill) RunScheduledReconciliation(_ context.Context, channel string, date time.Time) (ReconciliationRun, error) {
	f.channels = append(f.channels, channel)
	f.dates = append(f.dates, date)
	return ReconciliationRun{}, errors.Join(ErrPaymentProvider, &url.Error{Op: "Get", URL: "https://controlled-private-supplier.invalid/path", Err: errors.New("controlled transport failure")})
}
func TestDailyReconciliationLogsOnlySanitizedFailureAndShanghaiDate(t *testing.T) {
	provider := &failedScheduledBill{}
	worker := NewDailyPaymentReconciliationWorker(provider)
	var log bytes.Buffer
	worker.logger = slog.New(slog.NewJSONHandler(&log, nil))
	worker.now = func() time.Time { return time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC) }
	worker.runOnce(context.Background())
	if len(provider.dates) != 1 || provider.channels[0] != "ALIPAY" || provider.dates[0].Format("2006-01-02") != "2026-10-06" {
		t.Fatal("worker lost channel or Shanghai yesterday schedule")
	}
	if strings.Contains(log.String(), "controlled-private-supplier") || !strings.Contains(log.String(), "payment provider unavailable") {
		t.Fatal("worker logged unsafe supplier error")
	}
}
