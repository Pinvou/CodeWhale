package billing

import (
	"encoding/json"
	"strings"
)

// legacyFinancialExecutionKeyWireDomainV1 is an immutable historical wire label,
// not the actual command type or an authorization boundary. Changing it would
// invalidate existing financial receipts and their read-only replay.
const legacyFinancialExecutionKeyWireDomainV1 = "LEDGER_REPAIR"

func legacyFinancialExecutionKeyDigestV1(key string) string {
	encoded, err := json.Marshal(map[string]any{"command_type": legacyFinancialExecutionKeyWireDomainV1, "execution_key": strings.TrimSpace(key)})
	if err != nil {
		return ""
	}
	return digestHex(encoded)
}
