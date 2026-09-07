import importlib.util
import json
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location(
    "workload_realm", Path(__file__).resolve().parents[1] / "account-platform-workload-realm.py"
)
realm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(realm)


class WorkloadRealmTests(unittest.TestCase):
    def test_exact_clients_no_secrets_or_custom_subjects(self):
        config = realm.build_realm()
        self.assertEqual([c["clientId"] for c in config["clients"]], list(realm.SERVICES))
        self.assertEqual(len(config["clients"]), 7)
        self.assertNotIn("users", config)
        self.assertNotIn("roles", config)
        for client in config["clients"]:
            self.assertNotIn("secret", client)
            self.assertNotIn("id", client)
            self.assertEqual(len(client["protocolMappers"]), 1)
            self.assertEqual(client["protocolMappers"][0]["protocolMapper"], "oidc-audience-mapper")

    def test_only_short_lived_service_credentials(self):
        config = realm.build_realm()
        self.assertEqual(config["sslRequired"], "all")
        self.assertEqual(config["accessTokenLifespan"], 300)
        self.assertFalse(config["registrationAllowed"])
        for name in ("clientScopes", "defaultDefaultClientScopes", "defaultOptionalClientScopes"):
            self.assertEqual(config[name], [])
        for client in config["clients"]:
            self.assertTrue(client["serviceAccountsEnabled"])
            for flag in ("publicClient", "standardFlowEnabled", "implicitFlowEnabled",
                         "directAccessGrantsEnabled", "fullScopeAllowed", "authorizationServicesEnabled"):
                self.assertFalse(client[flag])
            for name in ("defaultClientScopes", "optionalClientScopes", "redirectUris", "webOrigins"):
                self.assertEqual(client[name], [])
            self.assertEqual(client["attributes"]["access.token.lifespan"], "300")
            for name in ("use.refresh.tokens", "client_credentials.use_refresh_token",
                         "oauth2.device.authorization.grant.enabled", "oidc.ciba.grant.enabled",
                         "standard.token.exchange.enabled"):
                self.assertEqual(client["attributes"][name], "false")

    def test_separate_realm_and_bounded_audience(self):
        for name in ("master", "pinvou", "../pinvou-workload", "pinvou-workload?", "A-workload"):
            with self.assertRaises(ValueError):
                realm.build_realm(name)
        for audience in ("", "*", "a b", "a" * 129):
            with self.assertRaises(ValueError):
                realm.build_realm(audience=audience)
        self.assertEqual(realm.build_realm("pinvou-fixture-workload")["realm"], "pinvou-fixture-workload")

    def test_deterministic_and_independent(self):
        config = realm.build_realm()
        serialized = json.dumps(config, sort_keys=True)
        self.assertEqual(serialized, json.dumps(realm.build_realm(), sort_keys=True))
        config["clients"][0]["attributes"]["access.token.lifespan"] = "99999"
        self.assertEqual(realm.build_realm()["clients"][0]["attributes"]["access.token.lifespan"], "300")


if __name__ == "__main__":
    unittest.main()
