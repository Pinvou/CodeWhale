# 本地验证记录

日期：2026-09-09。版本：1.0.0-rc1。

- `python -m unittest -v test_controller.py`：19项控制器测试。涵盖模型索引、缺失分片、越界路径、文件损坏、GID交集、SSH引用和主机密钥检查、Worker失联、双机启动失败停止、镜像不一致拒绝、最终参数生成、文本/图片冒烟响应处理。
- `bash -n megacube.sh`：入口脚本语法检查。
- `build_release.py`：Python AST解析、归档Compose的patch文件依赖检查、UTF-8/LF规范化、显式文件白名单打包、SHA256清单、tar文件逐项内容回读核对。
- 包内模型代码、Compose和补丁来自本项目归档。新增控制器没有替换原研发部署脚本。

本地环境为Windows，没有Docker CLI/daemon和两台GB10直连硬件，未执行真实`docker compose config`、镜像拉取、模型下载、RDMA带宽、容器推理、网络初始化与恢复。现场执行时每次生成node.env会调用`docker compose config --quiet`；但运行时预检不等于本地已验证这些项目。

本记录不是性能验收报告。正式发布前必须完成README第6节实机测试。
