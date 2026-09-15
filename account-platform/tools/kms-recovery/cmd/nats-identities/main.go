package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/natsidentity"
)

func main() {
	config := flag.String("config", "", "absolute public NATS identity bundle configuration")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "UNEXPECTED_ARGUMENTS")
		os.Exit(1)
	}
	cfg, err := natsidentity.LoadConfig(*config)
	if err == nil {
		err = natsidentity.Generate(cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, natsidentity.ErrIdentity)
		os.Exit(1)
	}
	version := cfg.ArtifactVersion
	if version == 0 {
		version = 1
	}
	fmt.Printf("NATS_IDENTITIES_READY_V%d: encrypted custody and closed isolated identity bundle; not deployed\n", version)
}
