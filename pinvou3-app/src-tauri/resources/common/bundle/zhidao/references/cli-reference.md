# zhidao CLI 参考

## `download` – 文档下载

**支持两种模式**：
- 直接通过 URL 下载
- 从 `search` 命令输出的 JSON 文件（或标准输入）中选择文档下载

`search --raw-json` 仅允许作为本节自动下载流程的内部管道输入，不得用于面向用户展示搜索结果，也不得把原始 JSON 输出给用户。Windows 暂不支持 JSON 文件/标准输入模式，应改用 `download --url`。

**语法**：
```bash
# 直接 URL 模式
zhidao download --url "<URL>" [--filename NAME] [--output DIR]

# 从 JSON 文件模式
zhidao download --from-json <文件路径> [--index N] [--output DIR]

# 从标准输入模式
echo '<JSON>' | zhidao download --stdin-json [--index N] [--output DIR]
```

| 参数 | 必需 | 默认值 | 说明 |
|------|------|--------|------|
| `--url` | 三选一 | - | 直接下载链接 |
| `--from-json` | 三选一 | - | `search --raw-json` 输出保存的文件 |
| `--stdin-json` | 三选一 | - | 从管道读取 JSON |
| `--index` | ❌ | 全部下载 | JSON 中第几个文档（从1开始），不指定则下载全部 |
| `--filename` | ❌ | 自动 | 保存的文件名 |
| `--output` | ❌ | `downloads` | 输出目录 |

**自动识别 PMO 文档**：若文档来源（`source` 字段）为 `pmo` / `pm`，或 `fileWeb` 包含 `pmo.h3c.com`，则自动使用 PMO 专用 POST 下载接口。

**示例**：
```bash
# 直接下载 URL
zhidao download --url "https://pmo.h3c.com/doc/12345"

# 下载搜索结果中的第1篇文档
zhidao search --query "VLAN配置" --raw-json > result.json
zhidao download --from-json result.json --index 1

# 管道组合（搜索后直接下载第一篇）
zhidao search --query "无线配置" --raw-json | zhidao download --stdin-json --index 1
```

**默认输出**：
```
✅ 文档下载成功
📁 downloads/1_VLAN配置指南.pdf
💾 1.23 MB
```

## 常见错误处理

| 错误信息 | 原因 | 建议操作 |
|---------|------|----------|
| `Token 已过期且刷新失败，请重新登录: <URL>` | AT 和 AIT 均失效 | 将 URL 单行完整展示给用户，引导其在应用「工具商店」的「H3C 知道 · 内部知识库」卡片点「连接」完成登录后重试 |
| `未找到凭证，请先登录` | 无有效凭证 | 引导用户在应用「工具商店」的「H3C 知道 · 内部知识库」卡片点「连接」完成登录后重试 |
| `问答失败: 未找到相关答案` | 问题超出知识库范围 | 建议缩短问题或添加 `--deep` |
| `PMO下载失败 HTTP 401/403` | Token 无 PMO 权限 | 提示用户联系管理员 |
| `无效的JSON格式` | 输入文件不是合法 JSON | 确保使用 `--raw-json` 生成 JSON |

## 平台差异

Windows/macOS 版（zhidao/win-src/main.go 清洁室源码，Windows、linux/arm64 与 macOS darwin 构建同出一源）暂不支持 `--from-json`/`--stdin-json` 下载、不支持 `qa --session-id`，`clear` 会连 AIT 一起删除。Linux x86_64 为 H3C IT 官方 ELF。
