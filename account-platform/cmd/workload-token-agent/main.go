package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/workload"
)

func main() {
	path := flag.String("config", "", "absolute path to non-secret workload configuration")
	check := flag.Bool("check", false, "verify the projected token without requesting a new one")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected workload arguments")
	}
	if !*check && runtime.GOOS != "linux" {
		log.Fatal("workload token projection service requires Linux")
	}
	config, err := workload.ReadConfig(*path)
	if err != nil {
		log.Fatal(err)
	}
	agent, err := workload.New(config)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *check {
		if err := agent.Check(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := agent.Run(ctx, func(status string) { log.Print(status) }); err != nil && ctx.Err() == nil {
		log.Fatal("workload agent stopped unexpectedly")
	}
}
