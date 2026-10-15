#!/usr/bin/env python3
"""Validate commit messages against the pinvou3 mandatory convention."""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from pathlib import Path


ALLOWED_TYPES = (
    "feat",
    "fix",
    "refactor",
    "perf",
    "docs",
    "style",
    "test",
    "build",
    "ci",
    "chore",
    "revert",
)

SUBJECT_RE = re.compile(
    r"^(?P<type>{types})(?:\((?P<scope>[^()\s:]+)\))?(?P<breaking>!)?: (?P<desc>.+)$".format(
        types="|".join(ALLOWED_TYPES)
    )
)

CJK_RE = re.compile(r"[\u4e00-\u9fff]")
SIGNOFF_RE = re.compile(
    r"(?im)^Signed-off-by:\s*(?P<name>[^<>\r\n]+?)\s*<(?P<email>[^<>\s]+)>\s*$"
)
GITHUB_PR_SUFFIX_RE = re.compile(r" \(#\d+\)$")
PROHIBITED_DESCRIPTIONS = {"update", "fix bug", "修改代码", "测试"}
FORBIDDEN_ENDING_PUNCTUATION = set("。.，,；;、！!？?：:")
DOC_PATH = "docs/Git Commit 信息规范文档.md"
TRUSTED_BOT_AUTHORS = {
    ("dependabot[bot]", "49699333+dependabot[bot]@users.noreply.github.com"),
    ("github-actions[bot]", "41898282+github-actions[bot]@users.noreply.github.com"),
}
# #235 首次把这条门禁带入 main；此前 Windows 分支累积的提交不回写历史。
# 新门禁只约束该快照之后的非 merge 提交，避免 GitHub 自动 merge commit
# 和合流前的历史提交永久打红 main。
LEGACY_HISTORY_CUTOFF = "deae3ca0141390c06e14aa93610645088b8966d4"

# #1932: 已合入集成分支的 GitHub squash 提交保留了原签署，但作者不同。
# 仅接受这 19 个不可变对象；不是身份别名，也不豁免标题或签署检查。
# 值为来源 PR，供审核追溯；新增提交不能按作者、分支或时间获得此例外。
HISTORICAL_SQUASH_SIGNOFFS = {
    "55d39fbe6a90f8fee4f2c4d6a5fa709e7974a6d3": 1829,
    "1f0fe5d2e37955b8c888a9c41dfa4e27108d1cc9": 1828,
    "089dc15342fdfa9991b54b37a55c03257823ccbb": 1825,
    "dd9358617743c42c672282a5cdfd471e02f1cfd9": 1824,
    "40b6bab756fd1ee734f9c8bd13f1ec5d3ced8ea0": 1826,
    "ec03916575049792649b649ab45de8fdd8418243": 1787,
    "5d6919fc217fca820e6007d5a6b62181346db2c9": 1785,
    "71914ebe49bb1ee6d33081bb4d35b0a5ddbfd917": 1782,
    "f6f589f77a967f6e9185b731f69cd761e14fe616": 1827,
    "19dba2851a3f95875e0221fc1459f337dbe462df": 1817,
    "554cb5eb34a342d997f72292b6772f64c9710917": 1816,
    "c24ccab1071e8bc2fe2b3c2079a06db5b5320b5a": 1814,
    "133fba77c7a4aa10b7c3bd61cae5f77419c6bff8": 1801,
    "11ba329bfd3362f2020bc6b05112b2ce60419685": 1799,
    "7385f5b6c44bba8ea32fbbc19ce1a78ec8b577d6": 1796,
    "e9ed3c15c705fb2f333fed557d9d927412d95ef9": 1795,
    "4340c11513aad6b9f9676981237dc8d327bab29f": 1794,
    "a7587edfc44c25ddc06e17fe0366cd457b104438": 1792,
    "c74061589a8428d3542184a1876189536e303110": 1791,
}
HISTORICAL_SQUASH_AUTHOR = ("zhuowp", "zwp-1989@qq.com")
HISTORICAL_SQUASH_SIGNER = ("z27014", "zhuo.wenpei@h3c.com")


def clean_commit_message(raw: str) -> list[str]:
    return [line.rstrip() for line in raw.splitlines() if not line.startswith("#")]


def first_subject(raw: str) -> str:
    for line in clean_commit_message(raw):
        if line.strip():
            return line.lstrip("\ufeff")
    return ""


def validate_text(raw: str, label: str) -> list[str]:
    subject = first_subject(raw)
    errors: list[str] = []

    if not subject:
        return [f"{label}: commit message is empty"]

    match = SUBJECT_RE.match(subject)
    if not match:
        return [
            f"{label}: first line must be '<type>[scope]: <中文简短描述>'",
            f"  allowed types: {', '.join(ALLOWED_TYPES)}",
            f"  example: fix(installer): 修复安装器资源输出目录",
            f"  actual: {subject}",
        ]

    desc = match.group("desc")
    semantic_desc = GITHUB_PR_SUFFIX_RE.sub("", desc)
    normalized_desc = semantic_desc.strip().lower()

    if semantic_desc != semantic_desc.strip():
        errors.append(f"{label}: description must not start or end with whitespace")

    if len(semantic_desc) > 50:
        errors.append(
            f"{label}: description must be 50 characters or fewer, got {len(semantic_desc)}"
        )

    if not CJK_RE.search(semantic_desc):
        errors.append(f"{label}: description must use Chinese")

    if semantic_desc and semantic_desc[-1] in FORBIDDEN_ENDING_PUNCTUATION:
        errors.append(f"{label}: description must not end with punctuation")

    if normalized_desc in PROHIBITED_DESCRIPTIONS:
        errors.append(f"{label}: description is too vague: {semantic_desc}")

    return errors


def validate_signoff(
    raw: str,
    label: str,
    author_name: str | None = None,
    author_email: str | None = None,
    *,
    identity_label: str = "commit author",
) -> list[str]:
    signoffs = list(SIGNOFF_RE.finditer(raw))
    if not signoffs:
        return [f"{label}: missing Signed-off-by trailer; use 'git commit -s'"]

    if author_name is None or author_email is None:
        return []

    expected_name = author_name.strip().casefold()
    expected_email = author_email.strip().casefold()
    for signoff in signoffs:
        if (
            signoff.group("name").strip().casefold() == expected_name
            and signoff.group("email").strip().casefold() == expected_email
        ):
            return []

    return [
        f"{label}: Signed-off-by must match {identity_label} "
        f"'{author_name} <{author_email}>'"
    ]


def is_trusted_bot(author_name: str, author_email: str) -> bool:
    identity = (author_name.strip().casefold(), author_email.strip().casefold())
    return identity in TRUSTED_BOT_AUTHORS


def git(*args: str) -> str:
    return subprocess.check_output(("git", *args), text=True, encoding="utf-8").strip()


def git_commit_exists(revision: str) -> bool:
    return (
        subprocess.run(
            ("git", "cat-file", "-e", f"{revision}^{{commit}}"),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        ).returncode
        == 0
    )


def validate_file(path: Path, *, require_signoff: bool = True) -> list[str]:
    raw = path.read_text(encoding="utf-8")
    errors = validate_text(raw, str(path))
    if require_signoff:
        errors.extend(validate_signoff(raw, str(path)))
    return errors


def validate_range(
    base: str,
    head: str,
    *,
    require_matching_signoff: bool = True,
) -> list[str]:
    # Shallow clones may not have the base or the legacy cutoff object; fall
    # back to the `head^!` revision (head with all parents excluded — exactly
    # the one commit) rather than letting `git log` fail on a missing
    # revision. A bare `head` would walk every reachable ancestor and, when
    # the cutoff object is missing too, mis-flag old nonconforming commits
    # the cutoff would have grandfathered; `^!` validates head alone in both
    # subcases. `^!` also works on a parentless root commit (verified:
    # selects the root alone).
    revision = f"{base}..{head}" if git_commit_exists(base) else f"{head}^!"
    log_args = [
        "log",
        "--format=%H%x00%P%x00%an%x00%ae%x00%B%x1e",
        revision,
    ]
    if git_commit_exists(LEGACY_HISTORY_CUTOFF):
        log_args.extend(("--not", LEGACY_HISTORY_CUTOFF))
    commit_log = git(*log_args)
    errors: list[str] = []

    for record in commit_log.split("\x1e"):
        record = record.strip("\r\n")
        if not record:
            continue
        fields = record.split("\x00", 4)
        if len(fields) != 5:
            raise RuntimeError("unable to parse git log output for commit validation")
        commit, parents, author_name, author_email, raw = fields
        label = f"{commit[:12]} {first_subject(raw)}"
        if len(parents.split()) > 1:
            continue

        errors.extend(validate_text(raw, label))
        historical = commit in HISTORICAL_SQUASH_SIGNOFFS
        if historical or not is_trusted_bot(author_name, author_email):
            expected_name, expected_email = author_name, author_email
            if historical:
                if (author_name, author_email) != HISTORICAL_SQUASH_AUTHOR:
                    errors.append(f"{label}: historical squash author does not match registration")
                expected_name, expected_email = HISTORICAL_SQUASH_SIGNER
            match_signer = require_matching_signoff or historical
            errors.extend(
                validate_signoff(
                    raw,
                    label,
                    expected_name if match_signer else None,
                    expected_email if match_signer else None,
                    identity_label="registered historical signer" if historical else "commit author",
                )
            )

    return errors


def main() -> int:
    parser = argparse.ArgumentParser(
        description=f"Validate commit messages. See {DOC_PATH}."
    )
    parser.add_argument("message_file", nargs="?", type=Path)
    parser.add_argument("--range", nargs=2, metavar=("BASE", "HEAD"))
    parser.add_argument(
        "--allow-signoff-author-mismatch",
        action="store_true",
        help=(
            "require a Signed-off-by trailer without matching the rewritten commit "
            "author (for GitHub squash commits on main)"
        ),
    )
    parser.add_argument(
        "--subject-only",
        action="store_true",
        help="validate only the subject convention for non-commit text such as a PR title",
    )
    args = parser.parse_args()

    if bool(args.message_file) == bool(args.range):
        parser.error("provide either a commit message file or --range BASE HEAD")
    if args.allow_signoff_author_mismatch and not args.range:
        parser.error("--allow-signoff-author-mismatch requires --range BASE HEAD")
    if args.subject_only and not args.message_file:
        parser.error("--subject-only requires a message file")

    errors = (
        validate_range(
            *args.range,
            require_matching_signoff=not args.allow_signoff_author_mismatch,
        )
        if args.range
        else validate_file(args.message_file, require_signoff=not args.subject_only)
    )

    if errors:
        print("Commit message does not follow the mandatory pinvou3 convention.", file=sys.stderr)
        print(f"See {DOC_PATH}", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
