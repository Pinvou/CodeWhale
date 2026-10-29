#!/usr/bin/env bash
set -euo pipefail
: "${CI_RUNNER_DIR:?Set an independent bind-mounted runner directory}"
[[ "$CI_RUNNER_DIR" == /home/github-runner/ci-runners/* ]]
mkdir -p "$CI_RUNNER_DIR"
cd "$CI_RUNNER_DIR"
if [[ ! -f config.sh ]]; then
  cp -a /opt/actions-runner/. .
fi
if [[ ! -x toolchain/cargo/bin/cargo ]]; then
  mkdir -p toolchain
  cp -a /opt/rust/. toolchain/
fi
export CARGO_HOME="$CI_RUNNER_DIR/toolchain/cargo"
export RUSTUP_HOME="$CI_RUNNER_DIR/toolchain/rustup"
export PATH="$CARGO_HOME/bin:$PATH"
./env.sh
if [[ "${1:-}" == prepare ]]; then
  exit 0
fi
exec ./run.sh
