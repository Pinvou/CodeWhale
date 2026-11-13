package billingschema

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

var errPaymentDispatchProvenanceIdentity = errors.New("payment dispatch/provenance migration is not the frozen identity")

// Earlier partial carriers may omit these migrations. Every supplied identity
// is checked before database access, including on first install and verify.
func validatePaymentDispatchProvenance(input []migrations.Migration) error {
	for _, pin := range []struct {
		version        int64
		name, up, down string
	}{
		{900004, "refund_dispatch_fencing", "b21624f92414a8ec464138ab5bab13ac5d6e2d982881cb2b1e72ca22f7e8c694", "59c20eb8c2d538c3fd26d3967ac3c08e97dbf7293dda820238ebb2a396e748a0"},
		{900005, "payment_notification_provenance", "26c694e7b99c7d070654a2bd26d3b2af693d4a15e400c5c677fb93f5f3a88eee", "c3e0c5a8c37071cce258df5f4604d00412103abeb8ea50439d2b0bb158fea995"},
	} {
		seen := false
		for _, item := range input {
			if item.Version != pin.version {
				if item.Name == pin.name || item.Checksum == pin.up || sqlChecksum(item.UpSQL) == pin.up {
					return errPaymentDispatchProvenanceIdentity
				}
				continue
			}
			if seen || item.Name != pin.name || item.Checksum != pin.up || sqlChecksum(item.UpSQL) != pin.up || sqlChecksum(item.DownSQL) != pin.down {
				return errPaymentDispatchProvenanceIdentity
			}
			seen = true
		}
	}
	return nil
}
