package tokenauthority

import (
	"github.com/google/uuid"
	"testing"
)

func TestUnitOwnerIdentityFixedVector(t *testing.T) {
	unit := uuid.MustParse("018f0c7a-6b5d-7e3f-aa21-1234567890ad")
	organization := uuid.MustParse("018f0c7a-6b5d-7e3f-ba21-1234567890ae")
	if got := UnitOwnerIdentityDigest(unit, organization, "ACTIVE", 11); got != "4e85a03832a4908994f519baf8371460d3361fe5ae29ef175503929e4346a729" {
		t.Fatalf("unit owner digest=%s", got)
	}
	if UnitOwnerIdentityDigest(unit, organization, "ACTIVE", 11) == UnitOwnerIdentityDigest(unit, uuid.New(), "ACTIVE", 11) {
		t.Fatal("unit owner digest did not bind organization")
	}
}

func TestT3FixedVectors(t *testing.T) {
	actor := "018f0c7a-6b5d-7e3f-8a21-1234567890ab"
	reference := "018f0c7a-6b5d-7e3f-9a21-1234567890ac"
	owner := "018f0c7a-6b5d-7e3f-aa21-1234567890ad"
	ownerType := "ORGANIZATION"
	commandDigest, actorDigest, ownerDigest := repeat("a", 64), repeat("b", 64), repeat("c", 64)
	platformVersion, actorVersion, ownerVersion := "13", "7", "11"
	for _, scenario := range []struct {
		mode   string
		bytes  int
		digest string
	}{
		{ModeT3CreatorRecheck, 794, "af5fe3148c2d352552c1b54b62f70b16182ed469bd061aee30e5b573b75454ac"},
		{ModeT3OperatorRecheck, 795, "5d2038e2bea0487ea4ff23b7c6c1ad8366d9ddaf406078c8626e49f8044abba4"},
	} {
		request := Request{AuthoritySchemaVersion: SchemaVersion, Mode: scenario.mode, ActorUserID: &actor, OwnerType: &ownerType, OwnerID: &owner, RequiredPermissions: RequiredPermissions, CommandReference: reference, CommandRequestDigest: &commandDigest, ExpectedPlatformAuthorizationVersion: &platformVersion, ExpectedActorVersion: &actorVersion, ExpectedOwnerVersion: &ownerVersion, ExpectedActorIdentityDigest: &actorDigest, ExpectedOwnerIdentityDigest: &ownerDigest}
		request.AuthorityRequestSHA256 = request.Digest()
		if got := len(request.CanonicalJSON()); got != scenario.bytes {
			t.Fatalf("%s canonical bytes=%d want=%d: %s", scenario.mode, got, scenario.bytes, request.CanonicalJSON())
		}
		if request.AuthorityRequestSHA256 != scenario.digest {
			t.Fatalf("%s digest=%s", scenario.mode, request.AuthorityRequestSHA256)
		}
		if err := request.Validate(); err != nil {
			t.Fatalf("%s validate: %v", scenario.mode, err)
		}
	}
}

func TestAuthorityModeAndSemanticTamperingFailClosed(t *testing.T) {
	actor, reference, owner, ownerType := "018f0c7a-6b5d-7e3f-8a21-1234567890ab", "018f0c7a-6b5d-7e3f-9a21-1234567890ac", "018f0c7a-6b5d-7e3f-aa21-1234567890ad", "ORGANIZATION"
	commandDigest, actorDigest, ownerDigest := repeat("a", 64), repeat("b", 64), repeat("c", 64)
	platformVersion, actorVersion, ownerVersion := "13", "7", "11"
	request := Request{AuthoritySchemaVersion: SchemaVersion, Mode: ModeT3CreatorRecheck, ActorUserID: &actor, OwnerType: &ownerType, OwnerID: &owner, RequiredPermissions: RequiredPermissions, CommandReference: reference, CommandRequestDigest: &commandDigest, ExpectedPlatformAuthorizationVersion: &platformVersion, ExpectedActorVersion: &actorVersion, ExpectedOwnerVersion: &ownerVersion, ExpectedActorIdentityDigest: &actorDigest, ExpectedOwnerIdentityDigest: &ownerDigest}
	request.AuthorityRequestSHA256 = request.Digest()
	request.ExpectedOwnerIdentityDigest = pointer(repeat("d", 64))
	if err := request.Validate(); err == nil {
		t.Fatal("semantic mutation with the frozen digest was accepted")
	}
	request.AuthorityRequestSHA256 = request.Digest()
	request.Mode = ModeT3OperatorRecheck
	if err := request.Validate(); err == nil {
		t.Fatal("cross-mode mutation with the creator digest was accepted")
	}
}

func repeat(value string, count int) string {
	result := ""
	for i := 0; i < count; i++ {
		result += value
	}
	return result
}

func pointer(value string) *string { return &value }
