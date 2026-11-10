#!/usr/bin/env bash
# Job-scoped native tools for the supported Ubuntu runner userspaces. Public
# downloads only; no database connection or production credentials are used.
set -euo pipefail
test "$(uname -s)" = Linux
test -n "${RUNNER_TEMP:?}" && test -n "${GITHUB_ENV:?}"
. /etc/os-release
test "$ID" = ubuntu
[[ "$VERSION_CODENAME" =~ ^[a-z]+$ ]]
case "$(uname -m)" in
  x86_64) architecture=x64; powershell_sha=1da6d6d6035d651f20e70ddee7c4c15ac6ab8b9fb40ee92240d91263f28a884b ;;
  aarch64) architecture=arm64; powershell_sha=02eb9991af447c7e7fe881acde195961afd397c40b8eb1c2b3856b84ec2d85f2 ;;
  *) echo 'Unsupported history-export tool architecture' >&2; exit 1 ;;
esac
# PowerShell release checksums: https://github.com/PowerShell/PowerShell/releases/tag/v7.6.9
tools_root="$(mktemp -d "$RUNNER_TEMP/pinvou-history-tools.XXXXXX")"
# These artifacts and apt keys are public. _apt must traverse the private job's
# tool directory; history archives use a separate, strictly private directory.
chmod 0755 "$tools_root"
sudo apt-get update
sudo apt-get install -y --no-install-recommends ca-certificates curl gnupg libicu-dev libssl-dev libkrb5-3 zlib1g
fetch() { curl --fail --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 180 --retry 2 "$1" --output "$2"; }
fetch "https://github.com/PowerShell/PowerShell/releases/download/v7.6.9/powershell-7.6.9-linux-$architecture.tar.gz" "$tools_root/powershell.tar.gz"
printf '%s  %s\n' "$powershell_sha" "$tools_root/powershell.tar.gz" | sha256sum --check --strict
mkdir "$tools_root/pwsh"
tar -xzf "$tools_root/powershell.tar.gz" -C "$tools_root/pwsh"
chmod 0700 "$tools_root/pwsh/pwsh"
"$tools_root/pwsh/pwsh" -NoProfile -Command 'if ($PSVersionTable.PSVersion -ne [version]"7.6.9") {exit 1}'

pg_tools=/usr/lib/postgresql/16/bin
if ! test -x "$pg_tools/psql" || ! test -x "$pg_tools/pg_dump" || ! test -x "$pg_tools/pg_restore"; then
  # Use the official signed PGDG source without persisting a new host apt source
  # or key. Retain existing administrator-configured Ubuntu sources for deps.
  # https://www.postgresql.org/download/linux/ubuntu/
  mkdir "$tools_root/sources" "$tools_root/lists" "$tools_root/lists/partial" "$tools_root/gnupg"
  chmod 0700 "$tools_root/gnupg"
  fetch https://www.postgresql.org/media/keys/ACCC4CF8.asc "$tools_root/pgdg.asc"
  fingerprint="$(gpg --homedir "$tools_root/gnupg" --batch --with-colons --show-keys "$tools_root/pgdg.asc" | awk -F: '$1=="fpr" && !seen++ {first=$10} END {print first}')"
  test "$fingerprint" = B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8
  if test -f /etc/apt/sources.list; then cp /etc/apt/sources.list "$tools_root/sources/host-main.list"; fi
  for source_file in /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
    if test -f "$source_file"; then cp "$source_file" "$tools_root/sources/host-$(basename "$source_file")"; fi
  done
  {
    printf 'Types: deb\nURIs: https://apt.postgresql.org/pub/repos/apt\nSuites: %s-pgdg\nComponents: main\nSigned-By:\n' "$VERSION_CODENAME"
    sed 's/^$/./;s/^/ /' "$tools_root/pgdg.asc"
  } > "$tools_root/sources/history-pgdg.sources"
  # The ordinary job user can traverse its own runner directory even when its
  # home is 0750. Keep apt downloads unprivileged without widening home ACLs.
  apt_options=(-o Dir::Etc::sourcelist=/dev/null -o "Dir::Etc::sourceparts=$tools_root/sources" -o "Dir::State::lists=$tools_root/lists" -o "APT::Sandbox::User=$(id -un)" -o Acquire::Retries=2 -o Acquire::https::Timeout=30 -o Acquire::http::Timeout=30)
  sudo apt-get "${apt_options[@]}" update
  sudo apt-get "${apt_options[@]}" install -y --no-install-recommends postgresql-client-16
fi
for client in psql pg_dump pg_restore; do
  test -x "$pg_tools/$client"
  "$pg_tools/$client" --version | grep -E "^$client \(PostgreSQL\) 16\."
done
printf 'PINVOU_HISTORY_EXPORT_TEST_PWSH=%s\nPINVOU_HISTORY_EXPORT_TEST_PGTOOLS=%s\n' "$tools_root/pwsh/pwsh" "$pg_tools" >> "$GITHUB_ENV"
echo 'History-export native tool preparation complete'
