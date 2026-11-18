import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = (ROOT / '.github/workflows/pr-check.yml').read_text(encoding='utf-8')
BROWSER = WORKFLOW.split('\n  frontend-browser-test:', 1)[1].split('\n  relay-test:', 1)[0]


def selection_script(name):
    block = BROWSER.split('      - name: ' + name + '\n', 1)[1].split('\n      - name:', 1)[0]
    run = re.search(r'(?m)^        run: \|\n((?:          .*\n|\n)*)', block)[1]
    # Execute the actual workflow's Git preparation and diff, stopping before
    # the Node selection/Chromium pipeline rather than reimplementing it.
    return re.split(r'\\\n\s*\|\s*node\b', textwrap.dedent(run), maxsplit=1)[0] + '\n'


class FrontendCheckoutTests(unittest.TestCase):
    def test_checkout_depths_do_not_fetch_every_branch_and_tag(self):
        logic = WORKFLOW.split('\n  frontend-test:', 1)[1].split('\n  frontend-browser-test:', 1)[0]
        self.assertIn('fetch-depth: 1', logic)
        self.assertIn('fetch-depth: 2', BROWSER)
        self.assertNotIn('fetch-depth: 0', logic + BROWSER)
        self.assertNotIn('--unshallow', logic + BROWSER)

    @unittest.skipUnless(os.name != 'nt' and shutil.which('bash') and shutil.which('git'), 'Linux Git integration')
    def test_actual_pr_and_queue_diffs_match_full_history_from_shallow_checkouts(self):
        with tempfile.TemporaryDirectory(prefix='frontend-checkout-') as directory:
            root = Path(directory)
            source = root / 'source'

            def git(cwd, *args, check=True):
                return subprocess.run(['git', *args], cwd=cwd, text=True, capture_output=True, check=check, timeout=20)

            source.mkdir()
            git(source, 'init', '-b', 'main')
            git(source, 'config', 'user.name', 'CI Fixture')
            git(source, 'config', 'user.email', 'fixture@example.invalid')
            git(source, 'config', 'commit.gpgsign', 'false')

            def commit(name):
                (source / name).write_text(name, encoding='utf-8')
                git(source, 'add', '.')
                git(source, 'commit', '-m', name)
                return git(source, 'rev-parse', 'HEAD').stdout.strip()

            commit('base.txt')
            git(source, 'checkout', '-b', 'pr')
            pr = commit('pr.txt')
            git(source, 'checkout', 'main')
            base = commit('main.txt')
            git(source, 'merge', '--no-ff', 'pr', '-m', 'tested merge')
            tested = git(source, 'rev-parse', 'HEAD').stdout.strip()
            git(source, 'branch', 'tested-pr')
            git(source, 'checkout', '-b', 'queue')
            for index in range(4):
                queue = commit(f'queue-{index}.txt')
            git(source, 'tag', 'unrelated-tag')

            def execute(branch, name, variables):
                target = root / branch
                git(root, 'clone', '--no-tags', '--single-branch', '--depth=2', '--branch', branch, source.as_uri(), str(target))
                if branch == 'queue':
                    self.assertNotEqual(git(target, 'cat-file', '-e', base + '^{commit}', check=False).returncode, 0)
                result = subprocess.run(['bash', '-euo', 'pipefail', '-c', selection_script(name)], cwd=target,
                                        env={**os.environ, **variables}, text=True, capture_output=True, timeout=25)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, git(source, 'diff', '--name-only', base, variables.get('TESTED_SHA', queue)).stdout)
                self.assertEqual(git(target, 'rev-parse', '--is-shallow-repository').stdout.strip(), 'true')
                self.assertEqual(git(target, 'tag', '--list').stdout.strip(), '')
                return target

            execute('tested-pr', 'Ready PR 定向浏览器 smoke', {'TESTED_SHA': tested, 'PR_HEAD_SHA': pr})
            target = execute('queue', 'Merge Queue diff-selected browser smoke', {'BASE_SHA': base, 'HEAD_SHA': queue})
            invalid = subprocess.run(['bash', '-euo', 'pipefail', '-c', selection_script('Merge Queue diff-selected browser smoke')],
                                     cwd=target, env={**os.environ, 'BASE_SHA': '0' * 40, 'HEAD_SHA': queue},
                                     text=True, capture_output=True, timeout=25)
            self.assertNotEqual(invalid.returncode, 0, 'an unavailable exact base must fail instead of using a moving ref')


if __name__ == '__main__':
    unittest.main()
