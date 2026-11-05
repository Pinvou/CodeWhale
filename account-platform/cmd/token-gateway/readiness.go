package main

import (
	"context"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func tokenReadiness(environment string, ping func(context.Context) error, remaining func(context.Context) (int64, error)) func(context.Context) error {
	if !config.IsProduction(environment) {
		return ping
	}
	return func(ctx context.Context) error {
		if err := ping(ctx); err != nil {
			return err
		}
		count, err := remaining(ctx)
		if err != nil {
			return err
		}
		if count != 0 {
			return token.ErrSecretRotationPending
		}
		return nil
	}
}
