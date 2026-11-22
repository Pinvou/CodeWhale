"""Reviewed, conservative input domains for queue result reuse.

This is not a path-filter waiver. Unknown paths affect every job. A contract
includes whole dependency domains, rather than guessing dynamic imports or
individual test names. Git entries retain names, modes, symlinks and gitlinks.
"""
import hashlib
import json
import re
from functools import lru_cache

DOMAINS = frozenset(('pinvou3-app', 'CodeWhale', 'private-runtimes', 'pinvou-cli',
                     'pinvou-knowledge', 'account-platform', 'remote-control-relay',
                     'team-relay-hub', 'update-release-console'))
RUST = frozenset(('pinvou3-app', 'CodeWhale', 'private-runtimes', 'pinvou-cli',
                  'pinvou-knowledge', 'remote-control-relay'))
CONTRACTS = {
    **{name: RUST for name in ('rust-test', 'rust-lint', 'cli-test', 'cli-lint',
        'knowledge-rust', 'windows-rust-test', 'macos-rust-check', 'macos-cli-check',
        'rust-loongarch64-check')},
    **{name: DOMAINS for name in ('account-platform-test', 'account-financial-test',
        'account-closure-test', 'relay-test', 'windows-relay-dacl',
        'windows-relay-tools-test', 'team-relay-hub-test', 'update-release-console-test')},
    # Frontend/native packaging contracts inspect other product source trees.
    # Keep all domains until a narrower transitive closure has been reviewed.
    **{name: DOMAINS for name in ('version-consistency', 'frontend-test',
        'frontend-browser-test', 'release-contract-test', 'windows-static-updater-contract',
        'windows-rustup-repair-test', 'windows-codex-runtime-test', 'windows-runtime-test',
        'macos-codex-runtime-test')},
}
POLICY_KEYS = ('trusted_backflow', 'pure_release_backflow')
OBSERVATIONS = ('reused_jobs', 'reuse_verified', 'reuse_source')
NEEDS_SCALAR = re.compile(
    r"\bneeds(?:\.[\w-]+\.outputs\.[\w-]+|(?:\.[\w-]+|\[\s*(['\"])[\w-]+\1\s*\])\.result)"
    r"(?=\s*(?:$|[}),=!&|<>]))"
)


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'),
                                     ensure_ascii=False).encode()).hexdigest()


@lru_cache(maxsize=8)
def blocks(workflow):
    return dict(re.findall(r'^  ([\w-]+):\n(.*?)(?=^  [\w-]+:|\Z)',
                           workflow.split('\njobs:\n', 1)[1], re.M | re.S))


@lru_cache(maxsize=128)
def contexts(workflow, name):
    """Include producer job configuration as well as the consumer's own block."""
    jobs = blocks(workflow)
    selected, pending = {}, [name]
    while pending:
        current = pending.pop()
        if current in selected or current == 'changes':
            continue  # routing outputs are fingerprinted individually below
        if current not in jobs:
            raise ValueError('unknown producer job')
        block = selected[current] = jobs[current]
        needs = re.search(r'^    needs: (.+)$', block, re.M)
        if needs:
            value = needs[1].strip()
            if not re.fullmatch(r'[\w-]+|\[[\w, -]+\]', value):
                raise ValueError('unsupported producer dependency')
            pending.extend(re.findall(r'[\w-]+', value))
        elif re.search(r'^    needs:', block, re.M):
            raise ValueError('unsupported producer dependency')
    return workflow.split('\njobs:\n', 1)[0] + '\n' + '\n'.join(selected.values())


def selected(path, domains):
    # Root files, scripts, all workflow/actions, docs, deploy and newly introduced
    # roots are shared. Adding a directory never silently creates a blind spot.
    root = path.split('/', 1)[0]
    return root not in DOMAINS or root in domains


@lru_cache(maxsize=8)
def known_variables(workflow):
    return frozenset(re.findall(r'\bvars\.([A-Za-z_][A-Za-z0-9_]*)', workflow))


def fingerprint(name, workflow, entries, changed, configuration, routing):
    domains = CONTRACTS.get(name)
    if domains is None:
        return None  # unsupported jobs execute; never invent an empty contract
    context = contexts(workflow, name)
    # Named vars used only by a different job can change independently. Unknown
    # vars, image/tool epochs, and dynamic/bracket access remain global fences.
    known = known_variables(workflow)
    used = set(re.findall(r'\bvars\.([A-Za-z_][A-Za-z0-9_]*)', context))
    dynamic = bool(re.search(r'\bvars\s*\[|toJSON\(vars\)', context))
    config = configuration if dynamic else {key: value for key, value in configuration.items()
        if key not in known or key in used or key.startswith('PINVOU_CI_REUSE_')}
    keys = set(re.findall(r'needs\.changes\.outputs\.([\w-]+)', context)) - set(OBSERVATIONS)
    keys.update(POLICY_KEYS)
    # An explicit scalar output is already tracked above. Whole-object or
    # indirect reads can observe the mode, so keep it unless all needs reads
    # in this job and its producers are statically accounted for.
    if re.search(r'\bneeds\b(?![ \t]*:)', NEEDS_SCALAR.sub('', context), re.I):
        keys.add('backflow_mode')
    route = {key: routing.get(key, 'full' if key == 'backflow_mode' else 'false') for key in keys}
    return digest({'contract': 1, 'domains': sorted(domains),
                   'entries': [row for row in entries if selected(row[-1], domains)],
                   # Diff-selected smokes depend on base→tested changes too.
                   'changes': [row for row in changed if selected(row[-1], domains)],
                   'configuration': config, 'routing': route})
