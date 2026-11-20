package accountschema

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"strings"
)

const commerceManualCompletionVersion int64 = 900025
const commerceManualCompletionName = "commerce_manual_completion"
const commerceManualCompletionUp = "9fe562a84685342a5c71047acdba6d8587f1148199354417b1d647d6b27a01f5"
const commerceManualCompletionDown = "2749336f31232b322f7986c451da63173146d57d3b659831940c1140c62024d5"

func commerceManualCompletionChecksum(sql string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")))
	return hex.EncodeToString(sum[:])
}

// Earlier packages remain valid. A supplied guard must keep both published
// directions and its full identity before any migration DB operation starts.
func validateCommerceManualCompletion(input []migrations.Migration) error {
	seen := false
	for _, m := range input {
		actual := commerceManualCompletionChecksum(m.UpSQL)
		if m.Version != commerceManualCompletionVersion && m.Name != commerceManualCompletionName && m.Checksum != commerceManualCompletionUp && actual != commerceManualCompletionUp {
			continue
		}
		if seen || m.Version != commerceManualCompletionVersion || m.Name != commerceManualCompletionName || m.Checksum != commerceManualCompletionUp || actual != commerceManualCompletionUp || commerceManualCompletionChecksum(m.DownSQL) != commerceManualCompletionDown {
			return errors.New("commerce manual completions require the published forward-only migration")
		}
		seen = true
	}
	return nil
}
