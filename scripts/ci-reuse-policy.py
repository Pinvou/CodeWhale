"""Conservative queue execution plans for the repository's literal workflow dialect.

Unknown conditions require successful evidence; they never waive a step. This
module does not evaluate Python, shell or arbitrary Actions expressions.
"""
import json
import itertools
import math
import re

CONTEXT_JOBS = {'changes', 'required-gate', 'commit-message', 'merge-authored-audit', 'backflow-portability', 'fast-gate'}
CONTENT_JOBS = {
    'version-consistency', 'frontend-test', 'relay-test', 'account-platform-test', 'account-closure-test',
    'windows-relay-dacl', 'windows-relay-tools-test', 'team-relay-hub-test', 'update-release-console-test',
    'release-contract-test', 'knowledge-rust', 'rust-lint', 'rust-test', 'cli-test', 'cli-lint',
    'windows-static-updater-contract', 'windows-rust-test', 'windows-rustup-repair-test', 'macos-rust-check',
    'windows-codex-runtime-test', 'windows-runtime-test', 'macos-codex-runtime-test', 'macos-cli-check',
    'rust-loongarch64-check',
}
UNKNOWN = object()
TOKEN = re.compile(r"\s*(==|!=|&&|\|\||[!(),]|'(?:''|[^'])*'|[A-Za-z_][A-Za-z0-9_.-]*)")


def truth(value):
    return UNKNOWN if value is UNKNOWN else bool(value)


def condition(expression, context):
    expression = expression.strip().removeprefix('${{').removesuffix('}}').strip()
    tokens, offset = [], 0
    while offset < len(expression):
        match = TOKEN.match(expression, offset)
        if not match:
            return UNKNOWN
        tokens.append(match[1]); offset = match.end()
    position = 0

    def atom():
        nonlocal position
        token = tokens[position]; position += 1
        if token == '!':
            value = truth(atom())
            return UNKNOWN if value is UNKNOWN else not value
        if token == '(':
            value = logical_or()
            if tokens[position] != ')': raise ValueError('unbalanced condition')
            position += 1
            return value
        if token.startswith("'"): return token[1:-1].replace("''", "'")
        if token in ('true', 'false'): return token == 'true'
        if position < len(tokens) and tokens[position] == '(':
            position += 1
            if tokens[position] != ')': raise ValueError('unsupported function arguments')
            position += 1
            return {'always': True, 'success': True, 'cancelled': False}.get(token, UNKNOWN)
        return context.get(token, UNKNOWN)

    def comparison():
        nonlocal position
        left = atom()
        if position < len(tokens) and tokens[position] in ('==', '!='):
            operator = tokens[position]; position += 1; right = atom()
            if left is UNKNOWN or right is UNKNOWN: return UNKNOWN
            # Avoid guessing Actions' loose cross-type coercions.
            if type(left) is not type(right): return UNKNOWN
            equal = left.casefold() == right.casefold() if isinstance(left, str) else left == right
            return equal if operator == '==' else not equal
        return left

    def logical_and():
        nonlocal position
        value = comparison()
        while position < len(tokens) and tokens[position] == '&&':
            position += 1; right = comparison()
            a, b = truth(value), truth(right)
            value = False if a is False or b is False else UNKNOWN if a is UNKNOWN or b is UNKNOWN else True
        return value

    def logical_or():
        nonlocal position
        value = logical_and()
        while position < len(tokens) and tokens[position] == '||':
            position += 1; right = logical_and()
            a, b = truth(value), truth(right)
            value = True if a is True or b is True else UNKNOWN if a is UNKNOWN or b is UNKNOWN else False
        return value

    try:
        value = logical_or()
        return truth(value) if position == len(tokens) else UNKNOWN
    except (IndexError, ValueError):
        return UNKNOWN


def scalar(block, key, indent):
    match = re.search(r'^' + ' ' * indent + re.escape(key) + r': ([^\n]+)', block, re.M)
    if not match: return None
    value = literal(match[1])
    if value in ('>-', '>', '|', '|-'):
        tail = block[match.end():].splitlines()[1:]
        lines = []
        for line in tail:
            if line.strip() and len(line) - len(line.lstrip()) <= indent: break
            lines.append(line.strip())
        value = ' '.join(lines).strip()
    return value


def literal(value):
    value = value.strip()
    if value.startswith('"'):
        try:
            decoded, _ = json.JSONDecoder().raw_decode(value)
            return decoded if isinstance(decoded, str) else value
        except ValueError:
            return value
    if value.startswith("'"):
        match = re.match(r"'((?:''|[^'])*)'", value)
        if match: return match[1].replace("''", "'")
    return re.split(r'\s+#', value, maxsplit=1)[0].rstrip()


def catalog(workflow, *, jobs=None):
    body = workflow.split('\njobs:\n', 1)[1]
    allowed = CONTENT_JOBS if jobs is None else jobs
    result = {}
    for match in re.finditer(r'^  ([\w-]+):\n(.*?)(?=^  [\w-]+:|\Z)', body, re.M | re.S):
        name, block = match.groups()
        if name not in allowed or name in CONTEXT_JOBS or re.search(r'^    (?:outputs|uses):', block, re.M): continue
        # Per-run outputs and context-dependent matrix/runner selection require
        # a separate producer/consumer or input contract, not result reuse.
        runner = scalar(block, 'runs-on', 4) or ''
        strategy = re.search(r'^    strategy:\n(.*?)(?=^    \S|\Z)', block, re.M | re.S)
        if 'github.' in runner or (strategy and 'github.' in strategy[1]): continue
        display = scalar(block, 'name', 4) or name
        pattern, fields, offset = '', [], 0
        for expression in re.finditer(r'\$\{\{\s*matrix\.([\w-]+)\s*\}\}', display):
            pattern += re.escape(display[offset:expression.start()]) + r'([^()\r\n]+)'
            fields.append(expression[1]); offset = expression.end()
        pattern += re.escape(display[offset:])
        if '${{' in re.sub(r'\$\{\{\s*matrix\.[\w-]+\s*\}\}', '', display): continue
        if strategy and not fields: pattern += r' \([^()\r\n]+\)'
        steps = []
        for step in re.finditer(r'^      - (.*?)(?=^      - |\Z)', block, re.M | re.S):
            text = step.group()
            label = re.match(r'^      - name: (.+)', text)
            run = bool(re.search(r'^        run:', text, re.M) or re.match(r'^      - run:', text))
            uses = re.search(r'(?:^      - |^        )uses: (.+)', text, re.M)
            # Caches and diagnostic transport do not prove product execution.
            optional = bool(uses and (uses[1].startswith(('actions/cache', 'Swatinem/rust-cache'))
                                     or re.search(r'^        continue-on-error: true$', text, re.M)))
            if label and label[1].startswith('Warn when ') and re.search(r"^        run: echo '::warning::[^\n]*'$", text, re.M):
                optional = True
            steps.append({'name': literal(label[1]) if label else None, 'run': run,
                          'if': scalar(text, 'if', 8), 'optional': optional})
        if any(step['run'] for step in steps) and not any(step['run'] and not step['name'] for step in steps):
            result[name] = {'pattern': re.compile('^' + pattern + '$'), 'fields': fields, 'steps': steps,
                            'runner': runner, 'display': display, 'strategy': strategy[1] if strategy else None}
    return result


def routing_keys(workflow):
    changes = re.search(r'^  changes:\n(.*?)(?=^  [\w-]+:|\Z)', workflow, re.M | re.S)[1]
    outputs = re.search(r'^    outputs:\n(.*?)(?=^    steps:)', changes, re.M | re.S)[1]
    return tuple(key for key in re.findall(r'^      (\w+):', outputs, re.M)
                 if key not in ('reused_jobs', 'reuse_verified', 'reuse_source'))


def matches(policy, display):
    match = policy['pattern'].fullmatch(display)
    return None if not match else dict(zip(policy['fields'], match.groups()))


def matrix_rows(policy, configuration):
    if policy['strategy'] is None: return [{}]
    strategy = policy['strategy']
    expression = scalar(strategy, 'matrix', 6)
    if expression:
        match = re.fullmatch(r"\$\{\{ fromJSON\(vars\.PINVOU_WINDOWS_ENABLED != 'false' && \(vars\.PINVOU_MACOS_ENABLED != 'false' && '([^']+)' \|\| '([^']+)'\) \|\| \(vars\.PINVOU_MACOS_ENABLED != 'false' && '([^']+)' \|\| '([^']+)'\)\) \}\}", expression)
        if not match: return None
        windows = configuration.get('PINVOU_WINDOWS_ENABLED') != 'false'
        macos = configuration.get('PINVOU_MACOS_ENABLED') != 'false'
        rows = json.loads(match[1 if windows and macos else 2 if windows else 3 if macos else 4])
        if not isinstance(rows, dict) or set(rows) != {'include'}: return None
        rows = rows['include']
        return rows if isinstance(rows, list) and rows and all(isinstance(row, dict) and row for row in rows) else None
    axes = []
    for line in strategy.splitlines():
        if not line.startswith('        ') or not line.strip(): continue
        match = re.fullmatch(r'        ([\w-]+): \[([^\]]+)\]', line)
        if not match: return None
        values = [value.strip().strip('"\'') for value in match[2].split(',')]
        if any(not re.fullmatch(r'[\w.-]+', value) for value in values): return None
        axes.append((match[1], values))
    if not axes: return None
    if math.prod(len(values) for _, values in axes) > 100: return None
    return [dict(zip((key for key, _ in axes), values)) for values in itertools.product(*(values for _, values in axes))]


def display_name(policy, row):
    if policy['fields']:
        return re.sub(r'\$\{\{\s*matrix\.([\w-]+)\s*\}\}', lambda match: str(row[match[1]]), policy['display'])
    return policy['display'] + (' (' + ', '.join(map(str, row.values())) + ')' if row else '')


def complete_group(policy, jobs, routing, head_ref, configuration):
    rows = matrix_rows(policy, configuration)
    if rows is None: return False
    expected = [display_name(policy, row) for row in rows]
    if sorted(job.get('name', '') for job in jobs) != sorted(expected): return False
    return all(complete(policy, job, routing, head_ref, configuration) for job in jobs)


def labels(policy, matrix, configuration):
    runner = policy['runner']
    machine = matrix.get('os', matrix.get('runs-on'))
    if '${{' not in runner: return [runner.strip("'\"")]
    if machine and machine.startswith('macos'): return [machine]
    system = 'WINDOWS' if machine and machine.startswith('windows') else 'LINUX' if machine and machine.startswith('ubuntu') else None
    names = re.findall(r'vars\.(PINVOU_(?:LINUX|WINDOWS)(?:_(?:HEAVY|MEDIUM|LIGHT))?_RUNNER)', runner)
    if names:
        system = system or ('WINDOWS' if '_WINDOWS' in names[0] else 'LINUX')
        for name in names:
            if '_' + system not in name: continue
            if configuration.get(name):
                value = json.loads(configuration[name])
                if not isinstance(value, list) or not value or any(not isinstance(item, str) for item in value):
                    raise ValueError('invalid runner labels')
                return value
        return [machine or ('windows-latest' if system == 'WINDOWS' else 'ubuntu-22.04')]
    if machine and runner.strip() in ('${{ matrix.os }}', '${{ matrix.runs-on }}'): return [machine]
    return None


def complete(policy, job, routing, head_ref, configuration):
    matrix = matches(policy, job.get('name', ''))
    if matrix is None or job.get('conclusion') != 'success' or not job.get('steps'): return False
    rows = matrix_rows(policy, configuration)
    if rows is None: return False
    matching = [row for row in rows if display_name(policy, row) == job['name']]
    if len(matching) != 1: return False
    matrix = matching[0]
    expected = labels(policy, matrix, configuration)
    if expected is None or sorted(job.get('labels', [])) != sorted(expected): return False
    context = {'github.event_name': 'merge_group', 'github.ref': head_ref,
               'env.RUN_HEAVY_RUST_CHECKS': 'true'}
    context.update({'matrix.' + key: value for key, value in matrix.items()})
    context.update({'needs.changes.outputs.' + key: value for key, value in routing.items()})
    actual_labels = [value.lower() for value in job.get('labels', [])]
    for platform, prefixes in (('Windows', ('windows',)), ('macOS', ('macos',)), ('Linux', ('linux', 'ubuntu'))):
        if any(label.startswith(prefixes) for label in actual_labels): context['runner.os'] = platform
    witnessed = 0
    for step in policy['steps']:
        if step['optional'] or (step['if'] is not None and condition(step['if'], context) is False): continue
        if not step['name']:
            # Unnamed run commands still must have a successful API step.
            # Missing/ambiguous API evidence is rejected below.
            if step['run']:
                unnamed = [item for item in job['steps'] if item.get('name', '').startswith('Run ')
                           and item.get('conclusion') == 'success']
                if not unnamed: return False
            continue
        required = step['name']
        # Both paths run the same diff selector on the proven base/tree. The
        # source PR's named browser smoke is its queue-path execution witness.
        if required == 'Merge Queue diff-selected browser smoke': required = 'Ready PR 定向浏览器 smoke'
        found = [item for item in job['steps'] if item.get('name') == required]
        if len(found) != 1 or found[0].get('conclusion') != 'success': return False
        witnessed += int(step['run'])
    return witnessed > 0
