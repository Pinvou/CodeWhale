"""Deterministic inventory contract checks; requires the jsonschema package."""
import copy
import json
from pathlib import Path
import unittest

try:
    import jsonschema
except ImportError:
    jsonschema = None

ROOT = Path(__file__).resolve().parents[2]


@unittest.skipIf(jsonschema is None, 'install jsonschema to validate inventory contracts')
class InventoryContractTests(unittest.TestCase):
    def setUp(self):
        self.local_schema = json.loads((ROOT/'docs/contracts/megacube-local-inventory-v1.schema.json').read_text())
        self.local = jsonschema.Draft202012Validator(self.local_schema, format_checker=jsonschema.FormatChecker())
        self.baseline = {'device_id': 'dev-fixture', 'identity_scope': 'local-state-only', 'identity_key_id': None,
                         'boot_id': '11111111-1111-4111-8111-111111111111', 'observed_at': '2026-10-18T00:00:00Z',
                         'source': 'cmsd', 'protocol_versions': ['v1'], 'capabilities': ['local.inventory'],
                         'remote_pairing': False, 'group_coordination': False}

    def test_default_contract_stays_local_even_with_a_signed_identity_endpoint(self):
        self.local.check_schema(self.local_schema)
        self.local.validate(self.baseline)
        signed = copy.deepcopy(self.baseline)
        signed['capabilities'].append('local.identity.signed')
        self.local.validate(signed)
        for key, value in [('identity_key_id', 'ed25519:'+'a'*64), ('remote_pairing', True),
                           ('protocol_versions', ['v1', 'v2']), ('group_coordination', True)]:
            changed = {**self.baseline, key: value}
            self.assertFalse(self.local.is_valid(changed), key)

    def test_remote_identity_cannot_be_inferred_from_local_capabilities(self):
        for key, value in [('identity_scope', 'authenticated-control-v2'),
                           ('identity_key_id', 'ed25519:'+'a'*64),
                           ('protocol_versions', ['v1', 'v2']), ('remote_pairing', True)]:
            self.assertFalse(self.local.is_valid({**self.baseline, key: value}), key)


if __name__ == '__main__':
    unittest.main()
