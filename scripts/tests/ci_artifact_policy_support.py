"""Use the same independently checked transport projection as Node contracts."""
import json
from pathlib import Path
import shutil
import subprocess


def legacy_artifact_policy(source):
    if 'PINVOU_CI_NETWORK_NODE_OPTIONS' not in source:
        return source
    result = subprocess.run([shutil.which('node') or 'node',
        str(Path(__file__).with_name('ci-artifact-policy-support.cjs'))],
        input=json.dumps(source), capture_output=True, text=True, encoding='utf-8', timeout=10)
    if result.returncode:
        raise AssertionError('Unreviewed CI transport policy')
    return json.loads(result.stdout)
