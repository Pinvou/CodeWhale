package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/pki"
)

func main() {
	config := flag.String("config", "", "absolute public infrastructure PKI configuration")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "UNEXPECTED_ARGUMENTS")
		os.Exit(1)
	}
	cfg, err := pki.LoadInfrastructureConfig(*config)
	if err == nil {
		err = pki.GenerateInfrastructure(cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, pki.ErrInfrastructurePKI)
		os.Exit(1)
	}
	fmt.Println("INFRASTRUCTURE_PKI_READY: encrypted CA and five isolated server identities; not deployed")
}
