# rc3镜像校验修复与升级

版本：1.0.0-rc3，2026-09-15。候选修复包，需完成新镜像GB10推理验收后再作为生产版本。

## 修复内容

- rc1/rc2拉取nightly标签，却要求匹配历史`5cba4263…`，反馈设备拉到另一份镜像后被拒绝。
- rc3直接固定Docker Hub已核实的ARM64清单`a551e053…`，不拉浮动标签、不放宽为任意镜像。
- 使用完整RepoDigest与平台校验，运行按摘要引用，不依赖Docker存储后端的image ID语义。
- 新增`bash megacube.sh engine`，用于LMM“安装引擎”：拉取/复用镜像、校验身份、检查GPU及9个补丁目标路径；不下载模型或启动推理。
- `state/image.json`升级为schema 2；成功核对新镜像后备份旧锁。失败不覆盖旧锁，不删除旧镜像。

## 新安装

将tar.gz和同名.sha256文件放在同一目录：

```bash
sha256sum -c MegaCube-Qwen3.8-Flash-Next-NVFP4-single-1.0.0-rc3.tar.gz.sha256
tar -xzf MegaCube-Qwen3.8-Flash-Next-NVFP4-single-1.0.0-rc3.tar.gz
cd megacube-qwen38fn-single
sha256sum -c SHA256SUMS
bash megacube.sh engine
bash megacube.sh deploy
```

`engine`成功仅证明镜像和基础GPU/路径检查通过。`deploy`还会下载并校验权重、加载模型并执行真实生成请求，可能耗时较长。

## 从rc1/rc2升级，保留已有模型

以下假设安装路径为`$HOME/megacube-qwen38fn-single`，新包放在`$HOME`。如果实际位置不同，请替换路径；必须原地更新，以保留安装目录标签和已有配置。

```bash
cd "$HOME/megacube-qwen38fn-single"
bash megacube.sh stop
# 备份体积小的程序、配置、状态；不复制大模型或编译缓存。
backup="$HOME/megacube-qwen38fn-before-rc3-$(date +%Y%m%d-%H%M%S).tar.gz"
tar --exclude='./models' --exclude='./cache' --exclude='./logs' \
    --exclude='./.operation.lock' -czf "$backup" .

cd "$HOME"
sha256sum -c MegaCube-Qwen3.8-Flash-Next-NVFP4-single-1.0.0-rc3.tar.gz.sha256
tar -xzf MegaCube-Qwen3.8-Flash-Next-NVFP4-single-1.0.0-rc3.tar.gz
cd megacube-qwen38fn-single
sha256sum -c SHA256SUMS
bash megacube.sh engine
bash megacube.sh deploy
```

若自定义模型或缓存目录位于安装目录内，备份命令也应排除该自定义目录，避免把大文件打入程序备份。运行升级前确保没有其他部署操作在执行。

新包不包含config.json、state、模型或缓存文件，因此原地解压会更新程序并保留这些数据。`engine`迁移前自动保存`state/image.previous-*.json`。`deploy`沿用已有权重来源锁并校验/修复文件；不会为升级主动删除或强制重下全部权重。若权重已有有效验证记录，可在engine后用`start`代替deploy。

回退时先用rc3停止服务，再在原安装目录恢复上面的程序备份（含旧state/image.json）。旧镜像必须仍在原机器上；旧摘要仓库查询为404，因此不能保证回退时能从公网重新获取。不要删除旧镜像或旧程序备份。

## LMM接入

“安装引擎”调用`bash megacube.sh engine`并读取退出码，避免修改controller.py内部常量或跳过校验。其余入口沿用原有接口；本次没有把deploy改成只生成配置，deploy仍会启动模型。

## 验收与报错取证

```bash
docker image inspect vllm/vllm-openai@sha256:a551e05307cd2e0092139d84db32af9c97e67d2eeeff072d21e429131d8c23f0
cat state/image.json
bash megacube.sh status
bash megacube.sh smoke-test
bash megacube.sh stop
bash megacube.sh start
```

固定镜像包含的补丁目标路径与GPU检查失败时，保留engine输出；模型加载或生成失败时，保留logs输出及logs目录中的归档。不要删除镜像校验、把配置摘要当作清单摘要，或改用latest。验证范围与未完成项目见VALIDATION.md。
