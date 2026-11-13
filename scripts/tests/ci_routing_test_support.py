"""Resolve the optional runner settings before testing default CI policy.

The existing policy suites describe the default hosted, three-platform setup.
Decode the literal JSON matrices rather than pinning their former YAML spelling.
Unrecognized or invalid matrix shapes fail instead of disappearing from tests.
"""
import json
import re

MATRIX = re.compile(
    r"(?m)^( *)matrix: \$\{\{ fromJSON\(vars\.PINVOU_WINDOWS_ENABLED != 'false' && \(vars\.PINVOU_MACOS_ENABLED != 'false' && '([^']+)' \|\| '([^']+)'\) \|\| \(vars\.PINVOU_MACOS_ENABLED != 'false' && '([^']+)' \|\| '([^']+)'\)\) \}\}$"
)

# Only this reviewed expression can be rendered as a default hosted route.
# Unknown trust-policy edits must stay visible to the existing exact assertions.
WINDOWS_PR_IDENTITY = (
    "(github.event_name == 'pull_request' && github.repository != '' && "
    "github.repository_id != '' && "
    "github.event.pull_request.head.repo.full_name == github.repository && "
    "github.event.pull_request.base.repo.full_name == github.repository && "
    "github.event.pull_request.head.repo.id == github.repository_id && "
    "github.event.pull_request.base.repo.id == github.repository_id)"
)
WINDOWS_POOL_TRUSTS = tuple(
    "(github.event_name == 'push' || github.event_name == '" + event + "' || "
    + WINDOWS_PR_IDENTITY + ")"
    for event in ("merge_group", "workflow_dispatch")
)

def matrix_rows(value):
    matrix = json.loads(value)
    if not isinstance(matrix, dict) or set(matrix) != {'include'}:
        raise ValueError('GitHub strategy.matrix must be a map containing include')
    rows = matrix['include']
    if not isinstance(rows, list) or not rows or not all(isinstance(row, dict) and row for row in rows):
        raise ValueError('The platform matrix must contain nonempty row maps')
    return rows

def default_ci_routing(source, *, native_enabled=True, windows_enabled=None, macos_enabled=None):
    windows_enabled = native_enabled if windows_enabled is None else windows_enabled
    macos_enabled = native_enabled if macos_enabled is None else macos_enabled
    def expand(match):
        choice = 2 if windows_enabled and macos_enabled else 3 if windows_enabled else 4 if macos_enabled else 5
        rows = matrix_rows(match.group(choice))
        for index in range(2, 6):
            matrix_rows(match.group(index))
        indent = match.group(1)
        if all(set(row) == {'os'} for row in rows):
            return indent + 'matrix:\n' + indent + '  os: [' + ', '.join(row['os'] for row in rows) + ']'
        lines = [indent + 'matrix:', indent + '  include:']
        for row in rows:
            for index, (key, value) in enumerate(row.items()):
                scalar = json.dumps(value) if isinstance(value, bool) or value == '' else str(value)
                lines.append(indent + ('    - ' if index == 0 else '      ') + key + ': ' + scalar)
        return '\n'.join(lines)
    source = MATRIX.sub(expand, source)
    # These legacy suites render unset runner variables in the default hosted
    # configuration. The dedicated Windows trust suite replays the unrendered
    # expressions for fork/missing metadata and configured private pools.
    source = re.sub(
        r"fromJSON\((?:" + "|".join(map(re.escape, WINDOWS_POOL_TRUSTS)) + r") && "
        r"\((vars\.PINVOU_WINDOWS_[^()\n]+)\) \|\| '\[\"windows-latest\"\]'\)",
        r"fromJSON(\1)", source,
    )
    source = re.sub(r"vars\.PINVOU_WINDOWS_(?:HEAVY|MEDIUM|LIGHT)_RUNNER \|\| ", '', source)
    source = re.sub(r"runs-on: \$\{\{ fromJSON\(vars\.PINVOU_WINDOWS_RUNNER \|\| '\[\"windows-latest\"\]'\) \}\}", 'runs-on: windows-latest', source)
    source = re.sub(r" \|\| matrix\.(os|runs-on) == 'windows-latest' && fromJSON\(vars\.PINVOU_WINDOWS_RUNNER \|\| '\[\"windows-latest\"\]'\)", '', source)
    source = re.sub(r"vars\.PINVOU_LINUX_(?:HEAVY|MEDIUM|LIGHT)_RUNNER \|\| ", '', source)
    source = re.sub(r"runs-on: \$\{\{ fromJSON\(vars\.PINVOU_LINUX_RUNNER \|\| '\[\"ubuntu-22.04\"\]'\) \}\}", 'runs-on: ubuntu-22.04', source)
    source = re.sub(r"runs-on: \$\{\{ matrix\.(os|runs-on) == 'ubuntu-22.04' && fromJSON\(vars\.PINVOU_LINUX_RUNNER \|\| '\[\"ubuntu-22.04\"\]'\) \|\| matrix\.\1 \}\}", lambda m: 'runs-on: ${{ matrix.' + m.group(1) + ' }}', source)
    if native_enabled:
        source = re.sub(r"(?m)^ *vars\.PINVOU_(?:WINDOWS|MACOS)_ENABLED != 'false' &&\n", '', source)
        source = re.sub(r" *&& vars\.PINVOU_(?:WINDOWS|MACOS)_ENABLED != 'false'", '', source)
        source = re.sub(r"vars\.PINVOU_(?:WINDOWS|MACOS)_ENABLED != 'false' && ", '', source)
        source = re.sub(r'(?m)^( *WINDOWS_RUST_REQUIRED_BY_POLICY: \$\{\{ )\((.*)\)( \}\})$', r'\1\2\3', source)
    source = source.replace("cache: ${{ runner.environment == 'github-hosted' && 'npm' || '' }}", 'cache: npm')
    # In the default hosted mode this early self-hosted exec is not entered;
    # the existing suites continue checking the unchanged destructive recipe.
    source = re.sub(r'(?m)^          if \[\[ "\$RUNNER_ENVIRONMENT" == self-hosted \]\]; then\n            exec python3 scripts/ci-rust-disk\.py --check-only[^\n]*\n          fi\n', '', source)
    source = re.sub(r'(?m)^          if \[\[ "\$RUNNER_ENVIRONMENT" == self-hosted \]\]; then\n            python3 scripts/ci-rust-disk\.py --check-only\n          else\n((?:            [^\n]*\n)+)          fi\n', lambda m: '\n'.join(line[2:] for line in m.group(1).splitlines()) + '\n', source)
    return source
