package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const unknownProviderEntryVersion int64 = 900025
const unknownProviderEntryChecksum = "99b24e9021eabd48e23df45c84a56835fcbbaa1b62c19813ae075246b953f7e3"
const unknownProviderEntryDownChecksum = "463559a5d7485a2fa9fc0a3fa4d4bfd4ae8b8e1a62932163470fd35ed48b5dda"

func validateUnknownProviderEntries(input []migrations.Migration) error {
	for _, m := range input {
		if (m.Name == "reconciliation_unknown_provider_entry" || m.Checksum == unknownProviderEntryChecksum) && m.Version != unknownProviderEntryVersion {
			return errors.New("unknown provider entry migration has a different frozen version")
		}
		if m.Version == unknownProviderEntryVersion && (m.Name != "reconciliation_unknown_provider_entry" || m.Checksum != unknownProviderEntryChecksum || sqlChecksum(m.UpSQL) != unknownProviderEntryChecksum || sqlChecksum(m.DownSQL) != unknownProviderEntryDownChecksum) {
			return errors.New("unknown provider entry evidence is not the registered forward-only identity")
		}
	}
	return nil
}
