package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/usagewaiver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func (r *PostgresRepository) original828AdminReleaseUsageHold(ctx context.Context, holdID uuid.UUID, reason, actor, idempotencyKey string) (AdminHold, error) {
	reason, actor, idempotencyKey = strings.TrimSpace(reason), strings.TrimSpace(actor), strings.TrimSpace(idempotencyKey)
	if holdID == uuid.Nil || reason == "" || len(reason) > 500 || actor == "" || len(idempotencyKey) < 8 || len(idempotencyKey) > 120 {
		return AdminHold{}, ErrConflict
	}
	fingerprintBytes := sha256.Sum256([]byte(holdID.String() + "\x00" + reason + "\x00" + actor))
	fingerprint := fmt.Sprintf("%x", fingerprintBytes[:])
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AdminHold{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "usage-hold.admin.safe-release", idempotencyKey); err != nil {
		return AdminHold{}, err
	}
	var existingHoldID uuid.UUID
	var existingFingerprint string
	err = tx.QueryRow(ctx, `SELECT hold_id,request_fingerprint FROM usage_hold_admin_actions WHERE action='SAFE_RELEASE' AND idempotency_key=$1`, idempotencyKey).Scan(&existingHoldID, &existingFingerprint)
	if err == nil {
		if existingHoldID != holdID || existingFingerprint != fingerprint {
			return AdminHold{}, ErrIdempotencyConflict
		}
		return adminHoldByID(ctx, tx, holdID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AdminHold{}, err
	}
	var hold AdminHold
	err = tx.QueryRow(ctx, `SELECT id,usage_request_id,wallet_id,price_version_id,status,max_microcredits,actual_microcredits,expires_at,terminal_at,model_id,protocol_code,formula_version_id,pricing_snapshot,request_snapshot,usage_snapshot,organization_id,member_user_id,membership_version,created_at,updated_at FROM usage_holds WHERE id=$1 FOR UPDATE`, holdID).Scan(&hold.ID, &hold.UsageRequestID, &hold.WalletID, &hold.PriceVersionID, &hold.Status, &hold.MaxMicrocredits, &hold.ActualMicrocredits, &hold.ExpiresAt, &hold.TerminalAt, &hold.ModelID, &hold.ProtocolCode, &hold.FormulaVersionID, &hold.PricingSnapshot, &hold.RequestSnapshot, &hold.UsageSnapshot, &hold.OrganizationID, &hold.MemberUserID, &hold.MembershipVersion, &hold.CreatedAt, &hold.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminHold{}, ErrNotFound
	}
	if err != nil {
		return AdminHold{}, err
	}
	now := r.now()
	if hold.Status != "REVIEW" && !(hold.Status == "ACTIVE" && !hold.ExpiresAt.After(now)) {
		return AdminHold{}, ErrConflict
	}
	beforeStatus := hold.Status
	if err := releasePackageAllocations(ctx, tx, hold.ID, now); err != nil {
		return AdminHold{}, err
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits-$2,version=version+1,updated_at=$3 WHERE id=$1 AND reserved_microcredits >= $2`, hold.WalletID, hold.MaxMicrocredits, now)
	if err != nil || command.RowsAffected() != 1 {
		if err != nil {
			return AdminHold{}, err
		}
		return AdminHold{}, ErrConflict
	}
	snapshot, _ := json.Marshal(map[string]string{"admin_release_reason": reason, "admin_actor": actor})
	if _, err := tx.Exec(ctx, `UPDATE usage_holds SET status='RELEASED',usage_snapshot=COALESCE(usage_snapshot,'{}'::jsonb) || $2::jsonb,terminal_at=$3,updated_at=$3 WHERE id=$1`, holdID, snapshot, now); err != nil {
		return AdminHold{}, err
	}
	var allocationSnapshot json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT jsonb_build_object(
'component_allocations',COALESCE((SELECT jsonb_agg(jsonb_build_object('package_id',package_id,'component_id',component_id,'reserved_microcredits',reserved_microcredits,'settled_microcredits',settled_microcredits,'released_microcredits',released_microcredits) ORDER BY component_id) FROM usage_hold_allocations WHERE hold_id=$1),'[]'::jsonb),
'member_allocations',COALESCE((SELECT jsonb_agg(jsonb_build_object('grant_id',grant_id,'component_id',component_id,'reserved_microcredits',reserved_microcredits,'settled_microcredits',settled_microcredits,'released_microcredits',released_microcredits) ORDER BY grant_id) FROM member_quota_hold_allocations WHERE hold_id=$1),'[]'::jsonb))`, holdID).Scan(&allocationSnapshot); err != nil {
		return AdminHold{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_hold_admin_actions(id,hold_id,action,idempotency_key,request_fingerprint,reason,actor,before_status,after_status,allocation_snapshot,created_at) VALUES($1,$2,'SAFE_RELEASE',$3,$4,$5,$6,$7,'RELEASED',$8,$9)`, uuid.Must(uuid.NewV7()), holdID, idempotencyKey, fingerprint, reason, actor, beforeStatus, allocationSnapshot, now); err != nil {
		return AdminHold{}, err
	}
	hold.Status, hold.AnomalyState, hold.TerminalAt, hold.UpdatedAt = "RELEASED", "NONE", &now, now
	if err := tx.Commit(ctx); err != nil {
		return AdminHold{}, err
	}
	return hold, nil
}

// The writer above is extracted from 828242031c:internal/billing/admin.go,
// renamed only. It executes the actual original transaction and INSERT on the
// complete current public schema, using the current allocation-release helper.
// Account authority and Token are controlled HTTP boundaries, not live services.
type originalReplayToken struct{ calls int }

func (c *originalReplayToken) WaiveUsage(context.Context, usagewaiver.Command) (usagewaiver.Decision, error) {
	c.calls++
	return usagewaiver.Decision{}, errors.New("must not call Token for history")
}
func TestUsageWaiverOriginal828HTTPReplayPostgres(t *testing.T) {
	f06WalletActors(t)
	r, ctx := retainedFinanceRepository(t)
	actor := waiverFixtureActor()
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		var in struct {
			Issuer, Subject, Permission string
			All                         []string `json:"all_permissions"`
		}
		if json.NewDecoder(q.Body).Decode(&in) != nil || in.Issuer != actor.Issuer || in.Subject != actor.Subject || !((len(in.All) == 1 && in.All[0] == "platform.billing.holds.release") || (len(in.All) == 0 && in.Permission == "platform.billing.holds.release")) {
			t.Error("wrong authority request")
		}
		w.Header().Set("X-Authorization-Version", "19")
		json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actor.ID})
	}))
	defer authority.Close()
	authorizer, err := authorization.NewClient(authority.URL, "controlled-replay")
	if err != nil {
		t.Fatal(err)
	}
	token := &originalReplayToken{}
	handler := NewService(r).WithAuthorizer(authorizer).WithUsageWaiverClient(token).Handler("controlled-replay", func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: actor.Issuer, Subject: actor.Subject}, nil
	})
	for _, n := range []int{8, 9, 10, 11, 12, 13, 14, 15, 16, 120} {
		t.Run(fmt.Sprintf("key-%d", n), func(t *testing.T) {
			h, _ := waiverFixtureHold(t, r, ctx, 40)
			if _, err := r.pool.Exec(ctx, `UPDATE usage_holds SET status='REVIEW' WHERE id=$1`, h.ID); err != nil {
				t.Fatal(err)
			}
			key := strings.Repeat("k", n-4) + fmt.Sprintf("%04d", n)
			oldReason := "  original historical reason\t"
			oldActor := "  " + actor.Issuer + "|" + actor.Subject + "\t"
			hold, err := r.original828AdminReleaseUsageHold(ctx, h.ID, oldReason, oldActor, " "+key+"\t")
			if err != nil || hold.Status != "RELEASED" {
				t.Fatal("original writer", hold.Status, err)
			}
			sum := sha256.Sum256([]byte(h.ID.String() + "\x00" + strings.TrimSpace(oldReason) + "\x00" + strings.TrimSpace(oldActor)))
			var stored string
			if err := r.pool.QueryRow(ctx, `SELECT request_fingerprint FROM usage_hold_admin_actions WHERE idempotency_key=$1`, key).Scan(&stored); err != nil || stored != hex.EncodeToString(sum[:]) {
				t.Fatal("original writer identity", stored, err)
			}
			before := waiverFixtureSnapshot(t, r, ctx)
			call := func(id uuid.UUID, reason, key string) *httptest.ResponseRecorder {
				raw, _ := json.Marshal(map[string]string{"reason": reason})
				q := httptest.NewRequest("POST", "/admin/v1/usage-holds/"+id.String()+"/release", strings.NewReader(string(raw)))
				q.Header.Set("Authorization", "Bearer controlled-human")
				q.Header.Set("Content-Type", "application/json")
				q.Header.Set("Idempotency-Key", key)
				out := httptest.NewRecorder()
				handler.ServeHTTP(out, q)
				return out
			}
			for _, replayKey := range []string{key, " " + key + "\t"} {
				out := call(h.ID, oldReason, replayKey)
				if out.Code != 200 {
					t.Fatalf("original replay %d: %s", out.Code, out.Body.String())
				}
				var result LegacyUsageWaiverRecord
				if json.Unmarshal(out.Body.Bytes(), &result) != nil || !result.Legacy || !result.Completed || result.HoldID != h.ID || result.RequestFingerprint != stored || result.ActionID == nil {
					t.Fatal("wrong original receipt", out.Body.String())
				}
			}
			if out := call(h.ID, "changed reason", key); out.Code != 409 {
				t.Fatal("reason substitution accepted", out.Code)
			}
			if out := call(uuid.New(), oldReason, key); out.Code != 409 {
				t.Fatal("hold substitution accepted", out.Code)
			}
			other := actor
			other.Subject = "other"
			if _, err := r.LegacyUsageWaiver(ctx, h.ID, oldReason, key, other); !errors.Is(err, ErrConflict) {
				t.Fatal("actor substitution", err)
			}
			if token.calls != 0 || before != waiverFixtureSnapshot(t, r, ctx) {
				t.Fatal("history replay wrote facts or called Token", token.calls)
			}
		})
	}
	t.Run("unknown-short-key-does-not-prepare", func(t *testing.T) {
		h, _ := waiverFixtureHold(t, r, ctx, 40)
		before := waiverFixtureSnapshot(t, r, ctx)
		if _, err := r.PrepareUsageWaiver(ctx, h.ID, "new command", "newkey08", actor); !errors.Is(err, ErrConflict) {
			t.Fatal("modern key gate weakened", err)
		}
		if before != waiverFixtureSnapshot(t, r, ctx) {
			t.Fatal("short unknown key wrote facts")
		}
	})
}

func adminHoldByID(ctx context.Context, tx pgx.Tx, holdID uuid.UUID) (AdminHold, error) {
	var hold AdminHold
	err := tx.QueryRow(ctx, `SELECT hold.id,hold.usage_request_id,hold.wallet_id,hold.price_version_id,hold.status,
CASE WHEN hold.status='REVIEW' THEN 'REVIEW_REQUIRED'
 WHEN hold.status='ACTIVE' AND hold.expires_at<=now() THEN 'LEASE_EXPIRED'
 WHEN hold.status<>'ACTIVE' AND COALESCE(allocation.outstanding,0)<>0 THEN 'TERMINAL_ALLOCATION_REMAINS'
 WHEN hold.status='ACTIVE' AND COALESCE(allocation.outstanding,0)<>hold.max_microcredits THEN 'ALLOCATION_MISMATCH'
 ELSE 'NONE' END,
hold.max_microcredits,hold.actual_microcredits,hold.expires_at,hold.terminal_at,hold.model_id,hold.protocol_code,hold.formula_version_id,hold.pricing_snapshot,hold.request_snapshot,hold.usage_snapshot,hold.organization_id,hold.member_user_id,hold.membership_version,hold.created_at,hold.updated_at
FROM usage_holds hold LEFT JOIN (
 SELECT hold_id,sum(reserved_microcredits-settled_microcredits-released_microcredits) outstanding
 FROM usage_hold_allocations GROUP BY hold_id
) allocation ON allocation.hold_id=hold.id WHERE hold.id=$1`, holdID).Scan(&hold.ID, &hold.UsageRequestID, &hold.WalletID, &hold.PriceVersionID, &hold.Status, &hold.AnomalyState, &hold.MaxMicrocredits, &hold.ActualMicrocredits, &hold.ExpiresAt, &hold.TerminalAt, &hold.ModelID, &hold.ProtocolCode, &hold.FormulaVersionID, &hold.PricingSnapshot, &hold.RequestSnapshot, &hold.UsageSnapshot, &hold.OrganizationID, &hold.MemberUserID, &hold.MembershipVersion, &hold.CreatedAt, &hold.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminHold{}, ErrNotFound
	}
	return hold, err
}

func TestUsageWaiverRejectsInvalidKeysBeforeDatabase(t *testing.T) {
	r := &PostgresRepository{} // No pool: invalid inputs must return before Begin.
	for name, key := range map[string]string{"empty": "", "short": "1234567", "long": strings.Repeat("k", 121), "unicode-long": strings.Repeat("界", 121), "invalid-utf8": string([]byte{0xff}), "whitespace": strings.Repeat(" ", 300)} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.PrepareUsageWaiver(context.Background(), uuid.New(), "reason", key, waiverFixtureActor()); !errors.Is(err, ErrConflict) {
				t.Fatal("invalid key was not rejected", err)
			}
		})
	}
	for _, key := range []string{strings.Repeat("界", 16), strings.Repeat("界", 120)} {
		if !validWaiverKeyCandidate(key) || (len(key) > 120 && validOriginalWaiverKey(key)) {
			t.Fatal("modern Unicode rune boundary changed")
		}
	}
}
