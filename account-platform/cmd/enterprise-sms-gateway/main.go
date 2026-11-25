package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/smsgateway"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	accessKeyID, err := secret("SMS_GATEWAY_ACCESS_KEY_ID_FILE")
	if err != nil {
		return err
	}
	accessKeySecret, err := secret("SMS_GATEWAY_ACCESS_KEY_SECRET_FILE")
	if err != nil {
		return err
	}
	bearer, err := secret("SMS_GATEWAY_BEARER_TOKEN_FILE")
	if err != nil {
		return err
	}
	aliyun, err := smsgateway.NewAliyun(accessKeyID, accessKeySecret, os.Getenv("SMS_GATEWAY_SIGN_NAME"))
	if err != nil {
		return err
	}
	handler, err := smsgateway.NewHandler(bearer, smsgateway.Templates{
		Register:      os.Getenv("SMS_GATEWAY_REGISTER_TEMPLATE"),
		Login:         os.Getenv("SMS_GATEWAY_LOGIN_TEMPLATE"),
		ProfileMobile: os.Getenv("SMS_GATEWAY_PROFILE_MOBILE_TEMPLATE"),
		PasswordReset: os.Getenv("SMS_GATEWAY_PASSWORD_RESET_TEMPLATE"),
	}, os.Getenv("SMS_GATEWAY_STATE_DIR"), aliyun)
	if err != nil {
		return err
	}
	cert, key := os.Getenv("SMS_GATEWAY_TLS_CERT_FILE"), os.Getenv("SMS_GATEWAY_TLS_KEY_FILE")
	if !filepath.IsAbs(cert) || !filepath.IsAbs(key) {
		return errors.New("configure gateway TLS certificate and key files")
	}
	listen := os.Getenv("SMS_GATEWAY_LISTEN")
	if listen == "" {
		listen = ":8443"
	}
	server := &http.Server{
		Addr: listen, Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServeTLS(cert, key) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("SMS gateway could not start with the configured listener and TLS files")
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			return errors.New("SMS gateway shutdown incomplete")
		}
	}
	return nil
}

func secret(variable string) (string, error) {
	path := os.Getenv(variable)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("configure an absolute file path for %s", variable)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot read secret file for %s", variable)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("secret file for %s must be private", variable)
	}
	b, err := io.ReadAll(io.LimitReader(f, 1025))
	value := strings.TrimSpace(string(b))
	if err != nil || len(b) > 1024 || value == "" {
		return "", fmt.Errorf("invalid secret file for %s", variable)
	}
	return value, nil
}
