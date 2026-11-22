package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

const productAdmissionMaterialVersion int64 = 900037
const productAdmissionMaterialChecksum = "7b3ae6f7908de7fdd2e1fd227fb50e85416f3fa64f357133bd796e3465099c10"
const productAdmissionMaterialDownChecksum = "59d82853b39a25167167c3ef6f8c37c0b2d613960584be0c1678e7b628600c2b"

func validateProductAdmissionMaterials(input []migrations.Migration) error {
	for _, m := range input {
		if (m.Name == "product_admission_materials" || m.Checksum == productAdmissionMaterialChecksum) && m.Version != productAdmissionMaterialVersion {
			return errors.New("product admission material migration version mismatch")
		}
		if m.Version == productAdmissionMaterialVersion && (m.Name != "product_admission_materials" || m.Checksum != productAdmissionMaterialChecksum || sqlChecksum(m.UpSQL) != productAdmissionMaterialChecksum || sqlChecksum(m.DownSQL) != productAdmissionMaterialDownChecksum) {
			return errors.New("product admission material migration identity mismatch")
		}
	}
	return nil
}
