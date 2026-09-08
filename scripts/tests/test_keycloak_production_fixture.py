import copy
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location(
    "keycloak_production_fixture", Path(__file__).resolve().parents[1] / "account-platform-keycloak-production-fixture.py")
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class ProductionFixtureBoundaryTests(unittest.TestCase):
    def test_exact_mounts_reject_extra_missing_writable_or_anonymous(self):
        expected = [("/temporary/issuer", "/fixture", True)]
        temporary = {"/tmp": "rw,size=128m"}
        meta = {"Mounts": [{"Type": "bind", "Source": "/temporary/issuer", "Destination": "/fixture", "RW": False}],
                "HostConfig": {"Tmpfs": temporary}}
        fixture.verify_mounts(meta, expected, temporary)
        variants = []
        missing = copy.deepcopy(meta)
        missing["Mounts"] = []
        variants.append(missing)
        extra = copy.deepcopy(meta)
        extra["Mounts"].append({"Type": "bind", "Source": "/other", "Destination": "/other", "RW": False})
        variants.append(extra)
        writable = copy.deepcopy(meta)
        writable["Mounts"][0]["RW"] = True
        variants.append(writable)
        anonymous = copy.deepcopy(meta)
        anonymous["Mounts"][0]["Type"] = "volume"
        variants.append(anonymous)
        extra_tmp = copy.deepcopy(meta)
        extra_tmp["HostConfig"]["Tmpfs"]["/other"] = "rw"
        variants.append(extra_tmp)
        for variant in variants:
            with self.assertRaises(RuntimeError):
                fixture.verify_mounts(variant, expected, temporary)
        fixture.verify_mounts(meta, expected, temporary)

    def test_actual_security_limits_fail_closed(self):
        limits = {"ReadonlyRootfs": True, "PortBindings": {}, "CapDrop": ["ALL"],
                  "SecurityOpt": ["no-new-privileges:true"], "Memory": 1024, "NanoCpus": 1000000000,
                  "PidsLimit": 32, "Ulimits": [{"Name": "core", "Soft": 0, "Hard": 0}]}
        fixture.verify_limits({"HostConfig": limits}, 1024, 32)
        for key, value in (("ReadonlyRootfs", False), ("PortBindings", {"8443/tcp": [{}]}),
                           ("CapDrop", []), ("SecurityOpt", []), ("SecurityOpt", ["no-new-privileges:false"]),
                           ("SecurityOpt", ["no-new-privileges-pretend"]), ("Memory", 0), ("NanoCpus", 0),
                           ("PidsLimit", 0), ("Ulimits", [{"Name": "core", "Soft": 0, "Hard": 1}])):
            changed = {**limits, key: value}
            with self.assertRaises(RuntimeError):
                fixture.verify_limits({"HostConfig": changed}, 1024, 32)


if __name__ == "__main__":
    unittest.main()
