package accountschema

import (
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestOrderedPreservesOriginalsAndPlacesMetadataBeforeScope(t *testing.T) {
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var seed migrations.Migration
	for _, migration := range installed {
		if migration.Version == metadataVersion {
			seed = migration
		}
	}
	input := []migrations.Migration{{Version: 86}, {Version: 34}, seed, {Version: 2}, {Version: 27}}
	ordered, err := Ordered(input)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	for _, migration := range ordered {
		versions = append(versions, migration.Version)
	}
	if !slices.Equal(versions, []int64{2, 27, metadataVersion, 34, 86}) {
		t.Fatalf("incorrect migration order: %v", versions)
	}
	if input[0].Version != 86 {
		t.Fatal("ordering changed the caller's migration list")
	}
	if _, err := Ordered(input[0:2]); err == nil {
		t.Fatal("missing metadata migration accepted")
	}
	if _, err := Ordered(append(input, migrations.Migration{Version: metadataVersion})); err == nil {
		t.Fatal("duplicate metadata migration accepted")
	}
	seed.UpSQL += "\nSELECT 1;\n"
	if _, err := Ordered([]migrations.Migration{seed}); err == nil {
		t.Fatal("modified SQL with a forged original checksum accepted")
	}
}
