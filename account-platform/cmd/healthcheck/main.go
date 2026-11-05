package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	client := http.Client{Timeout: 2 * time.Second}
	target := os.Getenv("HEALTHCHECK_URL")
	if target == "" {
		target = "http://127.0.0.1:8080/health/ready"
	}
	response, err := client.Get(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "READINESS_CHECK_UNAVAILABLE")
		os.Exit(1)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		fmt.Fprintln(os.Stderr, "READINESS_CHECK_NOT_READY")
		os.Exit(1)
	}
}
