package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/newapitls"
)

func main() {
	server, err := newapitls.NewServer(":8443", os.Getenv("NEWAPI_TLS_CERT_FILE"), os.Getenv("NEWAPI_TLS_KEY_FILE"))
	if err != nil {
		log.Fatal("New API TLS certificate configuration is invalid")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("New API TLS listener failed")
	}
}
