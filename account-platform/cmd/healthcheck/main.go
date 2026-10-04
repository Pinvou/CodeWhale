package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
)

func main() {
	if err := checkReady(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func checkReady() error {
	cfg, err := config.Load("healthcheck")
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 2 * time.Second}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	url := os.Getenv("HEALTHCHECK_URL")
	if url == "" {
		url = "http://127.0.0.1" + cfg.Address + "/health/ready"
	}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("unexpected readiness status: %d", response.StatusCode)
	}
	return nil
}
