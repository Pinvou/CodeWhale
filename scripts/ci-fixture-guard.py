"""Prevent new tests from accidentally bypassing shared resource factories.

This is a repository regression guard, not a SQL or JavaScript security parser.
Runtime ownership/identity checks remain authoritative. Negative probes must be
explicitly reviewed here instead of being exempted by a magic inline comment.

Coverage is Go test database DDL and JavaScript server/browser factories. This
guard does not inspect Docker commands or Python/shell subprocess paths. The CI
job's PATH wrapper owns ordinary Docker CLI leases; absolute Docker executables
and direct Docker APIs bypass that wrapper and need equivalent explicit leases.
"""
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]
PROCESS_ONLY = {
    'pinvou3-app/tests/browser_wrapper_lazy.test.mjs': 'The generated child is the browser-wrapper executable under test; its temporary standalone script cannot import repository helpers. The supervising process owns its complete tree.',
}


def without_comments(source):
    # Preserve quoted SQL/imports but remove both comment forms. This avoids
    # treating documentation and expected-error strings as resource operations.
    token = re.compile(r'"(?:\\.|[^"\\])*"|\'(?:\\.|[^\'\\])*\'|`[^`]*`|//[^\n]*|/\*[\s\S]*?\*/')
    return token.sub(lambda m: re.sub(r'[^\n]', ' ', m[0]) if m[0].startswith(('//', '/*')) else m[0], source)


def call_end(source, start):
    depth, quote, escape = 1, None, False
    for index in range(start, len(source)):
        char = source[index]
        if quote:
            if escape:
                escape = False
            elif char == '\\' and quote != '`':
                escape = True
            elif char == quote:
                quote = None
        elif char in ('"', "'", '`'):
            quote = char
        elif char == '(':
            depth += 1
        elif char == ')':
            depth -= 1
            if depth == 0:
                return index
    return len(source)


def native_factory(source, package, operation):
    literal = '(?:' + '|'.join(re.escape(quote + package + quote) for quote in ["'", '"', '`']) + ')'
    binding = r'(\{[^}]*\}|\*\s+as\s+[\w$]+|[\w$]+(?:\s*,\s*\{[^}]*\})?)'
    patterns = [r'\bimport\s+' + binding + r'\s+from\s*' + literal,
                r'\b(?:const|let|var)\s+' + binding + r'\s*=\s*(?:await\s+)?(?:require|import)\s*\(\s*' + literal + r'\s*\)']
    bindings = [match.group(1) for pattern in patterns for match in re.finditer(pattern, source)]
    for imported in bindings:
        names = re.search(r'\{([^}]*)\}', imported)
        if names and any(re.match(r'\s*' + re.escape(operation) + r'\b', part) for part in names[1].split(',')):
            return True
        namespace = imported.split(',')[0].strip()
        if namespace.startswith('*'):
            namespace = namespace.split()[-1]
        if re.fullmatch(r'[\w$]+', namespace):
            quoted_operation = '(?:' + '|'.join(re.escape(quote + operation + quote) for quote in ["'", '"', '`']) + ')'
            member = r'\b' + re.escape(namespace) + r'\s*(?:\.\s*' + re.escape(operation) + r'\b|\[\s*' + quoted_operation + r'\s*\])'
            if re.search(member, source):
                return True
    return bool(re.search(r'(?:require|import)\s*\(\s*' + literal + r'\s*\)\s*\)?\s*\.\s*' + re.escape(operation) + r'\b', source))


def violations(root=ROOT):
    failures = []
    for path in (root / 'account-platform').rglob('*_test.go'):
        relative = path.relative_to(root).as_posix()
        if '/internal/cifixture/' in relative:
            continue
        source = without_comments(path.read_text(encoding='utf-8'))
        for match in re.finditer(r'\b(?:Exec|exec)\s*\(', source):
            arguments = source[match.end():call_end(source, match.end())]
            strings = re.findall(r'"((?:\\.|[^"\\])*)"|`([^`]*)`', arguments)
            sql = ' '.join(a or b for a, b in strings)
            if re.search(r'\b(?:CREATE|DROP)\s+DATABASE\b', sql, re.I):
                failures.append(f'{relative}:{source[:match.start()].count(chr(10))+1}: database DDL must use a cifixture creation lease')
    for path in (root / 'pinvou3-app/tests').rglob('*'):
        if path.suffix not in {'.js', '.mjs', '.cjs'} or not path.is_file():
            continue
        relative = path.relative_to(root).as_posix()
        if '/helpers/' in relative or relative in PROCESS_ONLY:
            continue
        source = without_comments(path.read_text(encoding='utf-8'))
        for package, operation in [('vite', 'createServer'), ('puppeteer-core', 'launch'), *[(prefix + name, 'createServer') for prefix in ['', 'node:'] for name in ['http', 'https', 'net']]]:
            if native_factory(source, package, operation):
                failures.append(f'{relative}: {operation} must use the shared fixture factory for {package}')
    for relative, reason in PROCESS_ONLY.items():
        if root == ROOT and (not (root / relative).is_file() or len(reason) < 40):
            failures.append('invalid reviewed process-only fixture exception: ' + relative)
    return failures


if __name__ == '__main__':
    failures = violations()
    print('\n'.join(failures) if failures else 'Shared fixture creation/retirement guard passed')
    raise SystemExit(bool(failures))
