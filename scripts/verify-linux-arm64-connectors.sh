#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CONNECTOR_ROOT="$REPO_ROOT/pinvou3-app/src-tauri/resources/platforms/linux/aarch64/bundle/connectors"
LOCK_FILE="$CONNECTOR_ROOT/connectors.lock.json"
BIN_DIR="$CONNECTOR_ROOT/linux-arm64/bin"

for command_name in jq sha256sum; do
  command -v "$command_name" >/dev/null || {
    echo "缺少校验命令: $command_name" >&2
    exit 1
  }
done

test -f "$LOCK_FILE" || {
  echo "连接器锁定清单不存在: $LOCK_FILE" >&2
  exit 1
}

schema_version="$(jq -er '.schemaVersion' "$LOCK_FILE")"
platform="$(jq -er '.platform' "$LOCK_FILE")"
artifact_count="$(jq -er '.artifacts | length' "$LOCK_FILE")"
unique_name_count="$(jq -er '[.artifacts[].name] | unique | length' "$LOCK_FILE")"

test "$schema_version" = "1" || {
  echo "不支持的连接器锁定清单版本: $schema_version" >&2
  exit 1
}
test "$platform" = "linux-arm64" || {
  echo "连接器锁定清单平台错误: $platform" >&2
  exit 1
}
test "$artifact_count" = "3" || {
  echo "连接器锁定清单必须包含 3 个产物，实际为: $artifact_count" >&2
  exit 1
}
test "$unique_name_count" = "$artifact_count" || {
  echo "连接器锁定清单包含重复名称" >&2
  exit 1
}

expected_names="$(jq -r '.artifacts[].name' "$LOCK_FILE" | LC_ALL=C sort)"
required_names=$'dws\nlark-cli\nwecom-cli'
test "$expected_names" = "$required_names" || {
  echo "连接器锁定清单名称不完整:" >&2
  printf '%s\n' "$expected_names" >&2
  exit 1
}

while IFS=$'\t' read -r name version license source url archive_sha binary_sha; do
  for value in "$name" "$version" "$license" "$source" "$url" "$archive_sha" "$binary_sha"; do
    test -n "$value" && test "$value" != "null" || {
      echo "连接器 $name 的锁定信息不完整" >&2
      exit 1
    }
  done
  [[ "$source" == https://* ]] && [[ "$url" == https://* ]] || {
    echo "连接器 $name 必须使用 HTTPS 来源" >&2
    exit 1
  }
  [[ "$archive_sha" =~ ^[0-9a-f]{64}$ ]] && [[ "$binary_sha" =~ ^[0-9a-f]{64}$ ]] || {
    echo "连接器 $name 的 SHA-256 格式错误" >&2
    exit 1
  }

  binary="$BIN_DIR/$name"
  test -f "$binary" || {
    echo "连接器二进制不存在: $binary" >&2
    exit 1
  }
  actual_sha="$(sha256sum "$binary" | awk '{print $1}')"
  test "$actual_sha" = "$binary_sha" || {
    echo "连接器 $name 哈希不匹配: expected=$binary_sha actual=$actual_sha" >&2
    exit 1
  }
  echo "连接器校验通过: $name $version ($license)"
done < <(
  jq -r '.artifacts[] | [
    .name,
    .version,
    .license,
    .source,
    .url,
    .archiveSha256,
    .binarySha256
  ] | @tsv' "$LOCK_FILE"
)

actual_names="$(find "$BIN_DIR" -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort)"
test "$actual_names" = "$required_names" || {
  echo "连接器目录存在未锁定或缺失的二进制:" >&2
  printf '%s\n' "$actual_names" >&2
  exit 1
}

echo "Linux ARM64 连接器锁定清单与内置二进制一致"
