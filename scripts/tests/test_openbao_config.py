import json
from pathlib import Path
import unittest

CONFIG = Path(__file__).resolve().parents[2] / "account-platform" / "deploy" / "openbao"


class OpenBaoConfigurationTests(unittest.TestCase):
    def test_nondev_raft_tls_and_declarative_audit(self):
        config = (CONFIG / "server.hcl").read_text(encoding="utf-8")
        for required in ('storage "raft"', 'tls_min_version = "tls12"', 'tls_key_file = "/bao/tls/server.key"',
                         'audit "file" "platform-audit"', 'log_raw = "false"', 'hmac_accessor = "true"',
                         'unsafe_allow_api_audit_creation = false', 'ui = false'):
            self.assertIn(required, config)
        for denied in ('tls_disable', 'initialize {', 'log_raw = "true"', 'storage "inmem"', 'file_path = "discard"'):
            self.assertNotIn(denied, config)

    def test_agent_is_sink_only_and_explicitly_private(self):
        config = (CONFIG / "agent.hcl").read_text(encoding="utf-8")
        for required in ('address = "https://openbao:8200"', 'ca_cert = "/bao/trust/ca.crt"',
                         'method "approle"', 'role_id_file_path = "/bao/bootstrap/role-id"',
                         'secret_id_file_path = "/bao/bootstrap/secret-id"', 'path = "/bao/token/token"', 'mode = 384'):
            self.assertIn(required, config)
        for denied in ('listener ', 'api_proxy ', 'tls_skip_verify', 'token ='):
            self.assertNotIn(denied, config)

    def test_runtime_policy_has_only_key_and_self_lease_capabilities(self):
        config = (CONFIG / "runtime-policy.hcl").read_text(encoding="utf-8")
        paths = [line.split('"')[1] for line in config.splitlines() if line.startswith('path "')]
        self.assertEqual(paths, ["transit/encrypt/pinvou-token-data", "transit/decrypt/pinvou-token-data",
                                 "auth/token/lookup-self", "auth/token/renew-self", "auth/token/revoke-self"])
        self.assertEqual(config.count('"associated_data" = [""]'), 2)
        self.assertEqual(config.count("required_parameters"), 2)
        self.assertNotIn('"create"', config)
        self.assertNotIn('"sudo"', config)
        self.assertNotIn('"*"', config)

    def test_role_has_bounded_tokens_and_no_shared_policy_or_secret(self):
        config = json.loads((CONFIG / "approle.json").read_text(encoding="utf-8"))
        self.assertTrue(config["bind_secret_id"])
        self.assertTrue(config["token_no_default_policy"])
        self.assertEqual(config["token_ttl"], 300)
        self.assertEqual(config["token_max_ttl"], 900)
        self.assertEqual(config["token_explicit_max_ttl"], 900)
        self.assertNotIn("token_policies", config)
        self.assertNotIn("secret_id", config)
        self.assertNotIn("role_id", config)

    def test_bootstrap_embedded_runtime_policy_matches_deployment(self):
        embedded = CONFIG.parents[1] / "tools/kms-recovery/internal/operator/plans/runtime-policy.hcl"
        self.assertEqual(embedded.read_text(encoding="utf-8"), (CONFIG / "runtime-policy.hcl").read_text(encoding="utf-8"))

    def test_standalone_facility_does_not_bootstrap_or_expose_ports(self):
        config = (CONFIG / "compose.yaml").read_text(encoding="utf-8")
        for required in ('read_only: true', 'user: "65532:65532"', 'cap_drop: ["ALL"]',
                         'soft: 0', 'hard: 0', 'external: true', 'create_host_path: false',
                         'BAO_CACERT: /bao/trust/ca.crt', 'OPENBAO_PRIVATE_IPV4:?required'):
            self.assertIn(required, config)
        for denied in ('ports:', 'BAO_TOKEN', 'root_token', 'unseal', 'tls_skip_verify', 'privileged:'):
            self.assertNotIn(denied, config)
        self.assertEqual(config.count('create_host_path: false'), 5)
        self.assertIn('/openbao/file:rw,nosuid,nodev,noexec,size=1m', config)
        self.assertIn('/openbao/logs:rw,nosuid,nodev,noexec,size=1m', config)

    def test_agent_compose_separates_bootstrap_and_memory_sinks(self):
        config = (CONFIG / "compose.agents.yaml").read_text(encoding="utf-8")
        for role in ("pinvou-token-gateway", "pinvou-credential-portal", "pinvou-kms-backup"):
            self.assertEqual(config.count("/bootstrap/" + role), 1)
            self.assertEqual(config.count("/run/pinvou-kms-agents/" + role), 1)
        for denied in ("pinvou-kms-operations", "ports:", "BAO_TOKEN", "privileged:", "cap_add:", "healthcheck:"):
            self.assertNotIn(denied, config)
        self.assertEqual(config.count('create_host_path: false'), 8)
        self.assertIn('cap_drop: ["ALL"]', config)
        self.assertIn('core: {soft: 0, hard: 0}', config)
        self.assertIn('external: true', config)

    def test_tmpfiles_recreates_directories_only_without_widening_access(self):
        lines = [line.split() for line in (CONFIG / "pinvou-kms-agents.tmpfiles.conf").read_text().splitlines()
                 if line and not line.startswith("#")]
        self.assertEqual(len(lines), 4)
        for fields in lines:
            self.assertEqual(fields[0], "d")
            self.assertEqual(fields[2], "0700")
            self.assertTrue(fields[1].startswith("/run/pinvou-kms-agents"))
        self.assertEqual(lines[0][3:5], ["root", "root"])
        self.assertTrue(all(fields[3:5] == ["65532", "65532"] for fields in lines[1:]))

    def test_snapshot_worker_only_reads_backup_sink_and_public_inputs(self):
        config = (CONFIG / "compose.backup.yaml").read_text(encoding="utf-8")
        self.assertEqual(config.count('create_host_path: false'), 6)
        self.assertEqual(config.count('read_only: true'), 6)  # Root FS plus five inputs.
        self.assertIn('source: /run/pinvou-kms-agents/pinvou-kms-backup', config)
        self.assertIn('target: /bao/output', config)
        self.assertIn('entrypoint: ["/bao/bin/kms-snapshot"]', config)
        self.assertIn('user: "65532:65532"', config)
        for denied in ('bootstrap', 'identity.age', 'BAO_TOKEN', 'ports:', 'cap_add:', 'privileged:', 'restart: unless-stopped'):
            self.assertNotIn(denied, config)

    def test_snapshot_timer_propagates_failure_and_never_prunes(self):
        service = (CONFIG / "pinvou-kms-snapshot.service").read_text(encoding="utf-8")
        timer = (CONFIG / "pinvou-kms-snapshot.timer").read_text(encoding="utf-8")
        self.assertIn('Type=oneshot', service)
        self.assertIn('TimeoutStartSec=120', service)
        self.assertIn('run --rm --no-deps --pull never snapshot', service)
        self.assertNotIn('ExecStart=-', service)
        self.assertNotIn('prune', service)
        for required in ('OnCalendar=hourly', 'RandomizedDelaySec=60', 'Persistent=true', 'Unit=pinvou-kms-snapshot.service'):
            self.assertIn(required, timer)


if __name__ == "__main__":
    unittest.main()
