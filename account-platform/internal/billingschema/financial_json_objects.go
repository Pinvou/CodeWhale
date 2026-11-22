package billingschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"strings"
)

const financialJSONObjectVersion int64 = 900042
const financialJSONObjectName = "financial_json_object_contracts"
const financialJSONObjectUp = "a25714955d96b06f5cfec08a412618bc4e9dc16ed7b91dabdfc51a3ee0cb7d10"
const financialJSONObjectDown = "a95c55e4e8a7bb566f15df7fc3b4d0ee9a268587221329c5d5757dc6c26e0fdc"

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
