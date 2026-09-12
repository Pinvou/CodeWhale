#!/usr/bin/env bash
# CI Linux runner memory setup (best effort, never fatal).
#
# Standard GitHub-hosted Linux runners provide 16 GiB RAM and a small /mnt
# swapfile. The pinvou3 workspace (700+ crates, ThinLTO, dep-level O2)
# repeatedly exhausts that budget in rust-test; before memory provisioning
# existed the failure mode was "hosted runner lost communication" with all
# logs lost.
#
# This script enlarges effective memory at job start (every Linux job runs it
# right after checkout):
#   - a 16 GiB /mnt/swapfile (replaces the image's smaller one) as the
#     overflow layer;
#   - kernel tunables: vm.swappiness=130 (>=100 shifts reclaim towards
#     anonymous pages, i.e. actually use the swap we just built),
#     vm.watermark_scale_factor=300 and vm.min_free_kbytes=65536 (wake kswapd
#     earlier and keep a deeper emergency reserve so the runner agent's
#     heartbeat survives link peaks), vm.overcommit_memory=1 (never refuse
#     mmap; let swap absorb peaks instead of failing allocations).
#
# zram (24 GiB lz4, priority 100, compressed pages never touch disk) is the
# preferred first layer and is ON by default for self-hosted runners that can
# load the module, but is DISABLED unless PINVOU3_CI_ENABLE_ZRAM=1: on the
# hosted ubuntu-24.04 image the module load path froze/killed the runner VM
# (observed 2026-09: every job died inside this step with no logs uploaded),
# and a compressed-RAM layer is worthless if the runner never survives to use
# it. Every degradation is loud but non-fatal, so a runner-image change can
# never break unrelated jobs.

set -uo pipefail

log() { echo "[memory-setup] $*"; }
warn() { echo "[memory-setup] WARNING: $*" >&2; }

if [[ ${EUID} -ne 0 ]]; then
  warn "must run as root (invoke as: sudo bash scripts/ci-memory-setup.sh)"
  exit 0
fi

DISK_SWAP_SIZE=16G
DISK_SWAP_PRIORITY=10

# sysctl tuning: each knob is independent and non-fatal.
sysctl -w vm.swappiness=130 >/dev/null 2>&1 \
  || warn "vm.swappiness=130 rejected by the running kernel"
sysctl -w vm.watermark_scale_factor=300 >/dev/null 2>&1 \
  || warn "vm.watermark_scale_factor=300 rejected"
sysctl -w vm.min_free_kbytes=65536 >/dev/null 2>&1 \
  || warn "vm.min_free_kbytes=65536 rejected"
sysctl -w vm.overcommit_memory=1 >/dev/null 2>&1 \
  || warn "vm.overcommit_memory=1 rejected"
log "sysctl tuned (best effort): swappiness=130 watermark_scale_factor=300 min_free_kbytes=65536 overcommit_memory=1"

setup_disk_swap() {
  # The runner image pre-creates a swapfile on /mnt via waagent; replace it.
  if swapon --show=NAME --noheadings 2>/dev/null | grep -qx '/mnt/swapfile'; then
    swapoff /mnt/swapfile 2>/dev/null || warn "could not swapoff the image swapfile; leaving it in place"
  fi
  if ! fallocate -l "${DISK_SWAP_SIZE}" /mnt/swapfile 2>/dev/null; then
    warn "fallocate ${DISK_SWAP_SIZE} /mnt/swapfile failed; keeping the existing swap configuration"
    return 0
  fi
  chmod 600 /mnt/swapfile
  mkswap /mnt/swapfile >/dev/null 2>&1 || { warn "mkswap failed"; return 0; }
  swapon -p "${DISK_SWAP_PRIORITY}" /mnt/swapfile 2>/dev/null \
    || warn "swapon /mnt/swapfile failed; keeping the existing swap configuration"
  log "disk swap ready: /mnt/swapfile ${DISK_SWAP_SIZE}, priority ${DISK_SWAP_PRIORITY}"
}

# zram and zswap overlap: zswap is only enabled when the zram device could not
# be created, so the "compress in RAM first" layer exists exactly once.
setup_zswap_fallback() {
  local params=/sys/module/zswap/params
  if [[ ! -d ${params} ]]; then
    warn "zswap unavailable too; continuing with plain disk swap"
    return 0
  fi
  echo 1 >"${params}/enabled" 2>/dev/null || true
  if echo lz4 >"${params}/compressor" 2>/dev/null; then
    log "zswap compressor set to lz4"
  else
    warn "zswap rejected lz4; keeping kernel default compressor"
  fi
  echo zsmalloc >"${params}/zpool" 2>/dev/null || true
  echo 25 >"${params}/max_pool_percent" 2>/dev/null || true
  log "zswap fallback enabled: compressed RAM pool (25% of RAM) in front of disk swap"
}

setup_disk_swap

if [[ ${PINVOU3_CI_ENABLE_ZRAM:-0} == 1 ]]; then
  log "PINVOU3_CI_ENABLE_ZRAM=1: attempting zram setup (self-hosted runners only)"
  modprobe zram 2>/dev/null || warn "modprobe zram failed; skipping zram"
else
  log "PINVOU3_CI_ENABLE_ZRAM not set: skipping zram on hosted runners (module load froze hosted VMs; see fork of upstream script)"
fi

if ! swapon --show=NAME --noheadings 2>/dev/null | grep -q '/dev/zram'; then
  setup_zswap_fallback
fi

log "final swap layout:"
swapon --show || true
if [[ -d /sys/module/zswap/params ]]; then
  grep -H . /sys/module/zswap/params/enabled /sys/module/zswap/params/compressor \
    /sys/module/zswap/params/max_pool_percent || true
fi
exit 0
