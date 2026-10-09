package billingschema

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func validateMemberFinancialReviewReason(input []migrations.Migration) error {
	var lifecycle, targeted bool
	var guard *migrations.Migration
	for _, m := range input {
		if m.Version == 20 {
			lifecycle = true
		}
		if m.Version == 27 {
			targeted = true
		}
		if m.Version == 900012 {
			if guard != nil {
				return errors.New("duplicate member financial reason migration")
			}
			copy := m
			guard = &copy
		}
	}
	if (lifecycle && targeted) != (guard != nil) {
		return errors.New("member financial review requires its mature reason migration")
	}
	if guard != nil && (guard.Name != "member_financial_review_reason" || guard.Checksum != "337f8cec282e0712b38ff5d5cd846d75f87b7e19e8a11ffd314fd2c4ee598427" || sqlChecksum(guard.UpSQL) != "337f8cec282e0712b38ff5d5cd846d75f87b7e19e8a11ffd314fd2c4ee598427" || sqlChecksum(guard.DownSQL) != "2b3893ff6a8672eb7ba423d3fa52a8e7d2af9cdd1ff85ba84d7e5abcd41ed7ba") {
		return errors.New("member financial review reason is not the frozen forward-only migration")
	}
	return nil
}
