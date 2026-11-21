#!/usr/bin/env python3
"""Reuse successful same-repository PR product checks for identical queue inputs.

No match, missing evidence, an API failure or an unsupported queue shape means
run the tests. Git history/policy checks are deliberately outside the allowlist.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import http.client
import io
import json
import lzma
import os
from pathlib import Path
import re
import runpy
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request
import zipfile
import zlib
from functools import lru_cache

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'scripts/lib'))
from ci_https import storage_opener  # noqa: E402
WORKFLOW = os.environ.get('CI_REUSE_WORKFLOW', '.github/workflows/pr-check.yml')
PROFILES = {
    '.github/workflows/pr-check.yml': ('required-gate', None),
    '.github/workflows/rustc-wrapper-smoke.yml': ('wrapper-gate', {'smoke'}),
    '.github/workflows/evidence-validator-container.yml': ('validator-gate', {'linux-amd64'}),
}
if WORKFLOW not in PROFILES:
    raise ValueError('unsupported workflow proof profile')
JOB_SCOPED = WORKFLOW == '.github/workflows/pr-check.yml'
POLICY = (WORKFLOW, 'scripts/ci-test-reuse.py', 'scripts/ci-reuse-policy.py', 'scripts/lib/ci_https.py',
          'scripts/ci-network-node-options.cjs', 'scripts/ci-artifact-node-options.cjs',
          'scripts/ci-artifact-upload-diagnostics.cjs', 'scripts/ci-rust-network.cjs',
          'scripts/ci-network-environment.py', 'scripts/ci-rust-dist.py',
          'scripts/ci-download.py', 'scripts/ci-rust-cache-paths.cjs',
          '.github/actions/linux-rust-cache/action.yml') + (('scripts/ci-job-inputs.py',) if JOB_SCOPED else ())
MAX_AGE = 24 * 60 * 60
MAX_BYTES = 65536
PLAN = runpy.run_path(str(ROOT / 'scripts/ci-reuse-policy.py'))
INPUTS = runpy.run_path(str(ROOT / 'scripts/ci-job-inputs.py')) if JOB_SCOPED else None
WORKFLOW_SOURCE = (ROOT / WORKFLOW).read_text(encoding='utf-8')
GATE_NAME, PROFILE_JOBS = PROFILES[WORKFLOW]
JOBS = PLAN['catalog'](WORKFLOW_SOURCE, jobs=PROFILE_JOBS)
ROUTING_KEYS = PLAN['routing_keys'](WORKFLOW_SOURCE)


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
    # All route and policy outputs matter, including Rust/full-regression and
    # matrix routing. Reuse's own outputs are observations, not test inputs.
    return {key: outputs.get(key, 'full' if key == 'backflow_mode' else 'false') for key in ROUTING_KEYS}


def fingerprint(tested, base, configuration, routing=None):
    tested, base = sha(tested), sha(base)
    return {'tree': sha(git('rev-parse', tested + '^{tree}')), 'base': base,
            'configuration': digest(configuration), 'selection': digest(selection(routing or {})),
            'policy': {path: sha(git('rev-parse', f'{tested}:{path}')) for path in POLICY}}


@lru_cache(maxsize=8)
def tree_entries(revision):
    rows = []
    for record in git('ls-tree', '-rz', '--full-tree', sha(revision)).split('\0'):
        if record:
            header, path = record.split('\t', 1)
            mode, kind, oid = header.split()
            rows.append((mode, kind, sha(oid), path))
    return rows


@lru_cache(maxsize=8)
def changed_entries(base, tested):
    fields = git('diff', '--raw', '--no-abbrev', '--no-renames', '-z', sha(base), sha(tested)).split('\0')
    return [(fields[i], fields[i+1]) for i in range(0, len(fields)-1, 2)]


def job_inputs(tested, base, configuration, routing):
    entries, changed = tree_entries(tested), changed_entries(base, tested)
    return {name: INPUTS['fingerprint'](name, WORKFLOW_SOURCE, entries, changed, configuration, routing)
            for name in JOBS}


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
    if pr.get('draft') is not False:
        print('::notice::CI success proof not recorded: draft PRs do not donate evidence')
        return
    if pr['head']['repo']['full_name'] != repository:
        print('::notice::CI success proof not recorded: source PR is not in this repository')
        return
    tested, head = map(sha, (os.environ['TESTED_SHA'], pr['head']['sha']))
    tested_parents = parents(tested)
    if len(tested_parents) != 2 or tested_parents[1] != head:
        print('::notice::CI success proof not recorded: tested commit is not the PR merge')
        return  # A custom head checkout is not proof for the merged result.
    # Event/API PR associations can retain an older base SHA. The tested Git
    # object is the immutable source of the base actually covered by this run.
    base = tested_parents[0]
    routing = needs.get('changes', {}).get('outputs', {})
    value = {'schema': 2 if JOB_SCOPED else 1, 'repository': repository, 'pr': number(pr['number']),
             'run': number(os.environ['GITHUB_RUN_ID']), 'attempt': number(os.environ['GITHUB_RUN_ATTEMPT']),
             'head': head, 'tested': tested, 'base_ref': pr['base']['ref'],
             'fingerprint': fingerprint(tested, base, configuration, routing),
             'created': datetime.now(timezone.utc).isoformat(),
             'jobs': sorted(name for name in JOBS if needs.get(name, {}).get('result') == 'success')}
    if JOB_SCOPED:
        value['job_inputs'] = job_inputs(tested, base, configuration, routing)
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
        self.opener = storage_opener(SafeRedirect())
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
        total = None
        for page in range(1, pages + 1):
            value = self.request(path + ('&' if '?' in path else '?') + f'per_page=100&page={page}')
            items = value[field]
            total = value.get('total_count', total)
            result.extend(items)
            if len(items) < 100:
                break
        if total is not None and len(result) < total:
            raise ValueError('incomplete evidence pagination')
        return result


def read_proof(raw):
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        members = archive.infolist()
        if len(members) != 1 or members[0].filename != 'proof.json' or members[0].file_size > MAX_BYTES:
            raise ValueError('unexpected proof archive')
        with archive.open(members[0]) as stream:
            content = stream.read(MAX_BYTES + 1)
        if len(content) > MAX_BYTES:
            raise ValueError('oversized proof content')
        proof = json.loads(content)
        if not isinstance(proof, dict):
            raise ValueError('unexpected proof format')
        return proof


def log_rejection(reason):
    print(f'Queue reuse: miss ({reason})')


def read_evidence(reason, read, *args, **kwargs):
    try:
        return read(*args, **kwargs)
    except Exception:
        # Log only the caller's fixed phase, never an exception or response.
        log_rejection(reason)
        raise


def verified_jobs(proof, run, jobs, expected, repository, pr_number, head, now, configuration, routing, head_ref):
    modern = JOB_SCOPED and proof.get('schema') == 2
    if (proof.get('schema') not in (1, 2) or proof.get('repository') != repository
            or (proof.get('schema') == 2 and not JOB_SCOPED)
            or proof.get('pr') != pr_number or proof.get('run') != run['id']
            or proof.get('attempt') != run['run_attempt'] or proof.get('head') != head
            or (not modern and proof.get('fingerprint') != expected)
            or (modern and proof.get('fingerprint', {}).get('policy') != expected['policy'])):
        log_rejection('proof_identity_or_fingerprint_mismatch')
        return []
    age = (now - datetime.fromisoformat(proof['created'])).total_seconds()
    if not 0 <= age <= MAX_AGE:
        log_rejection('proof_age_outside_window')
        return []
    if not isinstance(proof.get('jobs'), list) or len(set(proof['jobs'])) != len(proof['jobs']):
        log_rejection('invalid_proof_jobs')
        return []
    gates = [job for job in jobs if job.get('name') == GATE_NAME]
    if len(gates) != 1 or gates[0].get('conclusion') != 'success':
        log_rejection('source_gate_not_successful')
        return []
    result = []
    inputs = job_inputs(os.environ['TESTED_SHA'], expected['base'], configuration, routing) if modern else {}
    for name in proof['jobs']:
        if name not in JOBS:
            log_rejection('invalid_proof_jobs')
            return []
        if modern and (inputs.get(name) is None or proof.get('job_inputs', {}).get(name) != inputs[name]):
            print(f'Queue reuse: {name}: execute (inputs_changed_or_unclassified)')
            continue
        candidates = [job for job in jobs if PLAN['matches'](JOBS[name], job.get('name', '')) is not None]
        if (candidates and all(job.get('run_id') == run['id'] for job in candidates)
                and PLAN['complete_group'](JOBS[name], candidates, routing, head_ref, configuration)):
            result.append(name)
        else:
            print(f'Queue reuse: {name}: execute (incomplete_execution_evidence)')
    return sorted(result)


def lookup(event, configuration, api, *, now=None):
    now = now or datetime.now(timezone.utc)
    repository = os.environ['GITHUB_REPOSITORY']
    if configuration.get('PINVOU_CI_REUSE_FORCE_RUN') == 'true':
        log_rejection('force_run_enabled')
        return [], None
    group = event['merge_group']
    tested, base = sha(group['head_sha']), sha(group['base_sha'])
    if tested != sha(os.environ['TESTED_SHA']):
        log_rejection('tested_commit_mismatch')
        return [], None
    routing = json.loads(os.environ.get('CI_REUSE_ROUTING_FILTERS', os.environ.get('CI_REUSE_ROUTING', '{}')))
    policy_outputs = json.loads(os.environ.get('CI_REUSE_ROUTING_POLICY', '{}'))
    if 'CI_REUSE_ROUTING_FILTERS' in os.environ:
        routing = {key: value for key, value in routing.items() if key in ROUTING_KEYS}
        routing.update({key: policy_outputs.get(key) or fallback for key, fallback in
                        (('trusted_backflow', 'false'), ('backflow_mode', 'full'), ('pure_release_backflow', 'false'))})
    expected = read_evidence('queue_git_objects_unavailable', fingerprint, tested, base, configuration, routing)
    # A PR which edits the verifier or workflow cannot authorize its own reuse.
    if any(expected['policy'][path] != read_evidence('queue_git_objects_unavailable', git, 'rev-parse', f'{base}:{path}') for path in POLICY):
        log_rejection('queue_policy_changed')
        return [], None
    branch = group['base_ref'].removeprefix('refs/heads/')
    match = re.fullmatch(r'refs/heads/gh-readonly-queue/' + re.escape(branch) + r'/pr-([1-9][0-9]*)-([0-9a-f]{40})', group['head_ref'])
    if not match or match[2] != base:
        log_rejection('unsupported_queue_ref')
        return [], None
    pr_number = number(match[1])
    pr = read_evidence('pr_metadata_unavailable', api.request, f'pulls/{pr_number}')
    if (pr['head']['repo']['full_name'] != repository or pr.get('draft') is not False
            or pr['base']['ref'] != branch):
        log_rejection('ineligible_pr')
        return [], None
    head = sha(pr['head']['sha'])
    if read_evidence('queue_git_objects_unavailable', parents, tested) != [base, head]:
        log_rejection('unsupported_queue_parents')
        return [], None  # Combined/multi-member queues retain full validation.
    runs_path = f'actions/workflows/{WORKFLOW.rsplit("/", 1)[1]}/runs?event=pull_request&status=success&head_sha={head}&per_page=30'
    runs = read_evidence('source_runs_unavailable', api.request, runs_path)['workflow_runs']
    if not runs:
        log_rejection('no_successful_pr_run')
    for run in runs[:30]:
        if (run.get('event') != 'pull_request' or run.get('conclusion') != 'success'
                or run.get('head_sha') != head or run.get('path') != WORKFLOW
                or run.get('head_repository', {}).get('full_name') != repository
                or not any(row.get('number') == pr_number and row.get('head', {}).get('sha') == head
                           and row.get('base', {}).get('ref') == branch
                           for row in run.get('pull_requests', []))):
            log_rejection('source_run_identity_mismatch')
            continue
        run_id, attempt = number(run['id']), number(run['run_attempt'])
        if (now - datetime.fromisoformat(run['updated_at'].replace('Z', '+00:00'))).total_seconds() > MAX_AGE:
            log_rejection('source_run_outside_window')
            continue
        artifacts = read_evidence('artifact_listing_unavailable', api.pages, f'actions/runs/{run_id}/artifacts', 'artifacts')
        matching = [item for item in artifacts if item.get('name') == f'ci-success-proof-{attempt}']
        current = [item for item in matching if item.get('expired') is False]
        if not matching:
            log_rejection('artifact_missing')
            continue
        if not current:
            log_rejection('artifact_expired_or_unavailable')
            continue
        if len(current) != 1:
            log_rejection('artifact_ambiguous')
            continue
        artifact = current[0]
        if (artifact.get('size_in_bytes', MAX_BYTES + 1) > MAX_BYTES
                or artifact.get('workflow_run', {}).get('id') != run_id
                or artifact['workflow_run'].get('head_sha') != head):
            log_rejection('artifact_identity_or_size_mismatch')
            continue
        try:
            raw = read_evidence('artifact_download_failed', api.request,
                                f'actions/artifacts/{number(artifact["id"])}/zip', binary=True)
            proof = read_evidence('artifact_decode_failed', read_proof, raw)
        except (OSError, ValueError, EOFError, http.client.HTTPException,
                lzma.LZMAError, zipfile.BadZipFile, RuntimeError, zlib.error):
            # A broken artifact does not invalidate independently verified
            # evidence from another run for the same head.
            continue
        if proof.get('base_ref') != branch:
            log_rejection('proof_base_ref_mismatch')
            continue
        # PR associations above establish ownership, not an immutable base.
        # The producer's tested Git object identifies its own immutable base/tree;
        # schema 2 compares the queue's inputs separately for each job below.
        # Fetching it through the API is read-only; no PR-controlled URL is used.
        source = proof.get('fingerprint', {})
        if not isinstance(source, dict) or source.get('policy') != expected['policy']:
            log_rejection('source_policy_mismatch')
            continue
        commit = read_evidence('source_commit_unavailable', api.request, f'git/commits/{sha(proof["tested"])}')
        source_base = source.get('base') if proof.get('schema') == 2 else base
        source_tree = source.get('tree') if proof.get('schema') == 2 else expected['tree']
        if (commit.get('tree', {}).get('sha') != source_tree
                or [item.get('sha') for item in commit.get('parents', [])] != [source_base, head]):
            log_rejection('source_commit_mismatch')
            continue
        if proof.get('schema') == 2:
            if not JOB_SCOPED:
                log_rejection('unsupported_proof_schema')
                continue
            # Read immutable source objects, never an artifact-controlled URL.
            # A shallow queue checkout need not have the earlier PR merge.
            try:
                git('cat-file', '-e', sha(proof['tested']) + '^{commit}')
                git('cat-file', '-e', sha(source_base) + '^{commit}')
            except subprocess.CalledProcessError:
                read_evidence('source_git_objects_unavailable', git, 'fetch', '--no-tags', '--depth=2', 'origin', sha(proof['tested']))
            if read_evidence('source_git_objects_unavailable', parents, proof['tested']) != [source_base, head]:
                log_rejection('source_commit_mismatch')
                continue
            source_policy = read_evidence('source_git_objects_unavailable', fingerprint,
                                          proof['tested'], source_base, configuration, routing)['policy']
            if (source_policy != expected['policy']
                    or any(read_evidence('source_git_objects_unavailable', git, 'rev-parse', f'{source_base}:{path}')
                           != expected['policy'][path] for path in POLICY)):
                log_rejection('source_policy_mismatch')
                continue
            # The artifact cannot lie about a source tree or dependency digest.
            # Recompute source file/diff inputs using current configuration and
            # routing. Equality with the recorded digest additionally establishes
            # that those effective settings did not change for this job.
            source_inputs = read_evidence('source_job_inputs_unavailable', job_inputs,
                                          proof['tested'], source_base, configuration, routing)
            proof = dict(proof, job_inputs={name: value for name, value in proof.get('job_inputs', {}).items()
                                         if value is not None and source_inputs.get(name) == value})
        jobs = read_evidence('source_jobs_unavailable', api.pages, f'actions/runs/{run_id}/attempts/{attempt}/jobs', 'jobs')
        reusable = verified_jobs(proof, run, jobs, expected, repository, pr_number, head, now, configuration, routing, group['head_ref'])
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
            else:
                print('::notice::No compatible PR success proof; retained gates will execute normally')
    except Exception:
        # Do not print remote responses, configuration or exception text: they
        # can contain credentials or operator-specific runtime configuration.
        print('::notice::CI success proof unavailable; retained gates will execute normally')


if __name__ == '__main__':
    main()
