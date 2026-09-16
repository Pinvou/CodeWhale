package billing

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func TestBillingResumeProofClaimsPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	id := uuid.New()
	providerID, providerVersion, productID := "proof-provider", "v1", "PROOF_"+strings.ReplaceAll(id.String(), "-", "")
	walletID, requesterID, orderID, caseID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	itemID, mappingID, verificationID, evidenceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	steps := []struct {
		statement string
		args      []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',now(),now())`, []any{walletID, requesterID}},
		{`INSERT INTO recharge_products(product_id,amount_fen,credit_microcredits,display_points,currency,channels,catalog_version,enabled,created_at) VALUES($1,100,1000000,100,'CNY',ARRAY['ALIPAY'],1,true,now())`, []any{productID}},
		{`INSERT INTO recharge_product_catalog_items(catalog_item_id,product_id,display_name_zh_cn,display_name_en,display_name_ja,lifecycle_status,admission_version,created_at,updated_at) VALUES($1,$2,'Test','Test','Test','ON_SALE',1,now(),now())`, []any{itemID, productID}},
		{`INSERT INTO payment_product_mappings(id,provider_id,provider_version,product_id,mapping_version,external_sku,amount_fen,currency,channels,status,effective_from,created_at) VALUES($1,$2,$3,$4,1,$5,100,'CNY',ARRAY['ALIPAY'],'FROZEN',now()-interval '1 day',now())`, []any{mappingID, providerID, providerVersion, productID, productID}},
		{`INSERT INTO product_admission_verifications(verification_id,catalog_item_id,target_mapping_id,publish_fingerprint,status,requested_at,completed_at) VALUES($1,$2,$3,$4,'VERIFIED',now(),now())`, []any{verificationID, itemID, mappingID, strings.Repeat("a", 64)}},
		{`INSERT INTO product_admission_evidence(evidence_id,verification_id,catalog_item_id,publish_fingerprint,issuer_workload,verification_method,source_reference,issued_at,expires_at,created_at) VALUES($1,$2,$3,$4,'test','test','test',now()-interval '1 day',now()+interval '1 day',now())`, []any{evidenceID, verificationID, itemID, strings.Repeat("a", 64)}},
		{`INSERT INTO product_admission_bindings(catalog_item_id,admission_version,evidence_id,mapping_id,effective_from,created_at) VALUES($1,1,$2,$3,now()-interval '1 day',now())`, []any{itemID, evidenceID, mappingID}},
		{`INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,updated_at) VALUES($1,$2,false,now())`, []any{providerID, providerVersion}},
		{`INSERT INTO payment_product_controls(provider_id,provider_version,product_id,new_orders_enabled,reason_code,updated_at) VALUES($1,$2,$3,false,'MANUAL_SAFETY_PAUSE',now())`, []any{providerID, providerVersion, productID}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,created_at,updated_at,provider_id,provider_version,provider_payable_until,late_review_boundary_at,final_archive_eligible_at) VALUES($1,$2,$3,$3,$4,$5,'{}','ALIPAY',100,'CNY',1000000,'FAILED','PENDING',now()+interval '1 day',now(),now(),$6,$7,now()+interval '1 day',now()+interval '3 day',now()+interval '8 day')`, []any{orderID, "proof-" + id.String(), requesterID, walletID, productID, providerID, providerVersion}},
		{`INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'PAYMENT','PAYMENT_FACT_CONFLICT','PAYMENT_ORDER',$2,'P2','OPEN',now(),now()+interval '1 day',now()+interval '1 day',now()+interval '1 day',now()+interval '1 day',now())`, []any{caseID, orderID}},
		{`INSERT INTO payment_anomaly_cases(id,anomaly_type,dedupe_key,payment_order_id,status,resource_snapshot,created_at,updated_at) VALUES($1,'PAYMENT_FACT_CONFLICT',$2,$3,'OPEN','{}',now(),now())`, []any{caseID, "proof-" + id.String(), orderID}},
		{`INSERT INTO payment_provider_failure_confirmations(id,payment_anomaly_case_id,payment_order_id,provider_id,provider_version,provider_status,provider_payload_digest,provider_snapshot,queried_at,actor_id,reason,created_at) VALUES($1,$2,$3,$4,$5,'FAILED',$6,'{}',now(),$7,'confirmed failure',now())`, []any{uuid.New(), caseID, orderID, providerID, providerVersion, strings.Repeat("b", 64), requesterID}},
	}
	for index, step := range steps {
		if _, err := repository.pool.Exec(ctx, step.statement, step.args...); err != nil {
			t.Fatalf("fixture step %d: %v", index, err)
		}
	}
	service := NewService(repository)
	for _, commandType := range []string{"PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME"} {
		if commandType == "PAYMENT_PRODUCT_RESUME" {
			if _, err := repository.pool.Exec(ctx, `UPDATE payment_provider_controls SET new_orders_enabled=true,version=version+1,updated_at=now() WHERE provider_id=$1 AND provider_version=$2`, providerID, providerVersion); err != nil {
				t.Fatal(err)
			}
		}
		var command any
		var targetType, targetID string
		var policyVersion int64
		var providerControlVersion, productControlVersion any
		if commandType == "PAYMENT_PROVIDER_RESUME" {
			actual, err := repository.providerResumeVersions(ctx, ProviderResumeVersions{ProviderID: providerID, ProviderVersion: providerVersion, ProbeOrderID: orderID})
			if err != nil {
				t.Fatal(err)
			}
			command, targetType, targetID, policyVersion = actual, "PAYMENT_PROVIDER_CONTROL", providerID, 2
			providerControlVersion, productControlVersion = actual.ProviderControlVersion, nil
		} else {
			actual, err := repository.productResumeVersions(ctx, ProductResumeVersions{ProviderID: providerID, ProviderVersion: providerVersion, ProductID: productID, ProbeOrderID: orderID})
			if err != nil {
				t.Fatal(err)
			}
			command, targetType, targetID, policyVersion = actual, "PAYMENT_PRODUCT_CONTROL", productID, 1
			providerControlVersion, productControlVersion = actual.ProviderControlVersion, actual.ProductControlVersion
		}
		normalized, _ := json.Marshal(command)
		financial, _ := json.Marshal(map[string]any{"new_orders_enabled": false, "provider_control_version": providerControlVersion, "product_control_version": productControlVersion})
		artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
			CommandType: commandType, PolicyVersion: policyVersion, SnapshotSchemaVersion: 1,
			TargetResourceType: targetType, TargetResourceID: targetID,
			NormalizedCommand: normalized, CriticalResourceVersions: normalized, FinancialSnapshot: financial,
		})
		if err != nil {
			t.Fatal(err)
		}
		requestID, eventID := uuid.New(), uuid.New()
		proof, err := service.billingApprovalProof(ctx, approvalencoding.ProofRequest{ApprovalRequestID: requestID, Artifacts: artifacts})
		if err != nil || proof.Verdict != "ALLOW" {
			t.Fatalf("%s proof=%+v err=%v", commandType, proof, err)
		}
		event := approvalencoding.AuthorizationEvent{
			EventID: eventID, ApprovalRequestID: requestID, CommandType: commandType, PolicyVersion: policyVersion,
			TargetResourceType: targetType, TargetResourceID: targetID,
			NormalizedCommand: normalized, CriticalResourceVersions: normalized, FinancialSnapshot: financial,
			CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest,
			DomainRevision: proof.DomainRevision, ApprovalControlEpoch: 2,
		}
		binding := ApprovalAuthorizationBinding{ApprovalRequestID: requestID, EventID: eventID, CommandType: commandType, PolicyVersion: policyVersion, ApprovalEpoch: 2}
		if err := repository.ClaimBillingApprovalProof(ctx, binding, event); err != nil {
			t.Fatalf("%s claim: %v", commandType, err)
		}
	}
}
