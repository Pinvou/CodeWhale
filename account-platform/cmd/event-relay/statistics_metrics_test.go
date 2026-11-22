package main

import (
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

func TestOutboxStatisticsFailurePreservesEvidenceAndRecovers(t *testing.T) {
	m := newRelayMetrics("billing")
	read := func(name string) float64 {
		t.Helper()
		families, err := m.registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() == name {
				if len(family.Metric) != 1 {
					t.Fatal("unexpected metric cardinality")
				}
				metric := family.Metric[0]
				if metric.Gauge != nil {
					return metric.Gauge.GetValue()
				}
				return metric.Counter.GetValue()
			}
		}
		t.Fatalf("missing real registry metric %s", name)
		return 0
	}
	if read("pinvou_outbox_statistics_available") != 0 {
		t.Fatal("startup was reported as a successful query")
	}
	m.record(events.OutboxStats{Pending: 7, DeadLettered: 2, OldestPendingAge: 3 * time.Minute, RetainedStorageBytes: 12345})
	last := read("pinvou_outbox_statistics_last_success_timestamp_seconds")
	if last <= 0 || read("pinvou_outbox_statistics_available") != 1 {
		t.Fatal("successful query missing freshness")
	}
	m.recordStatsError()
	if read("pinvou_outbox_statistics_available") != 0 || read("pinvou_outbox_statistics_errors_total") != 1 {
		t.Fatal("failed query was hidden")
	}
	if read("pinvou_outbox_pending_events") != 7 || read("pinvou_outbox_dead_lettered_events") != 2 || read("pinvou_outbox_oldest_pending_age_seconds") != 180 || read("pinvou_outbox_retained_storage_bytes") != 12345 || read("pinvou_outbox_statistics_last_success_timestamp_seconds") != last {
		t.Fatal("query failure replaced last known facts with empty data")
	}
	m.record(events.OutboxStats{Pending: 4, RetainedStorageBytes: 15000})
	if read("pinvou_outbox_statistics_available") != 1 || read("pinvou_outbox_pending_events") != 4 || read("pinvou_outbox_dead_lettered_events") != 0 || read("pinvou_outbox_retained_storage_bytes") != 15000 || read("pinvou_outbox_statistics_errors_total") != 1 {
		t.Fatal("next successful query failed to recover")
	}
}
