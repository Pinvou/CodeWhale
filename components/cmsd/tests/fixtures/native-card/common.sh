#!/bin/bash
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
MODEL_ID="Ornith-1.5-35B-A3B-NVFP4"
ARTIFACT="ornith15-35b-a3b-nvfp4.tachyon"
TOTAL_BYTES="22892664832"
# This exact 262144-context profile is calibrated on GB10; not weights alone.
CMSD_RESERVED_GIB="48"
ENGINE_BIN_SHA256="0f8a4a7cec88304b7dbd8d8e46c2c70e938b77c31ae7185060ab770b52343c35"
export TACHYON_PROJ_W4A4=1 TACHYON_W4A4_SCHED=3
. "$CARD_DIR/.deploy-env"
