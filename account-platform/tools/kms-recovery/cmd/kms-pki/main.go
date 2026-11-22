package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/pki"
)

func main() {
	config := flag.String("config", "", "absolute public configuration file")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "UNEXPECTED_ARGUMENTS")
		os.Exit(1)
	}
	cfg, err := pki.LoadConfig(*config)
	if err == nil {
		err = pki.Generate(cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("KMS_PKI_READY: CA private key encrypted; no server contacted or KMS initialized")
}
