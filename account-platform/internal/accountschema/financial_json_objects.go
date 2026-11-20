package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"strings"
)

const financialJSONObjectVersion int64 = 900024
const financialJSONObjectName = "financial_json_object_contracts"
const financialJSONObjectUp = "2ae6887caa0f1641e54b1b953f9e949fd35482e0242ba26fadd3bf4c35f99694"
const financialJSONObjectDown = "d3a0e4b0e826c1ae512e9011c387dd33992c30b10c52500aff50fe5ac7e41108"

func financialJSONObjectChecksum(sql string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")))
	return hex.EncodeToString(sum[:])
}

// Earlier packages remain valid. A supplied guard must keep both published
// directions and its full identity before any migration DB operation starts.
func validateFinancialJSONObjects(input []migrations.Migration) error {
	seen := false
	for _, m := range input {
		actual := financialJSONObjectChecksum(m.UpSQL)
		if m.Version != financialJSONObjectVersion && m.Name != financialJSONObjectName && m.Checksum != financialJSONObjectUp && actual != financialJSONObjectUp {
			continue
		}
		if seen || m.Version != financialJSONObjectVersion || m.Name != financialJSONObjectName || m.Checksum != financialJSONObjectUp || actual != financialJSONObjectUp || financialJSONObjectChecksum(m.DownSQL) != financialJSONObjectDown {
			return errors.New("financial JSON objects require the published forward-only migration")
		}
		seen = true
	}
	return nil
}
