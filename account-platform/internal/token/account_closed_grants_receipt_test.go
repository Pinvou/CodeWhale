package token

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func TestClosedGrantReceiptBindsCanonicalFinancialTarget(t *testing.T) {
	org, grant := uuid.New(), uuid.New()
	valid := fmt.Sprintf(`{"grant":{"grant_id":%q,"organization_id":%q,"status":"RECLAIMED_ACCOUNT_CLOSED"},"amount_microcredits":9007199254740993,"subject_lifecycle_version":2,"replayed":true}`, grant, org)
	if err := validateClosedGrantReceipt(json.RawMessage(valid), org, grant); err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		name       string
		payload    string
		org, grant uuid.UUID
	}{
		{"wrong organization", valid, uuid.New(), grant},
		{"wrong grant", valid, org, uuid.New()},
		{"missing fields", `{"replayed":false}`, org, grant},
		{"overflow", fmt.Sprintf(`{"grant":{"grant_id":%q,"organization_id":%q,"status":"RECLAIMED_ACCOUNT_CLOSED"},"amount_microcredits":9223372036854775808,"subject_lifecycle_version":2,"replayed":false}`, grant, org), org, grant},
	} {
		t.Run(input.name, func(t *testing.T) {
			if validateClosedGrantReceipt(json.RawMessage(input.payload), input.org, input.grant) == nil {
				t.Fatal("noncanonical receipt accepted")
			}
		})
	}
}
