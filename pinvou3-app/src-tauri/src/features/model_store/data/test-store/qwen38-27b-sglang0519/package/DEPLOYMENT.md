# Qwen3.8-27B-NVFP4 × SGLang — Deployment Reference (NVIDIA GB10 / DGX Spark)

Serve **unsloth/Qwen3.8-27B-NVFP4** as an OpenAI-compatible endpoint using the
**official, multi-arch SGLang v0.5.19 stable docker image** — no custom builds.

| Component | Value |
|---|---|
| Model | `unsloth/Qwen3.8-27B-NVFP4` (NVFP4 dynamic quant, FP8 lm_head, FP8 KV cache, MTP head included) — Apache-2.0 |
| Engine | SGLang **v0.5.19** (first stable release supporting this checkpoint — includes the FP8 `lm_head` fix, PR #35228) |
| Image | `lmsysorg/sglang:v0.5.19` (official, multi-arch amd64+arm64, CUDA 13.0.3, FlashInfer 0.6.18, ~14.7 GB compressed) |
| Hardware | NVIDIA GB10 (SM121, Blackwell, aarch64, 128 GB unified memory), driver ≥ 580 |
| OS | Ubuntu 24.04 (noble) |
| Endpoint | OpenAI-compatible API on `http://<host>:30000/v1` |

## Files

| File | Purpose |
|---|---|
| `deploy.sh` | One-shot setup: installs Docker/toolkit if missing, pulls the image (China mirror fallback chain), downloads the model (ModelScope, fallback hf-mirror), writes `.deploy-env` |
| `start.sh` | Starts the endpoint container, waits for readiness, prints a test command |
| `stop.sh` | Stops the endpoint and waits for unified memory to be reclaimed |
| `uninstall.sh` | Removes container/image/caches; model weights only on request (`--all`) |
| `DEPLOYMENT.md` | This document |

These five files are self-contained. A fresh identical machine (Ubuntu 24.04 +
GB10 + NVIDIA driver) only needs `deploy.sh` → `start.sh`.

## Quick start

```bash
chmod +x deploy.sh start.sh stop.sh uninstall.sh   # once

./deploy.sh      # 15–40 min: ~15 GB image + ~23.5 GB model (skips what exists)
./start.sh       # ~5 min to ready on GB10 (weight load + CUDA graph capture;
                 # the image ships prebuilt SM121 kernel cubins — no long JIT)

curl http://127.0.0.1:30000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen3.8-27B-NVFP4","messages":[{"role":"user","content":"Hello!"}]}'

./stop.sh
./uninstall.sh   # add --all to also delete the model weights
```

If a model copy already exists (e.g. the `unsloth--Qwen3.8-27B-NVFP4/` ModelScope
cache shipped alongside these scripts), `deploy.sh` detects and reuses it — no
re-download. To use a model at a custom path: `MODEL_DIR=/path/to/model ./deploy.sh`.

## Mainland China network notes

Everything routes through mirrors automatically; no proxy is needed.

| Resource | Primary | Fallback chain |
|---|---|---|
| Docker image | `m.daocloud.io/docker.io` (DaoCloud) | `docker.1ms.run` → `docker.m.daocloud.io` → direct Docker Hub |
| Model weights | ModelScope (`modelscope download`) | `hf-mirror.com` (`HF_ENDPOINT`) |
| pip | `pypi.tuna.tsinghua.edu.cn` | — |
| apt / Docker CE | `mirrors.aliyun.com` (`ubuntu-ports`, `docker-ce`) | — |

Notes:

- USTC/TUNA/NetEase Docker Hub proxies are dead; Alibaba's ACR accelerator no
  longer syncs new images. DaoCloud and 1ms.run were verified working from this
  network at the time of writing. Community mirrors churn — if all pulls fail,
  set `MIRROR_LIST="your.mirror/namespace"` and re-run `deploy.sh`.
- DaoCloud throttles heavy pulls; if it is slow, pulling between 01:00–07:00
  Beijing time is noticeably faster. The fallback chain handles this
  automatically most of the time.
- `nvidia-container-toolkit` comes from `nvidia.github.io`, which is generally
  reachable; it is preinstalled on DGX OS / most GB10 images anyway.

## Server configuration (built into start.sh)

Every flag below was validated on GB10 against this exact checkpoint
(single-stream, 4-way and 16-way concurrency, 260k-context stress). You can
override any of them by appending flags: `./start.sh --max-running-requests 8`.

| Flag | Value | Why |
|---|---|---|
| `--speculative-algorithm EAGLE` + `--speculative-adaptive` | on | Loads the checkpoint's MTP head; adaptive tunes draft steps by acceptance EMA (measured accept length ≈ 4.4; +26% decode vs. baseline) |
| `--enable-linear-replayssm-spec` | on | Required for GDN hybrid + speculative decoding state replay |
| `--mamba-ssm-dtype bfloat16` | — | Halves GDN state pool (~2.3 GB back to KV cache); prerequisite for the flashinfer GDN kernels |
| `--linear-attn-{decode,verify,prefill}-backend` | `flashinfer` | CuTe-DSL GDN kernels work on SM121: +7.8% aggregate at 4-way concurrency, −5.7% TTFT |
| `--mamba-radix-cache-strategy extra_buffer_lazy` | — | Long-prefix reuse with retract backup |
| `--max-mamba-cache-size` | 64 | 4 state slots/request × 16 concurrent |
| `--max-running-requests` / `--cuda-graph-max-bs-decode` | 16 | Keeps CUDA graphs aligned with the real concurrency cap |
| `--mem-fraction-static` | 0.60 | 0.75+ exhausts the 128 GB unified memory (measured freeze); 0.60 leaves ~34 GB headroom, no swap |
| `--chunked-prefill-size` | 2048 | GB10 default (8192) is measurably worse (TTFT 6.6s → 3.8s @8k) |
| `--stream-interval` | 8 | Cuts scheduler CPU overhead (single-stream P99 ITL 133→67 ms) |
| `--mm-attention-backend` | `fa4` | FlashAttention 4 (bundled in the image) is the fast ViT path on SM121 — image TTFT 0.3–0.5 s. **Do not** use `flashinfer_cudnn` here: the image's cuDNN 9.20 is older than the 9.26 that path requires and it crashes the vision tower at warmup |
| `--reasoning-parser` / `--tool-call-parser` | `qwen3` / `qwen3_coder` | Correct thinking-block and nested tool-call parsing for Qwen3.8 |
| `MAX_JOBS=2` (env) | — | Prevents 17 parallel nvcc jobs from OOM-freezing the machine during first-boot JIT compilation |

Deliberately left at defaults (auto-detection is optimal on SM121):
`--attention-backend` (auto → flashinfer), `--kv-cache-dtype` (auto → fp8_e4m3
from the checkpoint), `--sampling-backend` / `--grammar-backend`.

**Why not FA3/FA4 for the LM attention?** Verified empirically on this image:
SGLang 0.5.19 hard-whitelists LM attention backends for hybrid GDN models on
Blackwell to `{flashinfer, triton, trtllm_mha}` — launching with
`--attention-backend fa4` aborts the scheduler at startup
(`AssertionError: Only {'trtllm_mha', 'flashinfer', 'triton'} backends are
supported on Blackwell GPUs for hybrid GDN models`). FA4 is therefore used
exactly where it is allowed and works: the vision encoder. An A/B on a 1080p
image (~2.6k vision tokens) showed no measurable ViT difference
(fa4 vs triton_attn: median TTFT 1608 ms both, min 1225 vs 1303 ms), so fa4 is
kept as the validated default.

Sampling defaults come from the model's `generation_config.json`
(temperature 1.0, top_p 0.95, top_k 20 — the official thinking-mode
recommendation). Thinking mode is on by default; clients can override per
request (`"chat_template_kwargs": {"enable_thinking": false}`, official
non-thinking preset: temperature 0.7, top_p 0.80, presence_penalty 1.5).

## API usage

```bash
# Chat (thinking mode by default; reasoning in the <think> block /
# `reasoning_content` field)
curl http://127.0.0.1:30000/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "Qwen3.8-27B-NVFP4",
  "messages": [{"role": "user", "content": "Explain KV cache quantization in one paragraph."}]
}'

# Vision (image_url with https or base64 data URI)
curl http://127.0.0.1:30000/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model": "Qwen3.8-27B-NVFP4",
  "messages": [{"role": "user", "content": [
    {"type": "image_url", "image_url": {"url": "https://example.com/pic.jpg"}},
    {"type": "text", "text": "What is in this image?"}
  ]}]
}'

# Non-thinking per request
... "chat_template_kwargs": {"enable_thinking": false} ...
```

Tool calling works with the standard OpenAI `tools` schema (parsed by
`qwen3_coder`). Native context is 262,144 tokens.

## Measured performance (GB10, this configuration)

Benchmarked with `sglang.bench_serving` (random dataset, exact lengths, radix
cache flushed before every run, no warmup requests) on the official
`lmsysorg/sglang:v0.5.19` image with the exact `start.sh` configuration:

| Scenario | TTFT median | TPOT median | Throughput |
|---|---|---|---|
| 8k in / 128 out, single stream | 4.09 s (≈1030 tok/s prefill) | 29.1 ms | — |
| 2k in / 512 out, single stream | 1.00 s | 33.5 ms (≈28 tok/s) | — |
| 4k in / 512 out × 16, 8-way concurrency | 4.10 s | 78.1 ms | 82.5 tok/s aggregate |
| 64k in / 1k out, single stream | 72.0 s (≈910 tok/s prefill) | 25.9 ms | — |
| 1080p image request (≈2.6k vision tokens) | 1.6 s | — | — |

Earlier full-pass benchmarks of the same tuning set (64k/2k single stream:
TTFT ≈ 43 s, TPOT ≈ 35 ms; 16-way 4k/512: ≈ 120 tok/s aggregate, 32/32
success; KV pool 1,148,217 tokens — 4 full-262k requests resident, 5th queues
via admission control) remain representative; run-to-run variance of ±15% on
prefill-heavy workloads is normal on unified memory.

Long-context caveat: GDN prefill serializes per sequence and full-attention
layers scale quadratically — a single 262k prefill takes ~6–7 min. Queued
requests proceed automatically without slowing running streams.

## Operations

- **Startup takes ~5 min** (measured on GB10: ~2.5 min weight load, the rest
  kernel warmup and CUDA graph capture). The official image ships prebuilt
  SM121 cubins via `flashinfer-jit-cache`, so there is no long compile on
  first boot. Any additional JIT output persists in `.cache/sglang` (mounted
  into the container). `MAX_JOBS=2` is set inside the container as a safety
  net — without it, parallel nvcc jobs can OOM-freeze the machine if a kernel
  ever does need local compilation.
- **Stopping:** `./stop.sh`, then wait ~20 s before restarting so the driver
  reclaims unified memory (`MemAvailable` back above ~100 GB).
- **Logs:** `docker logs -f qwen38-sglang`.
- **Autostart:** the container runs with `--restart unless-stopped`, so it
  comes back after a reboot until you run `stop.sh`.
- **Multimodal:** images verified working. Audio/video input is unavailable —
  torchcodec has no aarch64 wheel.
- **Known caveat:** `bfloat16` SSM + replayssm prints an upstream drift warning
  (committed states are re-quantized on fold; theoretically affects very long
  sessions). No issue observed in testing. To fall back (~5–8% slower):
  `./start.sh --mamba-ssm-dtype float32 --linear-attn-decode-backend triton
  --linear-attn-verify-backend triton --linear-attn-prefill-backend triton`

## Troubleshooting

| Symptom | Fix |
|---|---|
| `docker: permission denied` | Scripts fall back to `sudo` automatically. Permanent fix: `sudo usermod -aG docker $USER`, re-login |
| Image pull fails on all mirrors | Set `MIRROR_LIST="your.registry/docker.io"` (space-separated list) and re-run `./deploy.sh` |
| Container cannot see GPU | `sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml && sudo systemctl restart docker`, then re-run `./deploy.sh` |
| Machine freezes during kernel compilation | Should not happen with `MAX_JOBS=2`; if it does, nvcc parallelism is the cause — hard reset, then restart (cache survives) |
| Crash with `cudnnGraphNotSupportedError ... cuDNN version below 9.26.0` | You overrode `--mm-attention-backend` to `flashinfer_cudnn` — the image's cuDNN is 9.20. Use the default `fa4` (or `triton_attn`) |
| Crash with `AssertionError: Only {...flashinfer, triton, trtllm_mha} backends are supported on Blackwell GPUs for hybrid GDN models` | You set `--attention-backend fa3`/`fa4` — not whitelisted for this model on Blackwell. Remove the override (auto → flashinfer) |
| Container killed (exit −15) under load | Host `earlyoom` triggered — lower memory: `MEM_FRACTION_STATIC=0.55 ./start.sh` |
| Server up but slow first request per length bucket | Normal: CUDA graphs capture per batch size on first use |
| Quality degradation in very long sessions | Use the float32-SSM fallback flags shown above |
| Benchmarking | One bench at a time; `curl -X POST :30000/flush_cache` between runs (radix cache hits inflate numbers) |

## Provenance

- Image: `lmsysorg/sglang:v0.5.19` — official multi-arch release tag
  (arm64 build: CUDA 13.0.3 / sbsa, cudnn 9.14, FlashInfer 0.6.18). Since
  v0.5.12 the plain version tags are unified for all NVIDIA GPUs; the older
  arm64-only `spark` tag is stale and must not be used for this model.
- SGLang v0.5.19 release notes list Qwen3.8-27B as a new supported model and
  include the compressed-tensors FP8 `lm_head` loader fix — the reason older
  stables (< 0.5.19) cannot serve this checkpoint.
- Model: `unsloth/Qwen3.8-27B-NVFP4` (identical files on Hugging Face and
  ModelScope), base model `Qwen/Qwen3.8-27B`, Apache-2.0.
