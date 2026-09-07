// kms-operator is a separate interactive online command, never a secret printer.
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
	action := flag.String("action", "", "unseal, bootstrap, finalize-bootstrap, or offline stage-agents")
	config := flag.String("config", "", "absolute public operator configuration")
	dir := flag.String("directory", "", "absolute local custody directory")
	bundle := flag.String("bundle", "", "absolute encrypted package (roles.age for stage-agents; initialization package otherwise)")
	output := flag.String("output-directory", "", "private bootstrap artifact directory")
	flag.Parse()
	if flag.NArg() != 0 || (*action != "unseal" && *action != "bootstrap" && *action != "finalize-bootstrap" && *action != "stage-agents") || !term.IsTerminal(int(os.Stdin.Fd())) || (*action == "unseal" && *output != "") || (*action != "unseal" && *output == "") {
		return fmt.Errorf("LOCAL_INTERACTIVE_OPERATION_REQUIRED")
	}
	cfg, err := operator.LoadConfig(*config)
	if err != nil {
		return err
	}
	if *action == "bootstrap" {
		fmt.Fprintln(os.Stderr, "将创建固定四类最小权限身份并生成加密包，不撤销 root，不覆盖已有配置。")
	}
	if *action == "finalize-bootstrap" {
		fmt.Fprintln(os.Stderr, "将验证已有身份包与实际权限，然后撤销初始化 root；不创建或覆盖配置。")
	}
	if *action == "stage-agents" {
		fmt.Fprintln(os.Stderr, "仅离线暂存三个 Agent 的机器引导文件；不读取初始化包，不导出运维身份，不联网。")
	}
	fmt.Fprint(os.Stderr, "请输入本机恢复口令（输入不回显，不要通过聊天或 Agent 输入）： ")
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	defer clear(pass)
	if err != nil {
		return fmt.Errorf("LOCAL_PASSPHRASE_READ_FAILED")
	}
	if *action == "stage-agents" {
		if err := operator.RunStageAgentFiles(cfg, *dir, *bundle, pass, *output); err != nil {
			return err
		}
		fmt.Println("AGENT_FILES_STAGED：三个 Agent 的引导文件已受限暂存；不能作为离线恢复备份，尚未传输或启动服务。")
		return nil
	}
	if *action != "unseal" {
		if err := operator.RunBootstrap(context.Background(), cfg, *dir, *bundle, pass, *output, *action == "finalize-bootstrap"); err != nil {
			return err
		}
		if *action == "bootstrap" {
			fmt.Println("BOOTSTRAP_PREPARED：身份已加密保存并验证，尚未撤销 root。")
		} else {
			fmt.Println("BOOTSTRAP_FINALIZED：初始化 root 已撤销，四类身份已再次验证；尚未完成业务部署。")
		}
		return nil
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
