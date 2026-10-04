package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Pinvou/pinvou3/account-platform/internal/paymentsim"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
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
	deliveryDone := make(chan struct{})
	go func() { defer close(deliveryDone); service.RunDeliveryWorker(ctx) }()
	defer func() { cancel(); <-deliveryDone }()
	if err := httpserver.RunWithApplication("payment-simulator", service.Handler(os.Getenv("INTERNAL_SERVICE_TOKEN")), repository.Ping); err != nil {
		log.Fatal(err)
	}
}
