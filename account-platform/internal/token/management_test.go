package token

import (
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeMemberIDsRemovesNilAndDuplicates(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	items := normalizeMemberIDs([]uuid.UUID{second, uuid.Nil, first, second})
	if len(items) != 2 || items[0].String() > items[1].String() {
		t.Fatalf("unexpected normalized members: %v", items)
	}
}

func TestManagementScopeBoundaries(t *testing.T) {
	if shareScopeAllowed("ORG", "OWNER_SUBTREE") {
		t.Fatal("organization-owned account cannot use a unit subtree without an owner unit")
	}
	if !shareScopeAllowed("ORG_UNIT", "OWNER_SUBTREE") {
		t.Fatal("organization-unit account should support subtree sharing")
	}
	if defaultScopeAllowed("USER", "ORG") || defaultScopeAllowed("ORG", "UNIT") {
		t.Fatal("default route scope must match the token owner boundary")
	}
}
