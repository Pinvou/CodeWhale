package account

import (
	"slices"
	"sort"
	"testing"

	"github.com/google/uuid"
)

func TestF06MemberUnitNormalizationPreservesAcceptedSets(t *testing.T) {
	if value, err := normalizedMemberUnits(nil); err != nil || value != nil {
		t.Fatal("omitted units lost their legacy meaning", value, err)
	}
	if value, err := normalizedMemberUnits([]uuid.UUID{}); err != nil || value == nil || len(value) != 0 {
		t.Fatal("explicit empty units lost their meaning", value, err)
	}
	ids := make([]uuid.UUID, 257)
	for i := range ids {
		ids[i] = uuid.New()
	}
	want := slices.Clone(ids)
	sort.Slice(want, func(i, j int) bool { return want[i].String() < want[j].String() })
	slices.Reverse(ids)
	input := append(slices.Clone(ids), ids[0], ids[len(ids)-1])
	before := slices.Clone(input)
	got, err := normalizedMemberUnits(input)
	if err != nil || !slices.Equal(got, want) || !slices.Equal(input, before) {
		t.Fatal("large unit set, canonical ordering, duplicates or caller input changed", err)
	}
	if _, err := normalizedMemberUnits(append(input, uuid.Nil)); err != ErrConflict {
		t.Fatal("nil UUID stopped rejecting the whole input", err)
	}
}
