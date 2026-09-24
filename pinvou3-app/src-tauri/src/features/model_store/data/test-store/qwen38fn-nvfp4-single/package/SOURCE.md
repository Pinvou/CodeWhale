# 来源与改动范围

原项目目录：`models/09_Qwen3.8-Flash-Next-NVFP4/`。

运行参数依据`Qwen3.8-Flash-Next-NVFP4_单机部署与实测报告.md`的9月8日最终配置，以及`third_party/Qwen3.8-Flash-Next-NVFP4-DGX-Spark/single-spark-vllm-tp1/launch/qwen38fn-nvidia-tp1.sh`。

runtime中的9份Python补丁从上述third_party的`single-spark-vllm-tp1/patch/`复制，包括本地已经修复的`upstream-overlays/modelopt.py`，不是从较早tar.gz重取未修复版本。仅统一UTF-8/LF；没有改写推理算法。上游来源说明保留在`runtime/patch/upstream-overlays/PROVENANCE.md`和`runtime/NOTICE.upstream`。

本次新增控制器、模型下载/官方摘要校验、客户README、测试、打包工具。以Docker参数数组复现归档启动参数，使用原镜像默认entrypoint；去掉原启动脚本中固定旧路径、强制docker rm和全局drop_caches操作。

rc3：修复首次镜像身份校验阻塞，固定Docker Hub可获取的linux/arm64清单，使用RepoDigests而非image ID作为跨存储后端的校验依据；新增engine入口和旧镜像锁备份迁移。镜像源证据见IMAGE-PROVENANCE.json。

旧摘要来自`results/low-concurrency-tuning-20260910/phase-a/original-inspect.json`的ImageManifestDescriptor（linux/arm64），并非已证实的配置摘要误用；本次旧摘要在Docker Hub查询返回404。新镜像与历史镜像的等价性未确认。

没有连接或改动原集群，没有把原报告的性能数据当成本次控制器验证结果。模型首次下载的版本锁定规则见README。运行参数和9份补丁沿用rc2，不代表这些补丁已在新镜像上通过推理验收。
