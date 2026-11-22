// kms-initializer is a separate online, Linux-only operator command.
// The existing kms-recovery command remains offline.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/initialize"
)

func main() {
	config := flag.String("config", "", "absolute public configuration file")
	custody := flag.Bool("custody-backup-confirmed", false, "custodian confirmed offline backup and separate passphrase custody")
	fresh := flag.Bool("initialize-new-instance", false, "explicitly initialize the verified empty instance once")
	flag.Parse()
	if runtime.GOOS != "linux" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "LINUX_OPERATOR_AND_VALID_ARGUMENTS_REQUIRED")
		os.Exit(1)
	}
	cfg, err := initialize.LoadConfig(*config)
	if err == nil {
		err = initialize.Run(context.Background(), cfg, *custody, *fresh)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("INIT_CAPTURED: encrypted response saved; no unseal or production acceptance performed")
}
