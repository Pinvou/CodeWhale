"""Exercise the actual installer's signer-set guard before any privileged work.

GnuPG machine records are mocked here; no key generation, download or apt runs.
The public-key append probe in review separately establishes reachability.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'account-platform-history-export-tools.sh'
APPROVED = 'B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8'


def shell_path(path):
    value = str(path).replace('\\', '/')
    if os.name == 'nt' and len(value) > 2 and value[1] == ':':
        return '/' + value[0].lower() + value[2:]
    return value


class HistorySigningKeyTests(unittest.TestCase):
    def check_records(self, records, *, parser_exit=0, accepted=False):
        shell = os.environ.get('PINVOU_TEST_BASH')
        if not shell and os.name == 'nt':
            shell = 'C:/Program Files/Git/bin/bash.exe'
        shell = shell or shutil.which('bash')
        self.assertTrue(shell, 'explicit POSIX shell is required')
        with tempfile.TemporaryDirectory(prefix='history-key-') as directory:
            path = Path(directory) / 'machine-records'
            path.write_text(records, encoding='utf8', newline='\n')
            command = """source "$1"
gpg() {
  [[ "$#" == 6 && "$1" == --homedir && "$2" == unused && "$3" == --batch && "$4" == --with-colons && "$5" == --show-keys && "$6" == "$RECORDS" ]] || return 19
  cat "$RECORDS"
  return "$KEY_PARSER_EXIT"
}
if verify_pgdg_key unused "$2"; then exit 0; else exit 17; fi
"""
            env = {**os.environ, 'KEY_PARSER_EXIT': str(parser_exit), 'RECORDS': shell_path(path)}
            result = subprocess.run([shell, '-c', command, 'history-key', shell_path(SCRIPT), shell_path(path)],
                                    env=env, capture_output=True, timeout=15)
            self.assertEqual(result.returncode, 0 if accepted else 17, result.stderr.decode('utf8', 'replace'))

    def test_single_approved_primary_with_signing_subkeys(self):
        self.check_records(f'pub:::::::::\nfpr:::::::::{APPROVED}:\nsub:::::::::\nfpr:::::::::{"1" * 40}:\n', accepted=True)

    def test_approved_primary_without_subkeys(self):
        self.check_records(f'pub:::::::::\nfpr:::::::::{APPROVED}:\n', accepted=True)

    def test_appended_foreign_primary_is_rejected(self):
        self.check_records(f'pub:::::::::\nfpr:::::::::{APPROVED}:\npub:::::::::\nfpr:::::::::{"2" * 40}:\n')

    def test_wrong_primary_is_rejected(self):
        self.check_records(f'pub:::::::::\nfpr:::::::::{"3" * 40}:\n')

    def test_secret_key_material_cannot_expand_public_trust(self):
        self.check_records(f'pub:::::::::\nfpr:::::::::{APPROVED}:\nsec:::::::::\nfpr:::::::::{"4" * 40}:\n')
        self.check_records(f'sec:::::::::\nfpr:::::::::{APPROVED}:\n')

    def test_empty_or_failed_parser_cannot_authorize_install(self):
        self.check_records('')
        self.check_records(f'pub:::::::::\nfpr:::::::::{APPROVED}:\n', parser_exit=2)


if __name__ == '__main__':
    unittest.main()
