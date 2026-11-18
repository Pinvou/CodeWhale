package billing

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
)

var retainedBillingTemplates = cifixture.NewTemplatePool(os.Getenv("PINVOU_CI_RETAINED_BILLING_TEMPLATES") == "1", "billing_schema_owner", "billing_migrator", "billing_service")

func TestMain(m *testing.M) {
	code := m.Run()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := retainedBillingTemplates.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "owned fixture templates did not retire:", err)
		if code == 0 {
			code = 86
		}
	}
	os.Exit(code)
}
