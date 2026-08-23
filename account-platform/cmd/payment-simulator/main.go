package main

import (
	"context"
	"log"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/internal/paymentsim"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("payment-simulator"); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	repository, err := paymentsim.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"), os.Getenv("PAYMENT_SIM_PUBLIC_URL"), os.Getenv("PAYMENT_SIM_PAY_TOKEN_SECRET"), map[string]string{
		"UNIONPAY": os.Getenv("PAYMENT_SIM_HMAC_UNIONPAY"),
		"ALIPAY":   os.Getenv("PAYMENT_SIM_HMAC_ALIPAY"),
		"WECHAT":   os.Getenv("PAYMENT_SIM_HMAC_WECHAT"),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer repository.Close()
	service := paymentsim.NewService(repository)
	go service.RunDeliveryWorker(ctx)
	if err := httpserver.RunWithApplication("payment-simulator", service.Handler(os.Getenv("INTERNAL_SERVICE_TOKEN")), repository.Ping); err != nil {
		log.Fatal(err)
	}
}
