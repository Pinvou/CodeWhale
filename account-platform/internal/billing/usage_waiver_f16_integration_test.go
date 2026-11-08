package billing

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/usagewaiver"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestUsageWaiverExpiryRepairWinnerPostgres(t *testing.T) {
	for _, state := range []string{"pending-original-admission", "admitted-expiry-winner", "persisted-rejection-retired"} {
		t.Run(state, func(t *testing.T) {
			var command usagewaiver.Command
			f := f16ActualReleasedMemberFixture(t, f16MemberRepairOptions{BeforeExpiry: func(t *testing.T, f *f16MemberRepairFixture) {
				var err error
				command, err = f.Repository.PrepareUsageWaiver(f.Context, f.HoldA.ID, "controlled waiver before package expiry", uuid.NewString(), waiverFixtureActor())
				if err != nil {
					t.Fatal("original real prepare", err)
				}
			}})
			r, ctx := f.Repository, f.Context
			input := UsageSettlementRepairCommandInput{UsageRequestID: f.UsageRequestA, HoldID: f.HoldA.ID, LossReviewID: f.LossReviewID, EvidenceKind: usageSettlementRepairEvidenceKind}
			if state == "admitted-expiry-winner" {
				// This is the actual Token-only writer contract, after its admitted
				// coordinator. Expiry has already won the hold lock, so it must be read as
				// such rather than inventing an administrator successful release.
				result, err := r.ExecuteUsageWaiver(ctx, usagewaiver.ExecuteRequest{Reference: command.Reference, Digest: command.Digest})
				if err != nil || result.Outcome != usagewaiver.ReleasedOther {
					t.Fatal("expiry result changed", result, err)
				}
				var reason string
				if err := r.pool.QueryRow(ctx, `SELECT usage_snapshot->>'release_reason' FROM usage_holds WHERE id=$1`, f.HoldA.ID).Scan(&reason); err != nil || reason != "ENTITLEMENT_EXPIRED_UNCERTAIN" {
					t.Fatal("original expiry facts replaced", reason, err)
				}
				var actions int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_hold_admin_actions WHERE hold_id=$1`, f.HoldA.ID).Scan(&actions); err != nil || actions != 0 {
					t.Fatal("false successful admin audit", actions, err)
				}
			}
			if state == "persisted-rejection-retired" {
				// Exact decision contract from the separate Token authority is controlled
				// in this writer fixture. The real two-service admission test proves its
				// durable production publisher and lost-rejection ACK recovery.
				rejection := usagewaiver.Decision{CommandReference: command.Reference, RequestDigest: command.Digest, Admission: usagewaiver.Rejected, ReasonCode: "EXECUTION_ACTIVE"}
				if err := r.RecordUsageWaiverRejection(ctx, command, rejection); err != nil {
					t.Fatal(err)
				}
				_, approved := f16MemberRepairInput(t, f)
				result, err := r.ApplyUsageSettlementRepair(ctx, approved)
				if err != nil || result.ExpectedMicrocredits != f.Claimed {
					t.Fatal("rejected waiver poisoned legal repair", result, err)
				}
				return
			}
			before := f16MemberFinancialSnapshot(t, f) + waiverFixtureSnapshot(t, r, ctx)
			if _, err := usageSettlementRepairPlanFrom(ctx, r.pool, input, 1, 1, f.Activation); !errors.Is(err, ErrApprovalSnapshotMismatch) || !strings.Contains(err.Error(), "pending original release-only waiver decision") {
				t.Fatal("precheck did not name pending release-only fence", err)
			}
			// Exercise the real activated writer and original wallet -> hold lock.
			// The controlled envelope intentionally has no approved financial snapshot:
			// its rejection must be this prior pending fence, not a later digest check.
			request := UsageSettlementRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: uuid.NewString(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: f.Activation, Command: input}
			if _, err := r.ApplyUsageSettlementRepair(ctx, request); !errors.Is(err, ErrApprovalSnapshotMismatch) || !strings.Contains(err.Error(), "pending original release-only waiver decision") {
				t.Fatal("locked repair did not reject at pending fence", err)
			}
			if before != f16MemberFinancialSnapshot(t, f)+waiverFixtureSnapshot(t, r, ctx) {
				t.Fatal("rejected later repair changed customer money, source, audit or command")
			}
		})
	}
}
