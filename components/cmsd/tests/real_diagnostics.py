"""Shared bounded, read-only diagnostic CLI capture for opt-in local tests."""
import json
import os
import subprocess
import time


def capture(binary, root, label, expected=None, overrides=None):
    env = {k: v for k, v in os.environ.items() if not k.startswith('CMSD_')}
    env.update(CMSD_CONFIG=str(root / 'cmsd.toml'))
    env.update(overrides or {})
    before = time.monotonic()
    result = subprocess.run([str(binary), '--diagnose'], env=env, capture_output=True, timeout=7)
    value = json.loads(result.stdout)
    assert value['read_only'] is True and value['format'] == 'megacube-cmsd-diagnostic-v1'
    assert value['interpretation']['inference_verified'] is False
    assert value['recovery']['automatic_actions'] is False and value['recovery']['legacy_fallback'] is False
    assert not result.stderr and result.returncode in (0, 3, 4)
    if expected is not None:
        assert result.returncode == expected, (label, result.returncode, value)
    elapsed = time.monotonic() - before
    (root / ('diagnostic-' + label + '.json')).write_text(json.dumps({
        'exit_code': result.returncode, 'elapsed_seconds': elapsed, 'report': value}, indent=2) + '\n')
    return value, result.returncode, elapsed
