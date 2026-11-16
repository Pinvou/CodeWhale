package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

var errNotReady = errors.New("readiness endpoint returned a non-success status")

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if failure := readinessFailure(ctx, http.DefaultClient, os.Getenv("HEALTHCHECK_URL"), os.Getenv("APPROVAL_RELAY_HEALTHCHECK_URL")); failure != "" {
		fmt.Fprintln(os.Stderr, failure)
		os.Exit(1)
	}
}

func readinessFailure(ctx context.Context, client *http.Client, target, approvalTarget string) string {
	if target == "" {
		target = "http://127.0.0.1:8080/health/ready"
	}
	if err := checkReady(ctx, client, target); err != nil {
		if errors.Is(err, errNotReady) {
			return "READINESS_CHECK_NOT_READY"
		}
		return "READINESS_CHECK_UNAVAILABLE"
	}
	if approvalTarget != "" {
		if err := checkReady(ctx, client, approvalTarget); err != nil {
			if errors.Is(err, errNotReady) {
				return "APPROVAL_RELAY_READINESS_CHECK_NOT_READY"
			}
			return "APPROVAL_RELAY_READINESS_CHECK_UNAVAILABLE"
		}
	}
	return ""
}

func checkReady(ctx context.Context, client *http.Client, target string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errNotReady
	}
	return nil
}
