#!/usr/bin/env python3
"""Verify that the independent CI matrix executes every financial CLI group once."""
from pathlib import Path
import re
import runpy

ROOT = Path(__file__).resolve().parents[1]
PLAN = runpy.run_path(str(ROOT / 'scripts/ci-reuse-policy.py'))
GROUPS = runpy.run_path(str(ROOT / 'scripts/test-token-financial-postgres.py'))['GROUPS']


def verify_groups(workflow):
    policies = PLAN['catalog'](workflow, jobs={'account-financial-test'})
    policy = policies.get('account-financial-test')
    if policy is None:
        raise ValueError('financial CI job is missing or has an unsupported definition')
    rows = PLAN['matrix_rows'](policy, {})
    if rows is None or any(set(row) != {'group'} for row in rows):
        raise ValueError('financial CI requires one literal group axis')
    groups = [row['group'] for row in rows]
    if len(groups) != len(GROUPS) or set(groups) != set(GROUPS):
        raise ValueError('financial CI must include every CLI group exactly once')
    strategy = policy['strategy'] or ''
    if re.findall(r'^      fail-fast: (.+)$', strategy, re.M) != ['false']:
        raise ValueError('financial CI must let every group finish')
    if re.findall(r'^      max-parallel: (.+)$', strategy, re.M) != [str(len(GROUPS))]:
        raise ValueError('financial CI concurrency must match the complete group count')
    return groups


if __name__ == '__main__':
    groups = verify_groups((ROOT / '.github/workflows/pr-check.yml').read_text(encoding='utf-8'))
    print('Verified financial CI groups: ' + ', '.join(groups))
