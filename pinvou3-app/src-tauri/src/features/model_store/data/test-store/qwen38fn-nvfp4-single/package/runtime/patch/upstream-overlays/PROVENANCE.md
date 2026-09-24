# Provenance of the files in this directory

<!-- [LMM 本地补丁 2026-09-22] 这张来源表在交付时有三处对不上，都在本次改掉，
     技术内容和署名一字未动：
     1) 正文原先把审计结论挂在 `../../research/credits-audit-2026-09-05.md` 上，
        那是交付方的内部路径，本包里不存在——按 CONTRIBUTING.md 不得提交内部
        信息，改成如实说明"审计文档不随包交付"。
     2) modelopt.py 那行原先引用 `../modelopt_mtp_index.diff`，runtime/patch/ 下
        没有这个文件（整包也没有任何 .diff）——删掉悬空引用，改为直接写明改了
        什么，与 README 第 5 节的描述一致。
     3) modelopt.py 记的 sha256 是 eff707e7f42b12a0…，与实际文件（也与包自带的
        SHA256SUMS）不符，应为 aefff33c2e1166cd…，疑似 rc3 换了 modelopt.py 之后
        这张表没同步。其余 4 个 overlay 的记录核对无误，未改。 -->

Base: vLLM main at commit 8a728663c1c3eeace834a95f5654fa653cc1998c (nightly image `vllm/vllm-openai:nightly-8a728663c1c3eeace834a95f5654fa653cc1998c`, 2026-09-04). Each file below is that base file with exactly the stated change applied; a credits audit performed on 2026-09-05 re-derived them and confirmed byte identity (that audit document is not shipped with this package).

| File | Mounted at | Base + change | Author of the change | sha256 |
|---|---|---|---|---|
| ops_ple.py | vllm/models/qwen4_exp/nvidia/ops/ple.py | base + PR #55375 (fused PLE conv state-index stride fix, merged 2026-09-05) | peakcrosser7 | cc8994b653dd4b59d6ba35b6ec10af34017e9944482ec67cad6a32a02c5dd00a |
| ops_qsa.py | vllm/models/qwen4_exp/nvidia/ops/qsa.py | base + PR #54846 (fp8_e4m3 / nvfp4 KV on the QSA path, open; rebased onto the nightly, 3-way merge conflict-free) | andreasgru | 1e928e2994233496e9ef447768e8b63acbedc22e242d4b2fc9f4ed861a1616c3 |
| qsa.py | vllm/models/qwen4_exp/nvidia/qsa.py | base + PR #54846 (verbatim the PR-head file 1e11f40bbc) | andreasgru | f2fc3fb43f6c6e84e892a995697fb59a85ee8df2ef2959943c96cf0233078da7 |
| platforms_interface.py | vllm/platforms/interface.py | base + PR #54846 | andreasgru | 443b556b984b306c0274c58af8658ef853c664b8beae6deefda41908f789d26d |
| modelopt.py | vllm/model_executor/layers/quantization/modelopt.py | base + our two MTP-loading fixes (MTP RoutedExperts use explicit dynamic activation, and the 128×128-block FP8 method, avoiding the `weight_scale_inv` load error); applied in place, no separate diff ships in this package | Kai (Tech2Wild), 2026-09-05; same gaps fixed independently the same day by sfxnz (MIT) and MiaAI-Lab (AGPL), no shared code | aefff33c2e1166cdebe9a91b863650eb64079e01aeb5604cb4110bab2d1ae4eb |

All files keep vLLM's Apache-2.0 SPDX header. vLLM is Copyright contributors to the vLLM project, Apache-2.0.
