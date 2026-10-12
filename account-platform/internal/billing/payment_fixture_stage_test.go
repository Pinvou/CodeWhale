package billing

import (
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"reflect"
	"testing"
)

func TestPaymentFixtureStageSelection(t *testing.T) {
	loaded := []migrations.Migration{{Version: 55}, {Version: 56}, {Version: 57}, {Version: 58}, {Version: 59}, {Version: 60}, {Version: 900013}, {Version: 900014}, {Version: 900015}, {Version: 900016}, {Version: 900017}}
	cases := []struct {
		maximum int64
		want    []int64
	}{
		{0, []int64{55, 56, 57, 58, 59, 60, 900013, 900014, 900015, 900016, 900017}},
		{56, []int64{55, 56, 60, 900014, 900015, 900016, 900017}},
		{58, []int64{55, 56, 57, 58, 60, 900014, 900015, 900016, 900017}},
		{59, []int64{55, 56, 57, 58, 59, 60, 900013, 900014, 900015, 900016, 900017}},
		{900016, []int64{55, 56, 57, 58, 59, 60, 900013, 900014, 900015, 900016}},
		{900017, []int64{55, 56, 57, 58, 59, 60, 900013, 900014, 900015, 900016, 900017}},
	}
	for _, tc := range cases {
		t.Run(stringifyFixtureStage(tc.maximum), func(t *testing.T) {
			var got []int64
			for _, m := range selectEntitlementFixtureStage(loaded, tc.maximum) {
				got = append(got, m.Version)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("selected versions=%v want=%v", got, tc.want)
			}
		})
	}
}

func stringifyFixtureStage(value int64) string { return fmt.Sprint(value) }
