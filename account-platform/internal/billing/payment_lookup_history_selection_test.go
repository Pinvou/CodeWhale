package billing

import (
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestF12PaymentLookupHistoricalMembership(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, maximum := range []int64{900016, 900017} {
		selected, err := selectF12PaymentLookupStage(loaded, maximum)
		if err != nil {
			t.Fatal(err)
		}
		wantCount := 83
		if maximum == 900017 {
			wantCount = 84
		}
		if len(selected) != wantCount {
			t.Fatalf("historical membership count=%d want=%d", len(selected), wantCount)
		}
		for _, migration := range selected {
			if migration.Version == 67 || migration.Version == 68 || migration.Version > maximum {
				t.Fatal("later feature leaked into historical stage")
			}
		}
	}
	t.Run("future-low-base-excluded", func(t *testing.T) {
		future := append(append([]migrations.Migration(nil), loaded...), migrations.Migration{Version: 777, Name: "future_unregistered_base"})
		selected, err := selectF12PaymentLookupStage(future, 900016)
		if err != nil || len(selected) != 83 {
			t.Fatal("future base changed published historical membership", err)
		}
		for _, migration := range selected {
			if migration.Version == 777 {
				t.Fatal("numeric-prefix future base leaked into published history")
			}
		}
	})
	for _, kind := range []string{"missing", "name", "declared-checksum", "up-body", "down-body", "duplicate", "unsupported-stage"} {
		t.Run(kind, func(t *testing.T) {
			input := append([]migrations.Migration(nil), loaded...)
			index := -1
			for i, migration := range input {
				if migration.Version == 900016 {
					index = i
					break
				}
			}
			if index < 0 {
				t.Fatal("historical fixture lacks payment capability")
			}
			maximum := int64(900016)
			switch kind {
			case "missing":
				input = append(input[:index], input[index+1:]...)
			case "name":
				input[index].Name += "_different"
			case "declared-checksum":
				input[index].Checksum = "different"
			case "up-body":
				input[index].UpSQL += "\n-- different\n"
			case "down-body":
				input[index].DownSQL += "\n-- different\n"
			case "duplicate":
				input = append(input, input[index])
			case "unsupported-stage":
				maximum = 0
			}
			if _, err := selectF12PaymentLookupStage(input, maximum); err == nil {
				t.Fatal("historical identity mismatch accepted")
			}
		})
	}
}
