"""Build contract only; production startup requires the separate TLS/PG gate."""

from pathlib import Path
import re
import unittest
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[2] / "account-platform/deploy/keycloak"
VERSION = "26.7.0"
DIGEST = "0f198be292568439d700cdbfb893e69a6009bb43a94a06a945b1d3d506c76b13"


class KeycloakImageTests(unittest.TestCase):
    def test_runtime_builder_and_spi_remain_on_approved_version(self):
        dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")
        images = re.findall(r"^FROM (quay\.io/keycloak/keycloak:\S+)", dockerfile, re.M)
        self.assertEqual(images, [f"quay.io/keycloak/keycloak:{VERSION}@sha256:{DIGEST}"] * 2)
        pom = ET.parse(ROOT / "mobile-authenticator/pom.xml")
        ns = {"m": "http://maven.apache.org/POM/4.0.0"}
        self.assertEqual(pom.findtext("m:properties/m:keycloak.version", namespaces=ns), VERSION)
        self.assertEqual(pom.findtext("m:properties/m:maven.compiler.release", namespaces=ns), "21")
        for dep in pom.findall("m:dependencies/m:dependency", ns):
            self.assertEqual(dep.findtext("m:version", namespaces=ns), "${keycloak.version}")
            self.assertEqual(dep.findtext("m:scope", namespaces=ns), "provided")

    def test_extensions_and_options_precede_augmentation(self):
        dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")
        before, after = dockerfile.split("RUN /opt/keycloak/bin/kc.sh build")
        for fragment in (
            "COPY --from=provider --chown=keycloak:keycloak --chmod=644 /src/target/pinvou-mobile-authenticator-1.0.0.jar",
            "COPY themes/pinvou /opt/keycloak/themes/pinvou",
            "RUN touch -m --date=@1756684800 /opt/keycloak/providers/pinvou-mobile-authenticator.jar",
            "ENV KC_DB=postgres KC_HEALTH_ENABLED=true KC_METRICS_ENABLED=true",
        ):
            self.assertIn(fragment, before)
        self.assertIn("COPY --from=builder /opt/keycloak/ /opt/keycloak/", after)
        self.assertIn("ENV KC_DB=postgres KC_HEALTH_ENABLED=true KC_METRICS_ENABLED=true", after)
        registration = (ROOT / "mobile-authenticator/src/main/resources/META-INF/services/"
                        "org.keycloak.authentication.AuthenticatorFactory").read_text(encoding="utf-8")
        self.assertEqual(set(registration.splitlines()), {
            "com.pinvou.identity.MobileDirectGrantAuthenticatorFactory",
            "com.pinvou.identity.PinvouBrowserAuthenticatorFactory",
        })
        for filename in ("theme.properties", "pinvou-login.ftl", "messages/messages_zh_CN.properties",
                         "messages/messages_en.properties", "messages/messages_ja.properties"):
            self.assertTrue((ROOT / "themes/pinvou/login" / filename).is_file())

    def test_no_runtime_secrets_or_whole_context_copy(self):
        text = (ROOT / "Dockerfile").read_text(encoding="utf-8")
        instructions = "\n".join(line for line in text.splitlines() if not line.startswith("#"))
        for forbidden in ("PASSWORD", "SECRET", "TOKEN", "BOOTSTRAP_ADMIN", "KC_DB_URL",
                          "pinvou-dev-realm", "start-dev", "--import-realm", ".key", "COPY . "):
            self.assertNotIn(forbidden, instructions)


if __name__ == "__main__":
    unittest.main()
