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
	if len(provider.dates) != 7 || provider.channels[0] != "ALIPAY" || provider.dates[0].Format("2006-01-02") != "2026-10-06" {
		t.Fatal("worker lost channel or Shanghai yesterday schedule")
	}
	if strings.Contains(log.String(), "controlled-private-supplier") || !strings.Contains(log.String(), "payment provider unavailable") {
		t.Fatal("worker logged unsafe supplier error")
	}
}

type fileOnlyRecovery struct {
	recoveries int
	bills      int
}

func (f *fileOnlyRecovery) Capabilities() PaymentCapabilities {
	return PaymentCapabilities{BillFileImportSupported: true, Channels: []string{"ALIPAY"}}
}
func (f *fileOnlyRecovery) recoverReconciliationRuns(context.Context) error {
	f.recoveries++
	return nil
}
func (f *fileOnlyRecovery) RunScheduledReconciliation(context.Context, string, time.Time) (ReconciliationRun, error) {
	f.bills++
	return ReconciliationRun{}, ErrConflict
}
func TestFileOnlyReconciliationWorkerRecoversWithoutBillCalls(t *testing.T) {
	f := &fileOnlyRecovery{}
	w := NewDailyPaymentReconciliationWorker(f)
	w.runOnce(context.Background())
	if f.recoveries != 1 || f.bills != 0 {
		t.Fatalf("file-only recovery=%d unsupported bill=%d", f.recoveries, f.bills)
	}
}

func TestReconciliationHourlyObservationsAndSevenDayLookback(t *testing.T) {
	p := &failedScheduledBill{}
	w := NewDailyPaymentReconciliationWorker(p)
	w.logger = slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	now := time.Date(2026, 10, 8, 0, 1, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	w.runOnce(context.Background())
	if len(p.dates) != reconciliationLookbackDays {
		t.Fatal("lookback missing", len(p.dates))
	}
	for index, date := range p.dates {
		if date != reconciliationBusinessDate(now).AddDate(0, 0, -index) {
			t.Fatal("wrong completed Shanghai day", date)
		}
	}
	now = now.Add(time.Minute)
	w.runOnce(context.Background())
	if len(p.dates) != 7 {
		t.Fatal("minute recovery repeated supplier bill")
	}
	now = now.Add(time.Hour)
	w.runOnce(context.Background())
	if len(p.dates) != 14 {
		t.Fatal("hourly late-bill observation absent")
	}
}
