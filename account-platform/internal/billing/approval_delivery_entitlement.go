package billing

import (
	"encoding/json"
)

func entitlementAuthorizationResult(result EntitlementCorrectionResult) (json.RawMessage, json.RawMessage, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, nil, err
	}
	ledger, err := json.Marshal(map[string]any{"journal_entry_id": result.JournalEntryID})
	return encoded, ledger, err
}
