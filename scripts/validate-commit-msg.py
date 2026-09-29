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

    if semantic_desc[-1] in FORBIDDEN_ENDING_PUNCTUATION:
        errors.append(f"{label}: description must not end with punctuation")

    if normalized_desc in PROHIBITED_DESCRIPTIONS:
        errors.append(f"{label}: description is too vague: {semantic_desc}")

    return errors


def validate_signoff(
    raw: str,
    label: str,
    author_name: str | None = None,
    author_email: str | None = None,
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
        f"{label}: Signed-off-by must match commit author "
        f"'{author_name} <{author_email}>'"
    ]


def is_trusted_bot(author_name: str, author_email: str) -> bool:
    identity = (author_name.strip().casefold(), author_email.strip().casefold())
    return identity in TRUSTED_BOT_AUTHORS


def git(*args: str) -> str:
    return subprocess.check_output(("git", *args), text=True, encoding="utf-8").strip()


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
    commit_log = git(
        "log",
        "--format=%H%x00%P%x00%an%x00%ae%x00%B%x1e",
        f"{base}..{head}",
        "--not",
        LEGACY_HISTORY_CUTOFF,
    )
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
        if not is_trusted_bot(author_name, author_email):
            errors.extend(
                validate_signoff(
                    raw,
                    label,
                    author_name if require_matching_signoff else None,
                    author_email if require_matching_signoff else None,
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
