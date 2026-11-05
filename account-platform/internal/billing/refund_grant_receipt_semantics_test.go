package billing

import (
	"bytes"
	"encoding/json"
	"github.com/google/uuid"
	"reflect"
	"testing"
	"time"
)

// Compare the complete typed receipt protocol. Location identity is not a
// serialized fact; every DTO field and timestamp's nanosecond value remains.
func sameRefundRecoveryGrantReceipt(first, replayed MemberQuotaGrant) bool {
	original, err := json.Marshal(first)
	if err != nil {
		return false
	}
	replay, err := json.Marshal(replayed)
	return err == nil && bytes.Equal(original, replay)
}

func TestRefundRecoveryGrantReceiptProtocolFacts(t *testing.T) {
	reason := "MEMBER_REMOVED"
	now := time.Date(2026, 10, 26, 1, 2, 3, 123456789, time.UTC)
	original := MemberQuotaGrant{
		ID: uuid.New(), OrganizationID: uuid.New(), MemberUserID: uuid.New(), PackageID: uuid.New(), ComponentID: uuid.New(),
		ComponentType: "BASE", OriginalMicrocredits: 80, AvailableMicrocredits: 11, ReservedMicrocredits: 12, UsedMicrocredits: 13,
		ExpiredMicrocredits: 14, RevokedMicrocredits: 30, Status: "SUSPENDED", SuspensionReason: &reason,
		MembershipVersion: 2, AllocationVersion: 3, CreatedBy: uuid.New(),
		ExpiresAt: now.Add(time.Hour), CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}
	t.Run("same-complete-protocol-across-zero-offset-location", func(t *testing.T) {
		first := original
		first.ExpiresAt = first.ExpiresAt.In(time.FixedZone("controlled-scan-UTC", 0))
		first.CreatedAt = first.CreatedAt.In(time.FixedZone("controlled-scan-UTC", 0))
		first.UpdatedAt = first.UpdatedAt.In(time.FixedZone("controlled-scan-UTC", 0))
		if reflect.DeepEqual(first, original) || !sameRefundRecoveryGrantReceipt(first, original) {
			t.Fatal("same complete protocol differed by representation")
		}
	})
	// Mutation controls are independent of JSON comparison and cover every
	// current field, including exact UUIDs, each money bucket, versions and times.
	values := reflect.ValueOf(original)
	typ := values.Type()
	for i := 0; i < values.NumField(); i++ {
		t.Run("reject-"+typ.Field(i).Name, func(t *testing.T) {
			changed := original
			v := reflect.ValueOf(&changed).Elem().Field(i)
			switch current := v.Interface().(type) {
			case uuid.UUID:
				v.Set(reflect.ValueOf(uuid.New()))
			case int64:
				v.SetInt(current + 1)
			case string:
				v.SetString(current + "-changed")
			case *string:
				other := "ACCOUNT_CLOSED"
				v.Set(reflect.ValueOf(&other))
			case time.Time:
				v.Set(reflect.ValueOf(current.Add(time.Nanosecond)))
			default:
				t.Fatalf("uncovered receipt field type %s", typ.Field(i).Name)
			}
			if sameRefundRecoveryGrantReceipt(original, changed) {
				t.Fatal("changed complete receipt field was accepted")
			}
		})
	}
	t.Run("reject-omitted-vs-present-suspension", func(t *testing.T) {
		changed := original
		changed.SuspensionReason = nil
		if sameRefundRecoveryGrantReceipt(original, changed) {
			t.Fatal("omitted suspension accepted")
		}
	})
	t.Run("reject-unserializable-time", func(t *testing.T) {
		changed := original
		changed.UpdatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		if sameRefundRecoveryGrantReceipt(changed, changed) {
			t.Fatal("invalid protocol encoding accepted")
		}
	})
}
