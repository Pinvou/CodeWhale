# Compose 挂载编码回归夹具

`probe.yaml` 只用于离线 `config`；镜像地址是不可用的公开测试标记，不得启动容器。
两个 JSON 是相同输入经官方 Windows x86_64 Compose 二进制输出的原始 stdout，未补写缺省字段。

| Compose | 官方二进制 SHA-256 | 显式 false | 显式 true / 短语法 |
|---|---|---|---|
| [2.39.4](https://github.com/docker/compose/releases/tag/v2.39.4) | `6b3bccfabcdd172e1d9e15d011b54c9b5b13b93b1153148108f55e4349055955` | `bind: {}` | `create_host_path: true` |
| [5.4.0](https://github.com/docker/compose/releases/tag/v5.4.0) | `d51bc731b3ff6f062a26e8fdfd391ae98aeab516432f097c66d39c1c9d06680e` | `create_host_path: false` | `bind: {}` |

在本目录重现（先验证官方二进制校验和，只解析，不需要 daemon）：

```sh
docker compose --project-name public-bind-config-probe -f - config --format json < probe.yaml
```

Windows 使用 `COMPOSE_CONVERT_WINDOWS_PATHS=1`；独立二进制用其可执行文件替换 `docker compose`。
原始输出 SHA-256：2.39.4 为 `7472282139b08bd73d4a2eac29271d23e795629e81a86405a72288e3532454a3`；5.4.0 为 `b180779290059e9928e16af320c75256aa528e99abaabed598c4799915471eac`。

编码差异见 [compose-go 2.9.0](https://github.com/compose-spec/compose-go/blob/v2.9.0/types/types.go) 的布尔字段 `omitempty` 与 [2.14.0](https://github.com/compose-spec/compose-go/blob/v2.14.0/types/types.go) 的 `OptOut` / `omitzero`。夹具版本只用于回归断言，不作为正式检查器的信任输入；正式检查器一律拒绝缺省字段，仅接受明确的布尔值 false。因此旧版安全配置也会因输出有歧义而被拒绝，必须换用保留 false 的 renderer，从原配置重新渲染。
