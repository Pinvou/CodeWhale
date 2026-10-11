// kms-operator performs interactive unsealing; it never prints secret material.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/operator"
	"golang.org/x/term"
)

func run() error {
	action := flag.String("action", "", "unseal")
	config := flag.String("config", "", "absolute public operator configuration")
	dir := flag.String("directory", "", "absolute local custody directory")
	bundle := flag.String("bundle", "", "absolute encrypted initialization package")
	flag.Parse()
	if flag.NArg() != 0 || *action != "unseal" || !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("LOCAL_INTERACTIVE_OPERATION_REQUIRED")
	}
	cfg, err := operator.LoadConfig(*config)
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "请输入本机恢复口令（输入不回显，不要通过聊天或 Agent 输入）： ")
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	defer clear(pass)
	if err != nil {
		return fmt.Errorf("LOCAL_PASSPHRASE_READ_FAILED")
	}
	changed, err := operator.Run(context.Background(), cfg, *dir, *bundle, pass)
	if err != nil {
		return err
	}
	if changed {
		fmt.Println("KMS_UNSEALED：目标已解封；未发送 root token，未完成部署验收。")
	} else {
		fmt.Println("KMS_ALREADY_UNSEALED：目标已解封，未重复提交材料。")
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
