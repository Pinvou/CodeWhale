"""Exercise the real provisioning shell against a local, inert kcadm double.

Only the hardcoded executable location is replaced in a temporary script. No
Keycloak, network, system credentials, service, or database is contacted.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "provision-mobile-auth.sh"
BASH = os.environ.get("PINVOU_TEST_BASH") or shutil.which("bash")
FAKE = r'''#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$CONTROL_ARGS"
if [ "${CONTROL_FAIL_ROLE:-false}" = true ] && [ "$1" = add-roles ]; then exit 1; fi
case "$2" in
  realms/test-realm|events/config)
    resource=$(printf '%s' "$2" | tr '/' '-')
    if [ "$1" = update ]; then
      previous=''
      for argument in "$@"; do
        if [ "$previous" = -s ]; then
          field=${argument%%=*}
          case "$field" in *[!a-zA-Z0-9]*) exit 1 ;; esac
          printf '%s\n' "${argument#*=}" > "$CONTROL_STATE/$resource-$field"
        fi
        previous=$argument
      done
    elif [ "$1" = get ]; then
      field=''; previous=''
      for argument in "$@"; do
        if [ "$previous" = --fields ]; then field=$argument; fi
        previous=$argument
      done
      case "$field" in ''|*[!a-zA-Z0-9]*) exit 1 ;; esac
      if [ "$field" = "${CONTROL_UNAVAILABLE_READBACK_FIELD:-}" ]; then exit 1; fi
      value=$(cat "$CONTROL_STATE/$resource-$field")
      if [ "$field" = "${CONTROL_BAD_READBACK_FIELD:-}" ]; then value=false; fi
      if [ "$field" = enabledEventTypes ]; then
        if [ "${CONTROL_MISSING_EVENT:-false}" = true ]; then value='["LOGIN"]'; fi
        printf '{"enabledEventTypes":%s}\n' "$value"
      else
        printf '%s\n' "$value"
      fi
    fi ;;
esac
if [ "$1" = get ]; then
  case "$2" in
    clients)
      printf 'desktop-id,desktop-test\nrealm-management-id,realm-management\n'
      if [ "${CONTROL_EXISTING:-false}" = true ]; then
        printf 'provisioner-id,provisioner-test\nportal-id,portal-test\nhandoff-id,handoff-test\n'
      fi ;;
    */service-account-user) printf 'service-user-id\n' ;;
    authentication/flows) printf 'direct-flow-id,pinvou-mobile-direct-grant\nbrowser-flow-id,pinvou-browser\n' ;;
    */executions) printf 'direct-exec,pinvou-mobile-direct-grant\ncookie-exec,auth-cookie\nbrowser-exec,pinvou-browser-authenticator\n' ;;
    */protocol-mappers/models)
      if [ "${CONTROL_EXISTING:-false}" = true ]; then
        printf 'phone-id,pinvou-phone-number\nverified-id,pinvou-phone-number-verified\naccount-id,pinvou-account-audience\nhandoff-audience-id,pinvou-portal-handoff-audience\n'
      fi ;;
  esac
fi
case " $* " in
  *' -f - '*)
    body=$(cat)
    printf '%s\n' "$body" >> "$CONTROL_BODIES"
    if [ "$1" = create ] && [ "$2" = clients ]; then
      case "$body" in
        *'"clientId":"provisioner-test"'*) printf 'provisioner-id\n' ;;
        *'"clientId":"portal-test"'*) printf 'portal-id\n' ;;
        *'"clientId":"handoff-test"'*) printf 'handoff-id\n' ;;
        *) exit 1 ;;
      esac
    fi ;;
esac
'''


@unittest.skipUnless(BASH, "A local POSIX shell is required")
class ProvisioningContractTest(unittest.TestCase):
    def run_producer(self, changes=None, *, existing=False, role_failure=False):
        with tempfile.TemporaryDirectory(prefix="pinvou-keycloak-controls-") as directory:
            root = Path(directory)
            fake = root / "kcadm.sh"
            fake.write_text(FAKE, encoding="utf-8", newline="\n")
            fake.chmod(0o700)
            (root / "state").mkdir()
            source = SCRIPT.read_text(encoding="utf-8")
            marker = "KCADM_BINARY=/opt/keycloak/bin/kcadm.sh"
            self.assertEqual(1, source.count(marker))
            # Git Bash and native POSIX both accept forward slash paths here.
            clone = root / "provision.sh"
            clone.write_text(source.replace(marker, f'KCADM_BINARY="{fake.as_posix()}"'), encoding="utf-8", newline="\n")
            handoff = root / "handoff-secret"
            handoff.write_text("controlled-handoff-value-for-local-tests", encoding="utf-8")
            env = dict(os.environ)
            for key in list(env):
                if key.startswith(("KEYCLOAK_", "KC_BOOTSTRAP_", "PORTAL_", "PINVOU_OIDC_")):
                    del env[key]
            env.update({
                "KEYCLOAK_ADMIN_USERNAME": "controlled-test-admin",
                "KEYCLOAK_ADMIN_PASSWORD": "controlled-test-admin-value",
                "KEYCLOAK_PROVISIONER_CLIENT_SECRET": "controlled-provisioner-value-for-local-tests",
                "KEYCLOAK_REALM": "test-realm", "PINVOU_OIDC_CLIENT_ID": "desktop-test",
                "KEYCLOAK_PROVISIONER_CLIENT_ID": "provisioner-test",
                "PORTAL_OIDC_CLIENT_ID": "portal-test", "PORTAL_HANDOFF_OIDC_CLIENT_ID": "handoff-test",
                "PORTAL_PUBLIC_URL": "https://portal.example.test", "PORTAL_SESSION_HANDOFF": "true",
                "PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE": handoff.as_posix(),
                "CONTROL_ARGS": (root / "args").as_posix(), "CONTROL_BODIES": (root / "bodies").as_posix(),
                "CONTROL_EXISTING": str(existing).lower(), "CONTROL_FAIL_ROLE": str(role_failure).lower(),
                "CONTROL_STATE": (root / "state").as_posix(),
                "CONTROL_BAD_READBACK_FIELD": "", "CONTROL_UNAVAILABLE_READBACK_FIELD": "",
                "CONTROL_MISSING_EVENT": "false",
                "TMPDIR": root.as_posix(),
            })
            env.update(changes or {})
            result = subprocess.run([BASH, str(clone)], env=env, capture_output=True, timeout=60)
            args = (root / "args").read_text(encoding="utf-8") if (root / "args").exists() else ""
            body_text = (root / "bodies").read_text(encoding="utf-8") if (root / "bodies").exists() else ""
            # Pretty profile JSON occupies several lines; use a complete JSON decoder.
            bodies, text = [], body_text.strip()
            decoder = json.JSONDecoder()
            while text:
                body, end = decoder.raw_decode(text)
                bodies.append(body)
                text = text[end:].lstrip()
            self.assertFalse(list(root.glob("pinvou-kcadm.*")), "kcadm private session file must be removed")
            return result, args, bodies

    def assert_hardening(self, args):
        settings = {
            "realms/test-realm": {
                "bruteForceProtected": "true", "permanentLockout": "false", "failureFactor": "10",
                "maxFailureWaitSeconds": "900", "minimumQuickLoginWaitSeconds": "60",
                "waitIncrementSeconds": "60", "quickLoginCheckMilliSeconds": "1000",
                "maxDeltaTimeSeconds": "43200", "accessTokenLifespan": "600",
                "revokeRefreshToken": "true", "refreshTokenMaxReuse": "0",
            },
            "events/config": {
                "eventsEnabled": "true", "eventsExpiration": "2592000",
                "adminEventsEnabled": "true", "adminEventsDetailsEnabled": "false",
            },
        }
        lines = args.splitlines()
        for resource, fields in settings.items():
            for field, value in fields.items():
                with self.subTest(resource=resource, field=field):
                    self.assertTrue(any(line.startswith(f"update {resource} ") and f" -s {field}={value}" in line for line in lines))
                    self.assertEqual(1, sum(line.startswith(f"get {resource} ") and f" --fields {field} --format csv --noquotes" in line for line in lines))
        events = ["LOGIN", "LOGIN_ERROR", "LOGOUT", "REGISTER", "REGISTER_ERROR", "UPDATE_PASSWORD",
                  "UPDATE_PASSWORD_ERROR", "REFRESH_TOKEN", "REFRESH_TOKEN_ERROR"]
        event_update = next(line for line in lines if line.startswith("update events/config "))
        self.assertIn(" -s enabledEventTypes=" + json.dumps(events, separators=(",", ":")), event_update)
        readback = next(line for line in lines if line.startswith("get events/config ") and " --fields enabledEventTypes " in line)
        self.assertLess(lines.index(readback), next(index for index, line in enumerate(lines) if line.startswith("update users/profile ")))

    def assert_clients(self, result, args, bodies):
        self.assertEqual(0, result.returncode, result.stderr.decode())
        self.assert_hardening(args)
        clients = {body["clientId"]: body for body in bodies if "clientId" in body}
        portal, handoff = clients["portal-test"], clients["handoff-test"]
        self.assertTrue(portal["publicClient"])
        self.assertTrue(portal["standardFlowEnabled"])
        self.assertEqual(["https://portal.example.test/auth/callback"], portal["redirectUris"])
        self.assertEqual("S256", portal["attributes"]["pkce.code.challenge.method"])
        self.assertEqual([], portal["optionalClientScopes"])
        for name in ("directAccessGrantsEnabled", "implicitFlowEnabled", "serviceAccountsEnabled"):
            self.assertFalse(portal[name]); self.assertFalse(handoff[name])
        self.assertFalse(handoff["publicClient"]); self.assertFalse(handoff["standardFlowEnabled"])
        self.assertEqual("true", handoff["attributes"]["standard.token.exchange.enabled"])
        self.assertEqual("false", handoff["attributes"]["standard.token.exchange.enableRefreshRequestedTokenType"])
        self.assertEqual([], handoff["optionalClientScopes"])
        self.assertIn('optionalClientScopes=["offline_access"]', args)
        self.assertIn('included.client.audience\"=handoff-test', args)
        self.assertGreaterEqual(args.count('included.client.audience\"=desktop-test'), 2)
        self.assertNotIn(handoff["secret"], args)
        self.assertNotIn(clients["provisioner-test"]["secret"], args)
        public_output = result.stdout + result.stderr
        self.assertNotIn(handoff["secret"].encode(), public_output)
        self.assertNotIn(clients["provisioner-test"]["secret"].encode(), public_output)
        profile = next(body for body in bodies if "attributes" in body and isinstance(body["attributes"], list))
        attributes = {item["name"]: item for item in profile["attributes"]}
        for name in ("username", "phone_number", "phone_number_verified", "pinvou_registration_operation_id", "pinvou_bootstrap_managed", "pinvou_identity_security_fence_version"):
            self.assertEqual(["admin"], attributes[name]["permissions"]["edit"])

    def test_new_clients_have_separate_finite_grants_and_audiences(self):
        self.assert_clients(*self.run_producer())

    def test_existing_clients_and_mappers_are_updated_not_duplicated(self):
        result, args, bodies = self.run_producer(existing=True)
        self.assert_clients(result, args, bodies)
        self.assertNotIn("create clients ", args)
        self.assertIn("update clients/portal-id ", args)
        self.assertIn("protocol-mappers/models/phone-id", args)

    def test_handoff_disabled_does_not_provision_a_secret_client(self):
        result, args, bodies = self.run_producer({"PORTAL_SESSION_HANDOFF": "false"})
        self.assertEqual(0, result.returncode)
        self.assert_hardening(args)
        self.assertFalse(any(body.get("clientId") == "handoff-test" for body in bodies))
        self.assertNotIn("included.client.audience=handoff-test", args)

    def test_invalid_configuration_never_reaches_kcadm(self):
        cases = [
            {"PORTAL_PUBLIC_URL": value} for value in (
                "file:///portal", "https://", "https://user@host", "https://host?", "https://host#fragment",
                "https://host\\path", 'https://host/"quoted', "https://host/space path",
            )
        ] + [
            {"PORTAL_SESSION_HANDOFF": "yes"}, {"PORTAL_OIDC_CLIENT_ID": "desktop-test"},
            {"KEYCLOAK_PROVISIONER_CLIENT_ID": "portal-test"}, {"KEYCLOAK_REALM": "realm/escape"},
            {"KEYCLOAK_PROVISIONER_CLIENT_SECRET": "short"},
            {"KEYCLOAK_PROVISIONER_CLIENT_SECRET": "long-controlled-value-with-newline\ninvalid"},
            {"PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE": ""},
        ]
        for configuration in cases:
            with self.subTest(configuration=list(configuration)):
                result, args, bodies = self.run_producer(configuration)
                self.assertNotEqual(0, result.returncode)
                self.assertEqual("", args); self.assertEqual([], bodies)

    def test_role_failure_stops_before_public_clients(self):
        result, args, bodies = self.run_producer(role_failure=True)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("add-roles ", args)
        self.assertFalse(any(body.get("clientId") == "portal-test" for body in bodies))

    def test_wrong_hardening_readback_stops_before_profile_and_clients(self):
        for field in ("bruteForceProtected", "refreshTokenMaxReuse", "eventsEnabled"):
            with self.subTest(field=field):
                result, args, bodies = self.run_producer({"CONTROL_BAD_READBACK_FIELD": field})
                self.assertNotEqual(0, result.returncode)
                self.assertIn(f"Keycloak hardening readback failed for {field}".encode(), result.stderr)
                self.assertNotIn("update users/profile ", args)
                # The existing producer creates its admin-only provisioner
                # first; neither public client nor profile may follow failure.
                self.assertEqual(["provisioner-test"], [body.get("clientId") for body in bodies])

    def test_unavailable_hardening_readback_stops_before_profile_and_clients(self):
        for field in ("bruteForceProtected", "eventsExpiration"):
            with self.subTest(field=field):
                result, args, bodies = self.run_producer({"CONTROL_UNAVAILABLE_READBACK_FIELD": field})
                self.assertNotEqual(0, result.returncode)
                self.assertIn(f"Keycloak hardening readback unavailable for {field}".encode(), result.stderr)
                self.assertNotIn("update users/profile ", args)
                self.assertEqual(["provisioner-test"], [body.get("clientId") for body in bodies])

    def test_missing_enabled_events_stops_before_profile_and_clients(self):
        result, args, bodies = self.run_producer({"CONTROL_MISSING_EVENT": "true"})
        self.assertNotEqual(0, result.returncode)
        self.assertIn(b"Keycloak required audit event missing: LOGIN_ERROR", result.stderr)
        self.assertNotIn("update users/profile ", args)
        self.assertEqual(["provisioner-test"], [body.get("clientId") for body in bodies])


class ThemeContractTest(unittest.TestCase):
    def test_all_three_languages_cover_the_same_theme_messages(self):
        theme = SCRIPT.parent / "themes/pinvou/login"
        keys = []
        for locale in ("en", "ja", "zh_CN"):
            messages = {}
            for line in (theme / f"messages/messages_{locale}.properties").read_text(encoding="utf-8").splitlines():
                if not line.strip() or line.lstrip().startswith("#"):
                    continue
                key, value = line.split("=", 1)
                self.assertNotIn(key, messages)
                self.assertTrue(value.strip(), key)
                messages[key] = value
            keys.append(set(messages))
        self.assertEqual(keys[0], keys[1]); self.assertEqual(keys[0], keys[2])
        import re
        used = set(re.findall(r'msg\("([^"]+)"', (theme / "pinvou-login.ftl").read_text(encoding="utf-8")))
        self.assertFalse({key for key in used if key.startswith("pinvou")} - keys[0])

    def test_theme_references_actual_content_hashed_local_assets(self):
        theme = SCRIPT.parent / "themes/pinvou/login"
        properties = dict(line.split("=", 1) for line in (theme / "theme.properties").read_text(encoding="utf-8").splitlines() if "=" in line)
        for key in ("styles", "scripts"):
            for asset in properties[key].split():
                path = theme / "resources" / asset
                self.assertTrue(path.is_file())
                self.assertEqual(path.name.split(".")[-2], hashlib.sha256(path.read_bytes()).hexdigest())
        self.assertEqual("zh-CN,en,ja", properties["locales"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
