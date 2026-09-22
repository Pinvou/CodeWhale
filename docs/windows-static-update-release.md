# Windows 静态更新人工发布

Windows 正式安装包必须在独立签名服务器上构建并签署。发布端不持有签名凭据，
只负责核对签名服务器交付物、生成静态清单、人工上传并验证线上文件。

## 客户端更新协议

客户端固定读取 `https://pinvou.com/pinvou3/latest.json` 的
`platforms["windows-x86_64"]`：

```json
{
  "platforms": {
    "windows-x86_64": {
      "version": "0.9.0",
      "url": "https://pinvou.com/pinvou3/pinvou3_0.9.0_x64-setup.exe",
      "format": "nsis",
      "sha256": "<64 位十六进制 SHA-256>",
      "size": 123456789,
      "restart_after_install": false,
      "notes": "Windows 更新说明",
      "pub_date": "2026-09-22T08:00:00Z"
    }
  }
}
```

- Windows 不读取顶层 Linux 资产，也不回退到其他平台。
- 正式清单和安装器 URL 必须使用 HTTPS。
- `format` 支持 `nsis` 和 `msi`；当前正式发布使用 NSIS。
- 版本只能是三段或四段纯数字，且必须高于线上 Windows 版本。
- SHA-256 和 `size` 均为强校验；任一不一致都会拒绝安装。

## 1. 签名服务器交付

签名服务器检出确定的 Git commit，按
[`Windows-WoSign签名构建.md`](Windows-WoSign签名构建.md) 执行正式 `build:nsis`。
交付以下信息：

```text
pinvou3_<version>_x64-setup.exe
Git commit
安装包 SHA-256
```

正式发布不得使用 `build:nsis:local` 生成的未签名测试包。签名流程仍以沃通工具退出码
为成功依据，不增加与当前 UKey 链不兼容的 Authenticode 二次阻断校验。

## 2. 发布端复核交付物

1. 在目标 Git commit 对应的仓库中确认根目录 `VERSION`。
2. 重新计算安装包 SHA-256，与签名服务器交付值比较。
3. 确认文件名严格为 `pinvou3_<VERSION>_x64-setup.exe`。
4. 从线上下载当前 `latest.json` 到本地，不得从空对象重建已有清单。

## 3. 离线生成发布文件

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\prepare-windows-release.ps1 `
  -InstallerPath "D:\release\pinvou3_0.9.0_x64-setup.exe" `
  -Version "0.9.0" `
  -GitCommit "<签名服务器构建 commit>" `
  -ExpectedSha256 "<签名服务器交接单中的 SHA-256>" `
  -Notes "本次 Windows 更新说明" `
  -CurrentManifest "D:\release\latest.json" `
  -OutputManifest "D:\release\latest.json.new" `
  -OutputReleaseInfo "D:\release\pinvou3_0.9.0_windows-release-info.json"
```

该脚本完全离线，不上传文件，也不读取签名凭据。它会：

- 校验仓库 `VERSION`、参数版本和安装包文件名一致；
- 阻止同版本覆盖或版本回退；
- 重新计算 SHA-256，与签名服务器交接值比较，并读取文件大小；
- 校验正式安装器基地址是 HTTPS；
- 保留顶层 Linux 字段和所有其他平台条目；
- 只替换 `platforms["windows-x86_64"]`；
- 以 UTF-8 无 BOM 写出 `latest.json.new` 和发布记录。

## 4. 人工上传

上传顺序不可颠倒：

1. 上传签名安装器，文件名包含版本，不覆盖历史文件。
2. 用下面的工具从线上重新下载该安装器，核对大小和 SHA-256：

   ```powershell
   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\verify-windows-release-online.ps1 `
     -Url "https://pinvou.com/pinvou3/pinvou3_0.9.0_x64-setup.exe" `
     -ExpectedSha256 "<release-info 中的 SHA-256>" `
     -ExpectedSize <release-info 中的字节数>
   ```

3. 上传 `latest.json.new`。
4. 在服务端将 `latest.json.new` 原子重命名为 `latest.json`。
5. 回读线上 `latest.json`，确认 Windows 版本、URL、大小和 SHA-256。
6. 使用一台旧版本 Windows 设备完成检查、下载、取消、UAC、覆盖安装和新版拉起验证。

只有安装器已经在线且校验一致后，才能替换清单。Linux/macOS 发布脚本也必须继续使用
“读取现有清单后合并”的方式，不能抹掉 Windows 节点。

## 5. 回滚与修复

- 历史安装器保留为不可变文件，不得用相同版本号覆盖内容。
- 发布窗口内发现问题时，可将 Windows 平台节点恢复到上一版本，阻止更多设备获取问题版本。
- 已安装问题版本的设备不会自动降级；必须发布版本号更高的修复包。
- 每次发布保存 release-info、安装包 SHA-256、Git commit、发布时间和操作人记录。

## 6. 从旧 Windows OTA 迁移

旧客户端不会读取静态清单。切换时需先通过旧更新渠道发布一次桥接版本；桥接版本开始使用
本协议。桥接包可暂时使用 `scripts/build-windows-ota.ps1` 生成；该脚本只为旧协议迁移保留，
不能继续作为常规发布入口。确认主要设备已经升级后，后续版本即可只维护静态安装包和
`latest.json`。
