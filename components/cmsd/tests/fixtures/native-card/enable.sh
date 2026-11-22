#!/bin/bash
# cmsd reads this literal native profile without executing shell.
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"
echo "请通过 Pinvou / cmsd 启用模型。Use Pinvou / cmsd. Pinvou / cmsd から起動してください。" >&2
exit 70
# Declarative profile, unreachable from script execution. No independent supervisor.
"$ENGINE_DIR/tachyon-serve" "$MODEL_DIR/$ARTIFACT" \
 --host "$HOST" --port "$PORT" --model-id "$MODEL_ID" \
 --max-context 262144 --kv-capacity 262144 --prefill-chunk 16384 \
 --max-concurrency "$MAX_CONCURRENCY" --kv-dtype fp8 \
 --spec mtp --draft-tokens 1 --lm-head-draft --vision --no-thinking --default-max-tokens 512 >> /dev/null
