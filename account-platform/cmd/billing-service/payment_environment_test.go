package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func TestPaymentFactoryAllowsStartupAndFencesTestOnlyCollection(t *testing.T) {
	for _, environment := range []string{"", "development", "test", "staging", "production"} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("APP_ENV", environment)
			t.Setenv("BILLING_PAYMENT_PROVIDER", "simulator")
			t.Setenv("PAYMENT_SIMULATOR_URL", "http://127.0.0.1:1")
			t.Setenv("INTERNAL_SERVICE_TOKEN", strings.Repeat("t", 32))
			t.Setenv("BILLING_PAYMENT_CALLBACK_URL", "https://controlled.invalid/callback")
			for _, channel := range []string{"UNIONPAY", "ALIPAY", "WECHAT"} {
				t.Setenv("PAYMENT_SIM_HMAC_"+channel, strings.Repeat("h", 32))
			}
			cfg, err := config.Load("billing-service")
			if err != nil {
				t.Fatal(err)
			}
			pool := testdb.Open(t, "billing")
			repo, err := billing.NewPostgresRepository(context.Background(), pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(repo.Close)
			coordinator, err := paymentCoordinator(context.Background(), repo, cfg.Environment)
			if err != nil {
				t.Fatal("production startup factory rejected test-only provider", err)
			}
			if err := repo.Ping(context.Background()); err != nil {
				t.Fatal(err)
			}
			if coordinator.Capabilities().FormalCollectionAdmitted {
				t.Fatal("test-only provider admitted formal collection")
			}
			products, err := repo.ListProducts(context.Background(), "CNY", "ALIPAY")
			if err != nil {
				t.Fatal(err)
			}
			if environment == "production" || environment == "staging" {
				if len(coordinator.FilterProducts(products)) != 0 {
					t.Fatal("production advertised test-only collection products")
				}
				wallet, _, err := repo.EnsureWallet(context.Background(), "ORGANIZATION", uuid.New())
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := coordinator.CreateOrder(context.Background(), uuid.New(), wallet.ID, "CNY_1", "ALIPAY", uuid.NewString()); !errors.Is(err, billing.ErrConflict) {
					t.Fatal("collection reached unavailable provider instead of runtime gate", err)
				}
			}
		})
	}
}
