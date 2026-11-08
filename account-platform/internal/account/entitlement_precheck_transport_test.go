package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const entitlementPrecheckOwnerFixture = `{"command_type":"ENTITLEMENT_COMPONENT_ADJUSTMENT","policy_version":2,"snapshot_schema_version":2,"target_resource_type":"ENTITLEMENT_PACKAGE","target_resource_id":"00000000-0000-4000-8000-000000000001","normalized_command":{"package_id":"00000000-0000-4000-8000-000000000001","payment_order_id":"00000000-0000-4000-8000-000000000002","reason_code":"PRODUCT_SPLIT_CORRECTION"},"critical_resource_versions":{"package_id":"00000000-0000-4000-8000-000000000001","wallet_id":"00000000-0000-4000-8000-000000000003","wallet_version":9007199254740993,"payment_order_id":"00000000-0000-4000-8000-000000000002","payment_transaction_id":"00000000-0000-4000-8000-000000000004","source_component_id":"00000000-0000-4000-8000-000000000005","source_lot_id":"00000000-0000-4000-8000-000000000006","target_component_id":null,"target_lot_id":null,"source_type":"BASE","target_type":"BONUS","amount_microcredits":9007199254740993,"source_evidence_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","facts_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","policy_version":2},"financial_snapshot":{"package_total_microcredits":9223372036854775807,"source_original_microcredits":9223372036854775807,"target_original_microcredits":0,"effective_total_microcredits":9223372036854775807}}`

func TestF09EntitlementPrecheckOwnerTransport(t *testing.T) {
	request := ApprovalPrecheckRequest{CommandType: "ENTITLEMENT_COMPONENT_ADJUSTMENT", TargetResourceID: "00000000-0000-4000-8000-000000000001", PaymentOrderID: "00000000-0000-4000-8000-000000000002", ReasonCode: "PRODUCT_SPLIT_CORRECTION"}
	t.Run("closed-owner-projection-preserves-large-version", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.URL.Path != "/internal/v1/approval-command-prechecks" || r.Header.Get("Authorization") != "Bearer controlled-precheck" {
				t.Error("wrong authority request")
			}
			var got ApprovalPrecheckRequest
			if json.NewDecoder(r.Body).Decode(&got) != nil || got != request {
				t.Error("owner request facts changed")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(entitlementPrecheckOwnerFixture))
		}))
		defer server.Close()
		source, err := NewHTTPApprovalPrecheckSource(server.URL, "controlled-precheck")
		if err != nil {
			t.Fatal(err)
		}
		p, err := source.Precheck(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if p.PolicyVersion != 2 || p.SnapshotSchemaVersion != 2 || !bytes.Contains(p.CriticalResourceVersions, []byte(`"wallet_version":9007199254740993`)) {
			t.Fatal("closed proof changed")
		}
	})
	for _, bad := range []struct{ name, body string }{
		{"foreign-order", strings.Replace(entitlementPrecheckOwnerFixture, request.PaymentOrderID, "00000000-0000-4000-8000-000000000009", 1)},
		{"obsolete-schema", strings.Replace(entitlementPrecheckOwnerFixture, `"snapshot_schema_version":2`, `"snapshot_schema_version":1`, 1)},
		{"second-json", entitlementPrecheckOwnerFixture + " {}"},
		{"duplicate-key", strings.Replace(entitlementPrecheckOwnerFixture, `"wallet_version":9007199254740993`, `"wallet_version":9007199254740993,"wallet_version":7`, 1)},
	} {
		t.Run(bad.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(bad.body)) }))
			defer server.Close()
			source, err := NewHTTPApprovalPrecheckSource(server.URL, "controlled-precheck")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = source.Precheck(context.Background(), request); err == nil {
				t.Fatal("invalid owner statement accepted")
			}
		})
	}
	t.Run("same-host-redirect-cannot-change-owner-path", func(t *testing.T) {
		extra := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/other" {
				extra++
				_, _ = w.Write([]byte(entitlementPrecheckOwnerFixture))
				return
			}
			http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
		}))
		defer server.Close()
		source, err := NewHTTPApprovalPrecheckSource(server.URL, "controlled-precheck")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = source.Precheck(context.Background(), request); err == nil || extra != 0 {
			t.Fatal("redirect accepted")
		}
	})
	t.Run("future-command-never-reaches-owner", func(t *testing.T) {
		called := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
		defer server.Close()
		source, err := NewHTTPApprovalPrecheckSource(server.URL, "controlled-precheck")
		if err != nil {
			t.Fatal(err)
		}
		bad := request
		bad.CommandType = "LEDGER_REPAIR"
		if _, err = source.Precheck(context.Background(), bad); err == nil || called {
			t.Fatal("future owner dispatched")
		}
	})
}
