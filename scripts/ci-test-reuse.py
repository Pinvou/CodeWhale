#!/usr/bin/env python3
"""Reuse successful same-repository PR product checks for identical queue inputs.

No match, missing evidence, an API failure or an unsupported queue shape means
run the tests. Git history/policy checks are deliberately outside the allowlist.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import urllib.parse
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = '.github/workflows/pr-check.yml'
POLICY = (WORKFLOW, 'scripts/ci-test-reuse.py')
MAX_AGE = 24 * 60 * 60
MAX_BYTES = 65536
JOBS = {
    'account-platform-test': ('HEAVY', ('Test and vet all Account Platform Go modules', 'Token financial contracts on isolated PostgreSQL', 'Confirm Account test resource retirement')),
    'frontend-test': ('MEDIUM', ('前端逻辑测试 (pet 全套 + diff parser + markdown)', 'Ready PR 定向浏览器 smoke', 'Confirm test resource retirement')),
}


def git(*args):
    return subprocess.check_output(['git', *args], cwd=ROOT, text=True, encoding='utf-8', stderr=subprocess.DEVNULL, timeout=15).strip()


def sha(value):
    if not isinstance(value, str) or not re.fullmatch('[0-9a-f]{40}', value):
        raise ValueError('invalid revision')
    return value


def number(value):
    if isinstance(value, bool) or not str(value).isascii() or not str(value).isdigit() or int(value) <= 0:
        raise ValueError('invalid run identity')
    return int(value)


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode()).hexdigest()


def parents(revision):
    # checkout's depth=1 records a shallow boundary. Pretty-printing %P treats
    # that boundary as a root; the immutable object still has its real parents.
    header = git('cat-file', '-p', sha(revision)).split('\n\n', 1)[0]
    return [sha(line.removeprefix('parent ')) for line in header.splitlines() if line.startswith('parent ')]


def selection(outputs):
    return {key: outputs.get(key, 'full' if key == 'backflow_mode' else 'false')
            for key in ('frontend', 'pet', 'account_platform', 'pure_release_backflow', 'backflow_mode')}


def fingerprint(tested, base, configuration, routing=None):
    tested, base = sha(tested), sha(base)
    return {'tree': sha(git('rev-parse', tested + '^{tree}')), 'base': base,
            'configuration': digest(configuration), 'selection': digest(selection(routing or {})),
            'policy': {path: sha(git('rev-parse', f'{tested}:{path}')) for path in POLICY}}


def output(**values):
    with Path(os.environ['GITHUB_OUTPUT']).open('a', encoding='utf-8') as stream:
        for name, value in values.items():
            text = str(value)
            if '\n' in text or '\r' in text:
                raise ValueError('multiline output')
            stream.write(f'{name}={text}\n')


def read_context():
    return json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text(encoding='utf-8'))


def record(event, configuration, needs):
    pr = event['pull_request']
    repository = os.environ['GITHUB_REPOSITORY']
    if pr.get('draft') is not False or pr['head']['repo']['full_name'] != repository:
        return
    tested, base, head = map(sha, (os.environ['TESTED_SHA'], pr['base']['sha'], pr['head']['sha']))
    if parents(tested) != [base, head]:
        return  # A custom head checkout is not proof for the merged result.
    value = {'schema': 1, 'repository': repository, 'pr': number(pr['number']),
             'run': number(os.environ['GITHUB_RUN_ID']), 'attempt': number(os.environ['GITHUB_RUN_ATTEMPT']),
             'head': head, 'tested': tested, 'base_ref': pr['base']['ref'],
             'fingerprint': fingerprint(tested, base, configuration, needs.get('changes', {}).get('outputs', {})),
             'created': datetime.now(timezone.utc).isoformat(),
             'jobs': sorted(name for name in JOBS if needs.get(name, {}).get('result') == 'success')}
    directory = Path(tempfile.mkdtemp(prefix='ci-success-proof-', dir=os.environ['RUNNER_TEMP']))
    (directory / 'proof.json').write_text(json.dumps(value, sort_keys=True) + '\n', encoding='utf-8')
    output(directory=str(directory))


class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, new_url):
        if urllib.parse.urlsplit(new_url).scheme != 'https':
            raise ValueError('insecure artifact redirect')
        redirected = super().redirect_request(request, fp, code, message, headers, new_url)
        if urllib.parse.urlsplit(new_url).netloc != 'api.github.com':
            redirected.remove_header('Authorization')
        return redirected


class GitHub:
    def __init__(self, repository, token):
        if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository):
            raise ValueError('invalid repository')
        self.repository, self.token = repository, token
        self.opener = urllib.request.build_opener(SafeRedirect())
        self.deadline = time.monotonic() + 90

    def request(self, path, *, binary=False):
        request = urllib.request.Request(f'https://api.github.com/repos/{self.repository}/{path}', headers={
            'Authorization': 'Bearer ' + self.token, 'Accept': 'application/vnd.github+json',
            'X-GitHub-Api-Version': '2022-11-28', 'User-Agent': 'pinvou-ci-test-reuse'})
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError('lookup budget exhausted')
        with self.opener.open(request, timeout=min(15, remaining)) as response:
            raw = response.read((MAX_BYTES if binary else 4 * 1024 * 1024) + 1)
        if len(raw) > (MAX_BYTES if binary else 4 * 1024 * 1024):
            raise ValueError('oversized evidence')
        return raw if binary else json.loads(raw)

    def pages(self, path, field, *, pages=3):
        result = []
        for page in range(1, pages + 1):
            value = self.request(path + ('&' if '?' in path else '?') + f'per_page=100&page={page}')
            items = value[field]
            result.extend(items)
            if len(items) < 100:
                break
        return result


def read_proof(raw):
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        members = archive.infolist()
        if len(members) != 1 or members[0].filename != 'proof.json' or members[0].file_size > MAX_BYTES:
            raise ValueError('unexpected proof archive')
        return json.loads(archive.read(members[0]))


def runner_labels(configuration, pool):
    labels = json.loads(configuration.get(f'PINVOU_LINUX_{pool}_RUNNER')
                        or configuration.get('PINVOU_LINUX_RUNNER') or '["ubuntu-22.04"]')
    if not isinstance(labels, list) or not labels or any(not isinstance(label, str) or not label for label in labels):
        raise ValueError('invalid runner contract')
    return sorted(labels)


def verified_jobs(proof, run, jobs, expected, repository, pr_number, head, now, configuration):
    if (proof.get('schema') != 1 or proof.get('repository') != repository
            or proof.get('pr') != pr_number or proof.get('run') != run['id']
            or proof.get('attempt') != run['run_attempt'] or proof.get('head') != head
            or proof.get('fingerprint') != expected):
        return []
    age = (now - datetime.fromisoformat(proof['created'])).total_seconds()
    if not 0 <= age <= MAX_AGE or not isinstance(proof.get('jobs'), list) or len(set(proof['jobs'])) != len(proof['jobs']):
        return []
    gates = [job for job in jobs if job.get('name') == 'required-gate']
    if len(gates) != 1 or gates[0].get('conclusion') != 'success':
        return []
    result = []
    for name in proof['jobs']:
        if name not in JOBS:
            return []
        matches = [job for job in jobs if job.get('name') == name]
        if len(matches) != 1 or matches[0].get('conclusion') != 'success' or matches[0].get('run_id') != run['id']:
            continue
        if sorted(matches[0].get('labels', [])) != runner_labels(configuration, JOBS[name][0]):
            continue
        steps = matches[0].get('steps', [])
        if all(sum(step.get('name') == required and step.get('conclusion') == 'success' for step in steps) == 1
               for required in JOBS[name][1]):
            result.append(name)
    return sorted(result)


def lookup(event, configuration, api, *, now=None):
    now = now or datetime.now(timezone.utc)
    repository = os.environ['GITHUB_REPOSITORY']
    if configuration.get('PINVOU_CI_REUSE_FORCE_RUN') == 'true':
        return [], None
    group = event['merge_group']
    tested, base = sha(group['head_sha']), sha(group['base_sha'])
    if tested != sha(os.environ['TESTED_SHA']):
        return [], None
    expected = fingerprint(tested, base, configuration, json.loads(os.environ.get('CI_REUSE_ROUTING', '{}')))
    # A PR which edits the verifier or workflow cannot authorize its own reuse.
    if any(expected['policy'][path] != git('rev-parse', f'{base}:{path}') for path in POLICY):
        return [], None
    branch = group['base_ref'].removeprefix('refs/heads/')
    match = re.fullmatch(r'refs/heads/gh-readonly-queue/' + re.escape(branch) + r'/pr-([1-9][0-9]*)-([0-9a-f]{40})', group['head_ref'])
    if not match or match[2] != base:
        return [], None
    pr_number = number(match[1])
    pr = api.request(f'pulls/{pr_number}')
    if (pr['head']['repo']['full_name'] != repository or pr.get('draft') is not False
            or pr['base']['ref'] != branch):
        return [], None
    head = sha(pr['head']['sha'])
    if parents(tested) != [base, head]:
        return [], None  # Combined/multi-member queues retain full validation.
    runs = api.request('actions/workflows/pr-check.yml/runs?event=pull_request&status=success&head_sha=' + head + '&per_page=30')['workflow_runs']
    for run in runs[:30]:
        if (run.get('event') != 'pull_request' or run.get('conclusion') != 'success'
                or run.get('head_sha') != head or run.get('path') != WORKFLOW
                or run.get('head_repository', {}).get('full_name') != repository
                or not any(row.get('number') == pr_number and row.get('head', {}).get('sha') == head
                           and row.get('base', {}).get('sha') == base and row['base'].get('ref') == branch
                           for row in run.get('pull_requests', []))):
            continue
        run_id, attempt = number(run['id']), number(run['run_attempt'])
        if (now - datetime.fromisoformat(run['updated_at'].replace('Z', '+00:00'))).total_seconds() > MAX_AGE:
            continue
        artifacts = api.pages(f'actions/runs/{run_id}/artifacts', 'artifacts')
        artifacts = [item for item in artifacts if item.get('name') == f'ci-success-proof-{attempt}' and item.get('expired') is False]
        if len(artifacts) != 1:
            continue
        artifact = artifacts[0]
        if (artifact.get('size_in_bytes', MAX_BYTES + 1) > MAX_BYTES
                or artifact.get('workflow_run', {}).get('id') != run_id
                or artifact['workflow_run'].get('head_sha') != head):
            continue
        proof = read_proof(api.request(f'actions/artifacts/{number(artifact["id"])}/zip', binary=True))
        if proof.get('base_ref') != branch:
            continue
        # The producer's immutable tested SHA must identify the exact same tree.
        # Fetching it through the API is read-only; no PR-controlled URL is used.
        commit = api.request(f'git/commits/{sha(proof["tested"])}')
        if (commit.get('tree', {}).get('sha') != expected['tree']
                or [item.get('sha') for item in commit.get('parents', [])] != [base, head]):
            continue
        jobs = api.pages(f'actions/runs/{run_id}/attempts/{attempt}/jobs', 'jobs')
        reusable = verified_jobs(proof, run, jobs, expected, repository, pr_number, head, now, configuration)
        if reusable:
            return reusable, run_id
    return [], None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=('record', 'lookup'))
    command = parser.parse_args().command
    try:
        event = read_context()
        configuration = json.loads(os.environ['CI_REUSE_CONFIGURATION'])
        if command == 'record' and os.environ.get('GITHUB_EVENT_NAME') == 'pull_request':
            record(event, configuration, json.loads(os.environ['CI_REUSE_NEEDS']))
        elif command == 'lookup' and os.environ.get('GITHUB_EVENT_NAME') == 'merge_group':
            jobs, source = lookup(event, configuration, GitHub(os.environ['GITHUB_REPOSITORY'], os.environ['GITHUB_TOKEN']))
            if jobs:
                output(reused_jobs=json.dumps(jobs, separators=(',', ':')), reuse_verified='true', reuse_source=str(source))
                print(f'Verified identical test inputs against successful PR run {source}: ' + ', '.join(jobs))
    except Exception:
        # Do not print remote responses, configuration or exception text: they
        # can contain credentials or operator-specific runtime configuration.
        print('::notice::CI success proof unavailable; retained gates will execute normally')


if __name__ == '__main__':
    main()
