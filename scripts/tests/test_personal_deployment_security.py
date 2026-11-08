import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
DEPLOY = ROOT / "account-platform/deploy"


def nginx_active_text(text):
    # A hash starts a comment only outside quoted or escaped text.
    return re.sub(
        r"""(?P<quoted>"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')|(?P<escaped>\\.)|(?P<comment>#[^\r\n]*)""",
        lambda match: "" if match.lastgroup == "comment" else match.group(0),
        text,
    )


class PersonalDeploymentSecurityTests(unittest.TestCase):
    def test_personal_redis_acl_covers_existing_session_cleanup(self):
        acl = (DEPLOY / "production/redis-personal.acl.template").read_text(encoding="utf-8")
        portal = next(line.split() for line in acl.splitlines() if line.startswith("user portal "))
        store = (ROOT / "account-platform/internal/portal/store.go").read_text(encoding="utf-8")
        wire = (ROOT / "account-platform/internal/portal/approval_wire_store.go").read_text(encoding="utf-8")
        prefix = re.search(r'func approvalWireKey\(id string\) string \{ return "([^"]+)" \+ id \}', wire).group(1)
        # These operations clean up wire state even when personal approvals are
        # disabled, or when no corresponding Redis key has ever been created.
        for name in ["DeleteSession", "LogoutSession", "ReplaceSessionFromHandoff"]:
            with self.subTest(operation=name):
                body = re.search(rf"func \(s \*RedisStore\) {name}\([^\n]+\{{(.*?)(?=\nfunc |\Z)", store, re.S).group(1)
                self.assertTrue("approvalWireKey(" in body or f"'{prefix}'" in body)
                self.assertIn(f"~{prefix}*", portal)
        self.assertNotIn("~portal:*", portal)
        self.assertNotIn("~*", portal)

    def test_keycloak_applies_and_verifies_hardening(self):
        text = (DEPLOY / "keycloak/provision-mobile-auth.sh").read_text(encoding="utf-8")
        block = text.split("# BEGIN PINVOU_REALM_HARDENING", 1)[1].split("# END PINVOU_REALM_HARDENING", 1)[0]
        bash = shutil.which("bash")
        if not bash and os.name == "nt":
            candidate = Path("C:/Program Files/Git/bin/bash.exe")
            if candidate.is_file():
                bash = str(candidate)
        self.assertIsNotNone(bash, "Bash is required to execute the provisioning contract")
        # Execute the real security block against a CLI that persists its
        # updates and can simulate an ignored setting or unavailable read.
        fixture = r'''
set -eu
declare -A stored
REALM=fixture
KCADM=kcadm
kcadm() {
  action=$1; resource=$2; shift 2
  if [[ "$action" == update ]]; then
    while [[ $# -gt 0 ]]; do
      if [[ "$1" == -s ]]; then
        setting=$2; shift 2
        stored["$resource:${setting%%=*}"]="${setting#*=}"
      else shift; fi
    done
  else
    field=''
    while [[ $# -gt 0 ]]; do
      if [[ "$1" == --fields ]]; then field=$2; shift 2; else shift; fi
    done
    if [[ "$mode" == unavailable ]]; then return 9; fi
    if [[ "$mode" == ignored && "$field" == revokeRefreshToken ]]; then echo false; return; fi
    if [[ "$mode" == missing_event && "$field" == enabledEventTypes ]]; then echo '["LOGIN_ERROR"]'; return; fi
    printf '%s\n%s\n' "$field" "${stored[$resource:$field]:-MISSING}"
  fi
}
'''
        # One Bash startup; each case has isolated settings and executes the
        # exact script block. No Keycloak instance or credential is involved.
        script = fixture
        for mode, expected in [("ok", 0), ("ignored", 1), ("unavailable", 1), ("missing_event", 1)]:
            script += f'\nset +e\n( set -e; mode={mode};\n{block}\n)\nstatus=$?\nset -e\n[[ "$status" == {expected} ]] || exit 8\n'
        # File-backed output cannot leave communicate() waiting on an orphaned
        # MSYS child's inherited pipe after a Windows timeout.
        with tempfile.TemporaryDirectory(prefix="pinvou-kcadm-contract-") as directory:
            fixture_path = Path(directory) / "verify.sh"
            fixture_path.write_text(script, encoding="utf-8", newline="\n")
            with tempfile.TemporaryFile(mode="w+t", encoding="utf-8") as log:
                result = subprocess.run([bash, fixture_path.as_posix()], stdout=log, stderr=log, timeout=180)
                log.seek(0)
                self.assertEqual(result.returncode, 0, log.read())
        for field in ["bruteForceProtected", "revokeRefreshToken", "refreshTokenMaxReuse", "accessTokenLifespan", "eventsEnabled", "adminEventsEnabled"]:
            self.assertIn(field, block)
        self.assertIn("adminEventsDetailsEnabled=false", block)

    def test_nginx_re_resolves_without_replacing_request_uri_or_tls_name(self):
        text = (DEPLOY / "nginx/personal.conf.template").read_text(encoding="utf-8")
        self.assertIn("resolver 127.0.0.11 valid=10s ipv6=off;", text)
        targets = re.findall(r"proxy_pass\s+([^;]+);", text)
        self.assertEqual(len(targets), 11)
        self.assertEqual(targets.count("http://$billing_origin"), 2)
        self.assertIn("set $billing_origin billing-service:8080;", text)
        self.assertTrue(all(re.fullmatch(r"https?://\$[a-z_]+", target) for target in targets))
        # A URI suffix on variable proxy_pass replaces the rewritten URI.
        for prefix in ["account", "identity", "token"]:
            self.assertIn(f"rewrite ^/{prefix}/(.*)$ /$1 break;", text)
        self.assertIn("proxy_ssl_verify on;", text)
        self.assertIn("proxy_ssl_server_name on;", text)
        self.assertIn("proxy_ssl_name $keycloak_tls_name;", text)
        self.assertIn("map $keycloak_origin $keycloak_tls_name", text)
        self.assertIn("location = /realms/pinvou/protocol/openid-connect/token", text)

    def test_nginx_active_text_keeps_quoted_and_escaped_hashes(self):
        for config, active in [
            ('set $label "# retained"; # retired\n', 'set $label "# retained"; \n'),
            ("set $label '# retained'; # retired\n", "set $label '# retained'; \n"),
            (r"set $label \#retained; # retired", r"set $label \#retained; "),
            ("# proxy_read_timeout 300s;\n", "\n"),
        ]:
            with self.subTest(config=config):
                self.assertEqual(nginx_active_text(config), active)

    def test_token_location_preserves_stream_read_interval_without_widening_other_routes(self):
        text = (DEPLOY / "nginx/personal.conf.template").read_text(encoding="utf-8")
        # Use the actual rendered personal layout, including exact/private
        # location precedence; do not pass because an unrelated block says 300s.
        text = text.replace("__PUBLIC_HOST__", "personal.example.test").replace(
            "__KEYCLOAK_ORIGIN__", "identity.example.test:8443"
        ).replace("__PUBLIC_TLS_PORT__", "443")
        self.assertNotRegex(text, r"__[A-Z_]+__")
        legacy = (DEPLOY / "nginx/account-platform.conf.template").read_text(encoding="utf-8")
        legacy = nginx_active_text(legacy)
        legacy_token = re.search(r"location /token/\s*\{([^}]+)\}", legacy).group(1)
        self.assertEqual(re.findall(r"proxy_read_timeout\s+([^;]+);", legacy_token), ["300s"])
        text = nginx_active_text(text)
        locations = []
        for match in re.finditer(r'\blocation\s+(?:(=|\^~|~\*?)\s+)?(?:"([^"]+)"|([^\s{]+))\s*\{', text):
            depth = 1
            end = match.end()
            while depth:
                depth += (text[end] == "{") - (text[end] == "}")
                end += 1
            locations.append((match.group(1), match.group(2) or match.group(3), text[match.end():end - 1]))
        server_defaults = text.split("location", 1)[0]
        self.assertEqual(re.findall(r"proxy_read_timeout\s+([^;]+);", server_defaults), ["30s"])

        def selected_location(uri):
            exact = [item for item in locations if item[0] == "=" and item[1] == uri]
            if exact:
                return exact[0]
            prefixes = [item for item in locations if item[0] in (None, "^~") and uri.startswith(item[1])]
            prefix = max(prefixes, key=lambda item: len(item[1]))
            if prefix[0] == "^~":
                return prefix
            for item in locations:
                if item[0] in ("~", "~*") and re.search(item[1], uri, re.I if item[0] == "~*" else 0):
                    return item
            return prefix

        for uri in ["/token/v1/chat/completions", "/token/v1/models"]:
            with self.subTest(uri=uri):
                _, route, body = selected_location(uri)
                self.assertEqual(route, "/token/")
                self.assertEqual(re.findall(r"proxy_read_timeout\s+([^;]+);", body), ["300s"])
                self.assertIn("proxy_pass http://$token_origin;", body)
                self.assertIn("rewrite ^/token/(.*)$ /$1 break;", body)
        self.assertEqual(text.count("proxy_read_timeout 300s;"), 1)
        for uri in ["/token/admin", "/token/admin/requests", "/token/internal", "/token/internal/settlements"]:
            with self.subTest(private_uri=uri):
                body = selected_location(uri)[2]
                self.assertIn("return 404;", body)
                self.assertNotIn("proxy_pass", body)
        for uri in ["/account/v1/me", "/api/v1/payment-callbacks", "/session"]:
            with self.subTest(ordinary_uri=uri):
                self.assertNotRegex(selected_location(uri)[2], r"proxy_read_timeout\s+300s;")
        self.assertEqual(re.findall(r"proxy_connect_timeout\s+([^;]+);", text), ["3s"])
        self.assertEqual(re.findall(r"proxy_send_timeout\s+([^;]+);", text), ["30s"])

    def test_large_import_has_one_exact_authenticated_portal_path(self):
        text = (DEPLOY / "nginx/personal.conf.template").read_text(encoding="utf-8")
        block = re.search(r"location = /api/admin/billing/admin/v1/reconciliation-imports\s*\{([^}]+\}[^}]+)\}", text).group(1)
        self.assertIn("limit_except POST { deny all; }", block)
        self.assertIn("client_max_body_size 64m;", block)
        self.assertIn("proxy_pass http://$portal_origin;", block)
        self.assertEqual(text.count("client_max_body_size 64m;"), 1)
        self.assertIn("client_max_body_size 2m;", text)

    def test_original_upload_alone_gets_larger_unbuffered_portal_budget(self):
        text = (DEPLOY / "nginx/personal.conf.template").read_text(encoding="utf-8")
        route, block = re.search(r'location ~ "([^"]+/admission-materials\$)"\s*\{([^}]+\}[^}]+)\}', text).groups()
        root = "/api/admin/billing/admin/v1/recharge-products/"
        catalog = "a3456789-0123-4567-8901-234567890abc"
        upload = root + catalog + "/admission-materials"
        self.assertIsNotNone(re.fullmatch(route, upload))
        for other in [upload + "/", upload + "/" + catalog + "/content", root + catalog + "/publish",
                      root + "00000000-0000-0000-0000-000000000000/admission-materials",
                      root + catalog.upper() + "/admission-materials", root + "bad/admission-materials"]:
            self.assertIsNone(re.fullmatch(route, other))
        for directive in ["limit_except POST { deny all; }", "client_max_body_size 10m;",
                          "proxy_request_buffering off;", "proxy_read_timeout 120s;", "proxy_pass http://$portal_origin;"]:
            self.assertIn(directive, block)
        for directive in ["client_max_body_size 10m;", "proxy_request_buffering off;", "proxy_read_timeout 120s;"]:
            self.assertEqual(text.count(directive), 1)
        self.assertIn("client_max_body_size 2m;", text)

    def test_account_source_peers_are_explicit_and_pinned_in_production(self):
        compose = (DEPLOY / "production/compose.personal.yaml").read_text(encoding="utf-8")
        overlay = (DEPLOY / "production/compose.keycloak-personal-auth.yaml").read_text(encoding="utf-8")
        self.assertRegex(compose, r"ACCOUNT_TRUSTED_SOURCE_PEER_IPS:.*PERSONAL_EDGE_BACKEND_IP.*PERSONAL_PORTAL_BACKEND_IP.*PERSONAL_KEYCLOAK_BACKEND_IP")
        self.assertRegex(compose, r"token-portal:\s+<<: \*runtime\s+networks:\s+backend:\s+ipv4_address: \$\{PERSONAL_PORTAL_BACKEND_IP:\?")
        self.assertRegex(overlay, r"keycloak:\s+networks:\s+backend:\s+ipv4_address: \$\{PERSONAL_KEYCLOAK_BACKEND_IP:\?")
        command = (ROOT / "account-platform/cmd/account-service/main.go").read_text(encoding="utf-8")
        self.assertIn('account.NewTrustedSourceHandler(service.Handler(verify), os.Getenv("ACCOUNT_TRUSTED_SOURCE_PEER_IPS"))', command)


class PlatformAdministratorSecretTransportTests(unittest.TestCase):
    """Run the real shell with only its fixed executable replaced by a double."""

    FAKE = r'''#!/bin/sh
set -eu
for argument in "$@"; do printf '%s\0' "$argument" >> "$CONTROL_ARGS"; done
config=''; previous=''
for argument in "$@"; do
  if [ "$previous" = --config ]; then config=$argument; fi
  previous=$argument
done
[ -f "$config" ] || exit 91
mode=$(stat -c %a "$config" 2>/dev/null || stat -f %Lp "$config" 2>/dev/null)
[ "$mode" = 600 ] || exit 91
printf '%s\0' "$config" >> "$CONTROL_CONFIGS"
# Only the selected KC_CLI_PASSWORD may reach a CLI child, never both originals.
[ -z "${KEYCLOAK_ADMIN_PASSWORD+x}${KC_BOOTSTRAP_ADMIN_PASSWORD+x}${PINVOU_ADMIN_PASSWORD+x}" ] || exit 92
printf '%s %s\n' "$1" "$2" >> "$CONTROL_CALLS"
case "$1 $2" in
  'config credentials')
    [ "$KC_CLI_PASSWORD" = "$CONTROL_EXPECT_AUTH" ] || exit 93
    if [ "${CONTROL_FAIL:-}" = auth ]; then printf '%s' "$KC_CLI_PASSWORD" >&2; exit 23; fi
    printf 'inert-token-config\n' > "$config" ;;
  'set-password -r')
    [ "$KC_CLI_PASSWORD" = "$CONTROL_EXPECT_TARGET" ] || exit 94
    if [ "${CONTROL_FAIL:-}" = password ]; then printf '%s' "$KC_CLI_PASSWORD" >&2; exit 24; fi
    printf 'password-set\n' >> "$CONTROL_STATE/effects" ;;
  *)
    [ -z "${KC_CLI_PASSWORD+x}" ] || exit 95
    case "$1 $2" in
      'get realms/pinvou/roles/platform-admin') [ -f "$CONTROL_STATE/role" ] ;;
      'create realms/pinvou/roles') touch "$CONTROL_STATE/role" ;;
      'get users')
        if [ -f "$CONTROL_STATE/user" ]; then printf 'existing-id,platform-admin-user\n'; fi ;;
      'create users') touch "$CONTROL_STATE/user"; printf 'created-id\n' ;;
      'add-roles -r')
        if [ "${CONTROL_FAIL:-}" = roles ]; then exit 25; fi
        printf 'role-assigned\n' >> "$CONTROL_STATE/effects" ;;
      *) exit 96 ;;
    esac ;;
esac
'''

    def run_provision(self, *, existing=False, failure="", bootstrap=False, traced=False, repeat=False, ambient=False):
        bash = shutil.which("bash")
        if not bash and os.name == "nt":
            bash = str(Path("C:/Program Files/Git/bin/bash.exe"))
        self.assertTrue(bash and Path(bash).is_file(), "Bash is required for the inert CLI contract")
        # Synthetic values deliberately contain shell metacharacters, Unicode
        # and newlines. They are never real credentials or written to evidence.
        auth = "synthetic-auth-only-'\"\\$();\n\t管理员"
        target = "synthetic-target-only-'\"\\$();\n\t管理者"
        with tempfile.TemporaryDirectory(prefix="pinvou-platform-admin-contract-") as directory:
            root = Path(directory)
            state = root / "state"
            state.mkdir()
            if existing:
                (state / "role").touch()
                (state / "user").touch()
            cli = root / "kcadm.sh"
            cli.write_text(self.FAKE, encoding="utf-8", newline="\n")
            cli.chmod(0o700)
            text = (DEPLOY / "keycloak/provision-platform-admin.sh").read_text(encoding="utf-8")
            marker = "KCADM_BINARY=/opt/keycloak/bin/kcadm.sh"
            self.assertEqual(text.count(marker), 1)
            # Same seam as the existing mobile-auth shell contract: no product
            # executable override, real network client or credential is used.
            script = root / "provision.sh"
            script.write_text(text.replace(marker, f'KCADM_BINARY="{cli.as_posix()}"'), encoding="utf-8", newline="\n")
            env = dict(os.environ)
            for name in ["KEYCLOAK_ADMIN_USERNAME", "KEYCLOAK_ADMIN_PASSWORD", "KC_BOOTSTRAP_ADMIN_USERNAME", "KC_BOOTSTRAP_ADMIN_PASSWORD", "KC_CLI_PASSWORD"]:
                env.pop(name, None)
            prefix = "KC_BOOTSTRAP_ADMIN" if bootstrap else "KEYCLOAK_ADMIN"
            env.update({prefix + "_USERNAME": "bootstrap-user", prefix + "_PASSWORD": auth,
                        "PINVOU_ADMIN_USERNAME": "platform-admin-user", "PINVOU_ADMIN_PASSWORD": target,
                        "TMPDIR": root.as_posix(), "CONTROL_ARGS": (root / "args").as_posix(),
                        "CONTROL_CONFIGS": (root / "configs").as_posix(), "CONTROL_CALLS": (root / "calls").as_posix(),
                        "CONTROL_STATE": state.as_posix(), "CONTROL_EXPECT_AUTH": auth,
                        "CONTROL_EXPECT_TARGET": target, "CONTROL_FAIL": failure})
            if ambient:
                env["KC_CLI_PASSWORD"] = "synthetic-stale-ambient-password"
            results = []
            for _ in range(2 if repeat else 1):
                # File-backed capture avoids an orphaned MSYS child retaining
                # a pipe on Windows. The deadline is a test bound, not a retry.
                with tempfile.TemporaryFile() as stdout, tempfile.TemporaryFile() as stderr:
                    result = subprocess.run([bash, "--posix", *(["-x"] if traced else []), script.as_posix()],
                                            env=env, stdout=stdout, stderr=stderr, timeout=30)
                    stdout.seek(0); stderr.seek(0)
                    output = stdout.read() + stderr.read()
                self.assertNotIn(auth.encode(), output)
                self.assertNotIn(target.encode(), output)
                self.assertNotIn(b"synthetic-auth-only", output)
                self.assertNotIn(b"synthetic-target-only", output)
                self.assertNotIn(b"synthetic-stale-ambient-password", output)
                results.append((result.returncode, output))
            args = (root / "args").read_bytes().split(b"\0")
            self.assertNotIn(b"--password", args)
            self.assertNotIn(b"--new-password", args)
            self.assertNotIn(b"-p", args)
            self.assertFalse(any(b"synthetic-auth-only" in a or b"synthetic-target-only" in a for a in args))
            self.assertFalse(any(b"synthetic-stale-ambient-password" in a for a in args))
            configs = (root / "configs").read_bytes().decode().split("\0")
            self.assertTrue(configs[0])
            self.assertTrue(all(not Path(p).exists() for p in configs if p))
            self.assertEqual(list(root.glob("pinvou-admin-kcadm.*")), [])
            calls = (root / "calls").read_text(encoding="utf-8").splitlines()
            effects = (state / "effects").read_text(encoding="utf-8").splitlines() if (state / "effects").exists() else []
            return results, calls, effects, args

    def test_create_transports_both_secrets_without_argv(self):
        results, calls, effects, args = self.run_provision()
        self.assertEqual(results[0][0], 0, results[0][1])
        self.assertIn("create realms/pinvou/roles", calls)
        self.assertIn("create users", calls)
        self.assertEqual(effects, ["password-set", "role-assigned"])
        self.assertIn(b"created-id", args)
        self.assertIn(b"platform-admin", args)

    def test_existing_user_and_role_are_reused(self):
        results, calls, effects, args = self.run_provision(existing=True, repeat=True)
        self.assertTrue(all(code == 0 for code, _ in results))
        self.assertNotIn("create realms/pinvou/roles", calls)
        self.assertNotIn("create users", calls)
        self.assertEqual(effects, ["password-set", "role-assigned"] * 2)
        self.assertIn(b"existing-id", args)

    def test_bootstrap_environment_alias_and_disabled_xtrace(self):
        results, _, effects, _ = self.run_provision(bootstrap=True, traced=True)
        self.assertEqual(results[0][0], 0, results[0][1])
        self.assertEqual(effects, ["password-set", "role-assigned"])

    def test_ambient_cli_password_does_not_reach_other_calls(self):
        results, _, effects, _ = self.run_provision(ambient=True)
        self.assertEqual(results[0][0], 0, results[0][1])
        self.assertEqual(effects, ["password-set", "role-assigned"])

    def test_sensitive_failures_are_redacted_and_cleaned(self):
        for failure, expected in [("auth", b"Keycloak administrator authentication failed"),
                                  ("password", b"Platform administrator password update failed")]:
            with self.subTest(failure=failure):
                results, calls, effects, _ = self.run_provision(failure=failure)
                self.assertNotEqual(results[0][0], 0)
                self.assertIn(expected, results[0][1])
                self.assertEqual(effects, [])
                self.assertNotIn("add-roles -r", calls)

    def test_role_failure_cleans_config_and_does_not_report_success(self):
        results, _, effects, _ = self.run_provision(failure="roles")
        self.assertNotEqual(results[0][0], 0)
        self.assertEqual(effects, ["password-set"])
        self.assertNotIn(b"identity provisioned", results[0][1])


if __name__ == "__main__":
    unittest.main()
