package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"testing"
)

func TestLegacyChargebackCompatibilityRejectsUnfrozenSQL(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range loaded {
		if m.Version != 29 {
			continue
		}
		m.UpSQL += "\nSELECT 1;\n"
		m.Checksum = sqlChecksum(m.UpSQL)
		if err = applyBillingMigration(context.Background(), nil, m); err == nil {
			t.Fatal("mutated SQL accepted despite forged metadata")
		}
		return
	}
	t.Fatal("missing original 029")
}
