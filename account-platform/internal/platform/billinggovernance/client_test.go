package billinggovernance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func compatibleSnapshot() Snapshot {
	commands := []string{
		"CHARGEBACK_FINANCIAL_REPAIR", "ENTITLEMENT_COMPONENT_ADJUSTMENT", "LEDGER_REPAIR",
		"PAYMENT_FINANCIAL_REPAIR", "PAYMENT_PRODUCT_RESUME", "PAYMENT_PROVIDER_RESUME",
		"RECEIVABLE_WRITE_OFF", "RECONCILIATION_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR",
		"USAGE_SETTLEMENT_REPAIR", "WALLET_ADJUSTMENT",
	}
	value := Snapshot{SchemaVersion: 1}
	for _, command := range commands {
		activation := Activation{CommandType: command, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: time.Now().UTC().Add(-time.Second)}
		switch command {
		case "PAYMENT_PROVIDER_RESUME":
			activation.PolicyVersion = 2
			activation.State = "ACTIVE"
		case "PAYMENT_PRODUCT_RESUME", "WALLET_ADJUSTMENT":
			activation.State = "ACTIVE"
		}
		value.Activations = append(value.Activations, activation)
	}
	return value
}

func encodeSnapshot(t *testing.T, value Snapshot) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mutateSnapshot(value Snapshot, apply func(*Snapshot)) Snapshot {
	apply(&value)
	return value
}

func TestActivationClientClosedAuthoritativeRead(t *testing.T) {
	if compatibleSnapshot().Activations[1].CommandType != "ENTITLEMENT_COMPONENT_ADJUSTMENT" {
		t.Fatal("versioned cases must target the entitlement correction command")
	}
	valid := encodeSnapshot(t, compatibleSnapshot())
	tests := []struct {
		name, body, cache string
		status            int
		success           bool
	}{
		{"current", valid, "no-store", 200, true},
		{"existing inactive", strings.ReplaceAll(valid, `"state":"ACTIVE"`, `"state":"INACTIVE"`), "no-store", 200, true},
		{"versioned entitlement v2", encodeSnapshot(t, mutateSnapshot(compatibleSnapshot(), func(value *Snapshot) {
			value.Activations[1].PolicyVersion, value.Activations[1].SnapshotSchemaVersion = 2, 2
		})), "no-store", 200, true},
		{"mixed entitlement version", encodeSnapshot(t, mutateSnapshot(compatibleSnapshot(), func(value *Snapshot) {
			value.Activations[1].PolicyVersion = 2
		})), "no-store", 200, false},
		{"unknown entitlement version", encodeSnapshot(t, mutateSnapshot(compatibleSnapshot(), func(value *Snapshot) {
			value.Activations[1].PolicyVersion, value.Activations[1].SnapshotSchemaVersion = 3, 3
		})), "no-store", 200, false},
		{"activated entitlement v2", encodeSnapshot(t, mutateSnapshot(compatibleSnapshot(), func(value *Snapshot) {
			value.Activations[1].PolicyVersion, value.Activations[1].SnapshotSchemaVersion = 2, 2
			value.Activations[1].State = "ACTIVE"
		})), "no-store", 200, true},
		{"activated entitlement v1 history", encodeSnapshot(t, mutateSnapshot(compatibleSnapshot(), func(value *Snapshot) {
			value.Activations[1].State = "ACTIVE"
		})), "no-store", 200, false},
		{"duplicate envelope key", strings.Replace(valid, `"schema_version":1`, `"schema_version":2,"schema_version":1`, 1), "no-store", 200, false},
		{"envelope case alias", strings.Replace(valid, `"schema_version"`, `"Schema_Version"`, 1), "no-store", 200, false},
		{"missing envelope key", strings.Replace(valid, `"schema_version":1,`, ``, 1), "no-store", 200, false},
		{"future wire", strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1), "no-store", 200, false},
		{"unknown envelope field", strings.Replace(valid, `{`, `{"extra":true,`, 1), "no-store", 200, false},
		{"null activations", `{"schema_version":1,"activations":null}`, "no-store", 200, false},
		{"empty activations", `{"schema_version":1,"activations":[]}`, "no-store", 200, false},
		{"duplicate row key", strings.Replace(valid, `"version":1`, `"version":0,"version":1`, 1), "no-store", 200, false},
		{"unknown row key", strings.Replace(valid, `"version":1`, `"version":1,"extra":true`, 1), "no-store", 200, false},
		{"missing row key", strings.Replace(valid, `"version":1,`, ``, 1), "no-store", 200, false},
		{"null row key", strings.Replace(valid, `"version":1`, `"version":null`, 1), "no-store", 200, false},
		{"row key case alias", strings.Replace(valid, `"version":1`, `"Version":1`, 1), "no-store", 200, false},
		{"integer overflow", strings.Replace(valid, `"version":1`, `"version":9223372036854775808`, 1), "no-store", 200, false},
		{"fractional version", strings.Replace(valid, `"version":1`, `"version":1.5`, 1), "no-store", 200, false},
		{"string version", strings.Replace(valid, `"version":1`, `"version":"1"`, 1), "no-store", 200, false},
		{"unknown state", strings.Replace(valid, `"state":"INACTIVE"`, `"state":"FUTURE"`, 1), "no-store", 200, false},
		// USAGE_SETTLEMENT_REPAIR is activatable only at v1/s1, so a future version
		// reported ACTIVE must stay rejected.
		{"future version active", encodeSnapshot(t, mutateSnapshot(compatibleSnapshot(), func(value *Snapshot) {
			for index := range value.Activations {
				if value.Activations[index].CommandType == "USAGE_SETTLEMENT_REPAIR" {
					value.Activations[index].State = "ACTIVE"
					value.Activations[index].PolicyVersion, value.Activations[index].SnapshotSchemaVersion = 2, 2
				}
			}
		})), "no-store", 200, false},
		{"unknown command", strings.Replace(valid, `CHARGEBACK_FINANCIAL_REPAIR`, `FUTURE_COMMAND`, 1), "no-store", 200, false},
		{"invalid utf8", strings.Replace(valid, `CHARGEBACK_FINANCIAL_REPAIR`, string([]byte{0xff}), 1), "no-store", 200, false},
		{"trailing object", valid + `{}`, "no-store", 200, false},
		{"truncated", valid[:len(valid)-1], "no-store", 200, false},
		{"cacheable", valid, "max-age=30", 200, false},
		{"missing no-store", valid, "", 200, false},
		{"old Billing without reader", valid, "no-store", 404, false},
		{"unavailable", `private upstream details`, "no-store", 503, false},
		{"unauthorized", valid, "no-store", 401, false},
		{"no content", "", "no-store", 204, false},
		{"body limit inclusive", valid + strings.Repeat(" ", maxResponseBytes-len(valid)), "no-store", 200, true},
		{"body limit exceeded", valid + strings.Repeat(" ", maxResponseBytes-len(valid)+1), "no-store", 200, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/billing/internal/v1/approval-command-activations" || r.Header.Get("Authorization") != "Bearer test-workload-token" || r.Header.Get("Cache-Control") != "no-cache" {
					t.Error("activation request does not use the exact internal read contract")
				}
				w.Header().Set("Cache-Control", tc.cache)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewClient(server.URL+"/billing/", "test-workload-token")
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.Snapshot(context.Background())
			if (err == nil) != tc.success {
				t.Fatalf("success=%v error=%v", tc.success, err)
			}
			if tc.success {
				if len(got.Activations) != 11 || got.SchemaVersion != 1 {
					t.Fatal("valid read lost the complete compatibility projection")
				}
			} else if !errors.Is(err, ErrUnavailable) || got.SchemaVersion != 0 || len(got.Activations) != 0 {
				t.Fatal("failed read returned partial data or unsafe error")
			}
		})
	}
}

func TestActivationSnapshotCompatibilityAndVersionBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"missing row", func(s *Snapshot) { s.Activations = s.Activations[:10] }},
		{"extra row", func(s *Snapshot) { s.Activations = append(s.Activations, s.Activations[10]) }},
		{"duplicate row", func(s *Snapshot) { s.Activations[1] = s.Activations[0] }},
		{"unordered rows", func(s *Snapshot) { s.Activations[0], s.Activations[1] = s.Activations[1], s.Activations[0] }},
		{"old provider policy", func(s *Snapshot) { s.Activations[5].PolicyVersion = 1 }},
		{"future provider policy", func(s *Snapshot) { s.Activations[5].PolicyVersion = 3 }},
		{"mixed snapshot schema", func(s *Snapshot) { s.Activations[5].SnapshotSchemaVersion = 2 }},
		{"zero policy", func(s *Snapshot) { s.Activations[0].PolicyVersion = 0 }},
		{"zero version", func(s *Snapshot) { s.Activations[0].Version = 0 }},
		{"negative version", func(s *Snapshot) { s.Activations[0].Version = -1 }},
		{"zero time", func(s *Snapshot) { s.Activations[0].UpdatedAt = time.Time{} }},
		{"future time", func(s *Snapshot) { s.Activations[0].UpdatedAt = time.Now().Add(time.Minute) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value := compatibleSnapshot()
			tc.mutate(&value)
			if !errors.Is(ValidateSnapshot(value), ErrUnavailable) {
				t.Fatal("incompatible snapshot accepted")
			}
		})
	}
	value := compatibleSnapshot()
	if err := ValidateSnapshot(value); err != nil {
		t.Fatal(err)
	}
	if activation, ok := value.Find("PAYMENT_PROVIDER_RESUME", 2, 1); !ok || activation.State != "ACTIVE" {
		t.Fatal("current frozen policy did not resolve")
	}
	for _, query := range []struct {
		command string
		policy  int64
		schema  int
	}{
		{"PAYMENT_PROVIDER_RESUME", 1, 1}, {"PAYMENT_PROVIDER_RESUME", 3, 1},
		{"PAYMENT_PROVIDER_RESUME", 2, 2}, {"payment_provider_resume", 2, 1}, {"FUTURE", 1, 1},
	} {
		if _, ok := value.Find(query.command, query.policy, query.schema); ok {
			t.Fatal("lookup substituted an incompatible command or frozen version")
		}
	}
}

func TestActivationClientTransportBoundaries(t *testing.T) {
	for _, baseURL := range []string{"", "/relative", "ftp://service", "https://user:password@service", "https://service?query=1", "https://service?", "https://service#fragment"} {
		if _, err := NewClient(baseURL, "test-workload-token"); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unsafe base URL accepted")
		}
	}
	if _, err := NewClient("http://service", " "); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing workload credential accepted")
	}
	var nilClient *Client
	if _, err := nilClient.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("nil client did not fail closed")
	}
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, err := NewClient(redirect.URL, "test-workload-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) || redirected.Load() != 0 {
		t.Fatal("redirect was followed or did not fail closed")
	}
	if client.client.Timeout != 5*time.Second {
		t.Fatal("client lacks bounded total HTTP timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Snapshot(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("canceled read did not fail closed")
	}
	unavailable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unavailable.Close()
	client, _ = NewClient(unavailable.URL, "test-workload-token")
	if _, err := client.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("network failure did not return the stable error")
	}
}

func TestActivationClientExactRevisionAndConflictingCacheHeaders(t *testing.T) {
	value := compatibleSnapshot()
	value.Activations[0].Version = 9007199254740993
	body := encodeSnapshot(t, value)
	var conflicting atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Cache-Control", "no-store")
		if conflicting.Load() {
			w.Header().Add("Cache-Control", "public, max-age=60")
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "test-workload-token")
	got, err := client.Snapshot(context.Background())
	if err != nil || got.Activations[0].Version != value.Activations[0].Version {
		t.Fatal("activation revision lost int64 precision")
	}
	conflicting.Store(true)
	if _, err := client.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("conflicting cache headers accepted")
	}
}
