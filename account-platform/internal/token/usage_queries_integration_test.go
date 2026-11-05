package token

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This exercises private repository foundations against the full Token schema;
// it does not expose or simulate an unregistered F04 management HTTP route.
func TestF04PrivateUsageQueryFoundations(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	usage := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "HOLD_RESERVED")
	now := time.Now().UTC().Truncate(time.Microsecond)
	caseID, unrelatedCase := uuid.New(), uuid.New()
	for _, item := range []struct {
		id   uuid.UUID
		kind string
	}{{caseID, "USAGE_RECONCILIATION_REVIEW"}, {unrelatedCase, "UNRELATED_REVIEW"}} {
		_, err := r.pool.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'TOKEN',$2,'USAGE_REQUEST',$3,'P1','OPEN',$4,$4,$4,$4,$4,$4)`, item.id, item.kind, usage, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	cancelID, convergenceID := uuid.New(), uuid.New()
	if _, err := r.pool.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'CANCEL_REQUESTED','controlled','{"safe":true}',$3)`, cancelID, usage, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO usage_convergence_evidence(id,usage_request_id,evidence_type,source_type,decision,evidence_key,payload_digest,sanitized_snapshot,observed_at) VALUES($1,$2,'UNCERTAINTY','GATEWAY','REVIEW','controlled',$3,'{"safe":true}',$4)`, convergenceID, usage, strings.Repeat("a", 64), now.Add(time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	filter := func(values url.Values, allowed ...string) adminQueryFilter {
		f, err := parseAdminQuery(values, allowed)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	readMembers := func(t *testing.T, source AdminSnapshotMemberSource) []string {
		t.Helper()
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		id := uuid.New()
		_, err = tx.Exec(ctx, `INSERT INTO token_admin_pagination_snapshots(id,subject_id,authorization_version,resource_type,scope_digest,filter_digest,sort_spec,page_limit,high_water,expires_at,member_count,member_bytes,created_at) VALUES($1,$2,1,'CONTROLLED_PRIVATE_QUERY',$3,$3,'controlled',100,$4::timestamptz,$4::timestamptz+interval '1 hour',0,0,$4::timestamptz)`, id, uuid.New(), strings.Repeat("a", 64), now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err = source(ctx, tx, id, now.Add(time.Second), 100); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx, `SELECT resource_id FROM token_admin_pagination_snapshot_members WHERE snapshot_id=$1 ORDER BY position`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var key string
			if err = rows.Scan(&key); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, key)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	t.Run("usage filters and narrow projection", func(t *testing.T) {
		source, err := usageSnapshotSource(filter(url.Values{"q": {usage.String()}, "execution_status": {"HOLD_RESERVED"}}, "q", "execution_status"))
		if err != nil {
			t.Fatal(err)
		}
		keys := readMembers(t, source)
		if len(keys) != 1 || keys[0] != usage.String() {
			t.Fatal("wrong usage selection", keys)
		}
		var queries usageQueryRepository = r
		items, err := queries.adminUsageByIDs(ctx, keys)
		if err != nil || len(items) != 1 || items[0].ID != usage || items[0].MaximumMicrocredits != 100 {
			t.Fatal("wrong usage projection", items, err)
		}
		source, err = usageSnapshotSource(filter(url.Values{"execution_status": {"SUCCEEDED"}}, "execution_status"))
		if err != nil || len(readMembers(t, source)) != 0 {
			t.Fatal("execution filter ignored", err)
		}
		if _, err = queries.adminUsageByIDs(ctx, []string{uuid.NewString()}); !errors.Is(err, ErrAdminSnapshotResourceChanged) {
			t.Fatal("missing snapshot member accepted", err)
		}
	})
	t.Run("reconciliation domain and membership", func(t *testing.T) {
		source, err := reconciliationSnapshotSource(filter(url.Values{"status": {"OPEN"}}, "status"))
		if err != nil {
			t.Fatal(err)
		}
		keys := readMembers(t, source)
		if len(keys) != 1 || keys[0] != caseID.String() {
			t.Fatal("unrelated case leaked", keys)
		}
		items, err := r.adminReconciliationByIDs(ctx, keys)
		if err != nil || len(items) != 1 || items[0].ResourceID != usage {
			t.Fatal(items, err)
		}
		if _, err = r.adminReconciliationByIDs(ctx, []string{unrelatedCase.String()}); !errors.Is(err, ErrAdminSnapshotResourceChanged) {
			t.Fatal("unrelated projection accepted", err)
		}
	})
	t.Run("evidence union stable ordering and filter", func(t *testing.T) {
		source, err := evidenceSnapshotSource(filter(url.Values{}), usage)
		if err != nil {
			t.Fatal(err)
		}
		keys := readMembers(t, source)
		items, err := r.adminEvidenceByIDs(ctx, keys)
		if err != nil || len(items) != 2 || items[0].SourceID != cancelID || items[1].SourceID != convergenceID {
			t.Fatal("evidence order/source mismatch", items, err)
		}
		reversed, err := r.adminEvidenceByIDs(ctx, []string{keys[1], keys[0]})
		if err != nil || reversed[0].SourceID != convergenceID {
			t.Fatal("snapshot member order lost", err)
		}
		source, err = evidenceSnapshotSource(filter(url.Values{"evidence_type": {"UNCERTAINTY"}}, "evidence_type"), usage)
		if err != nil || len(readMembers(t, source)) != 1 {
			t.Fatal("evidence filter ignored", err)
		}
		if _, err = r.adminEvidenceByIDs(ctx, []string{"not-a-member"}); !errors.Is(err, ErrAdminPaginationCursorInvalid) {
			t.Fatal("malformed evidence key accepted", err)
		}
	})
}
