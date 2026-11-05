package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const reconciliationFairRetryVersion int64 = 900026
const reconciliationFairRetryChecksum = "bb362cb8d473ba5625986e98a2c936aa6c737ecc2bf617ac3cd935eb7c74a153"
const reconciliationFairRetryDownChecksum = "02712cfee57e182b145c3ee3708d2322fcf49e8f7c5e7d8e3e79cb70652cacbb"

func validateReconciliationFairRetry(input []migrations.Migration) error {
	for _, m := range input {
		if (m.Name == "reconciliation_fair_retry" || m.Checksum == reconciliationFairRetryChecksum) && m.Version != reconciliationFairRetryVersion {
			return errors.New("reconciliation retry progress has a different registered version")
		}
		if m.Version == reconciliationFairRetryVersion && (m.Name != "reconciliation_fair_retry" || m.Checksum != reconciliationFairRetryChecksum || sqlChecksum(m.UpSQL) != reconciliationFairRetryChecksum || sqlChecksum(m.DownSQL) != reconciliationFairRetryDownChecksum) {
			return errors.New("reconciliation retry progress is not the registered forward-only identity")
		}
	}
	return nil
}
