// kms-snapshot is a Linux-only, one-shot encrypted backup worker, not a restore tool.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/operator"
)

func run() error {
	config := flag.String("config", "", "absolute public backup configuration")
	flag.Parse()
	if runtime.GOOS != "linux" || flag.NArg() != 0 || *config == "" {
		return operator.ErrConfig
	}
	cfg, err := operator.LoadSnapshotConfig(*config)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := operator.CaptureSnapshot(ctx, cfg); err != nil {
		return err
	}
	fmt.Println("KMS_SNAPSHOT_CAPTURED_ENCRYPTED_NOT_OFFLINE_CONFIRMED")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
