import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
DEPLOY = ROOT / "account-platform/deploy"


class PersonalDeploymentSecurityTests(unittest.TestCase):
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
        self.assertEqual(len(targets), 9)
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

    def test_account_source_peers_are_explicit_and_pinned_in_production(self):
        compose = (DEPLOY / "production/compose.personal.yaml").read_text(encoding="utf-8")
        overlay = (DEPLOY / "production/compose.keycloak-personal-auth.yaml").read_text(encoding="utf-8")
        self.assertRegex(compose, r"ACCOUNT_TRUSTED_SOURCE_PEER_IPS:.*PERSONAL_EDGE_BACKEND_IP.*PERSONAL_PORTAL_BACKEND_IP.*PERSONAL_KEYCLOAK_BACKEND_IP")
        self.assertRegex(compose, r"token-portal:\s+<<: \*runtime\s+networks:\s+backend:\s+ipv4_address: \$\{PERSONAL_PORTAL_BACKEND_IP:\?")
        self.assertRegex(overlay, r"keycloak:\s+networks:\s+backend:\s+ipv4_address: \$\{PERSONAL_KEYCLOAK_BACKEND_IP:\?")
        command = (ROOT / "account-platform/cmd/account-service/main.go").read_text(encoding="utf-8")
        self.assertIn('account.NewTrustedSourceHandler(service.Handler(verify), os.Getenv("ACCOUNT_TRUSTED_SOURCE_PEER_IPS"))', command)


if __name__ == "__main__":
    unittest.main()
