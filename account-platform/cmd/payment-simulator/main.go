package main

import (
	"log"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func main() {
	if err := httpserver.Run("payment-simulator"); err != nil {
		log.Fatal(err)
	}
}
