import json
import re
from ci_routing_test_support import default_ci_routing
import os
import shutil
import subprocess
import tempfile
import textwrap
import tomllib
import unittest
from fnmatch import fnmatchcase
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PR_WORKFLOW = ROOT / ".github/workflows/pr-check.yml"
RELEASE_WORKFLOW = ROOT / ".github/workflows/release-packages.yml"
PUBLIC_SUBMODULE_VERIFIER = ROOT / "scripts/verify-public-submodule.sh"
TAURI_MANIFEST = ROOT / "pinvou3-app/src-tauri/Cargo.toml"


def _extract_quoted_paths(block):
    """提取 YAML 块中 `- 'path'` / `- "path"` 形式的路径条目(保持文本序)。

    Round-40 review M4: 单引号形式之外还要认双引号形式——一条
    `- "pinvou-cli/**"` 若对提取器不可见,下面所有基于提取结果的 pin
    (存在性、死条目、成员、workflow 排除)都会被同一条目绕过。

    Round-41 review M8: 条目行尾的同列注释不再制造盲区——
    `- '!pinvou-cli/**' # trim scope` 对 dorny 是一条排除项,旧提取器
    (要求行以引号收尾)却看不见它。引号内出现的 `#` 属于路径本身
    (YAML 规范:注释以引号后的 `#` 开始),因此先取引号闭包、再剥其后的
    注释。闭包按引号配对(`[^']*` / `[^"]*`):贪婪闭包 `(.*)` 会把行尾
    注释里的引号一并吞进路径(`# was 'old/path'` 解析出一条 dorny 根本
    看不见的垃圾条目)。凡是引用形式的条目行(`- '…'` / `- "…"` 开头)
    却匹配不上引号闭包正则的,一律视为未识别形状直接让断言红掉——那
    正是"提取器看不见 → 下方全部 pin 可被同一条目绕过"的形状。

    Round-51 review: fail-closed 覆盖所有列表条目形状的行(可选空格 +
    `- ` 开头)。折叠标量(`- >-` + 缩进文本)与裸词条目(`- pinvou-cli/**`)
    对 dorny 都是真实的过滤条目(dorny 会把块重新按 YAML 解析),旧提取器
    却静默忽略——一条隐藏的 `!pinvou-cli/**` 排除即可静默跳过全部 CLI 腿。
    这类行一律让断言红掉。曾经的 `tight=False` 逃生门没有生产调用方,
    已删除:调用方必须先把 `- name:`、`- uses:` 等作业步骤行排除在切片
    之外,而不是让提取器吞掉未识别的条目行。
    """
    paths = []
    unrecognized = []
    for line in block.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        match = re.match(r"""^- ('([^']*)'|"([^"]*)")(?:\s+#.*)?$""", stripped)
        if match:
            paths.append(match.group(2) if match.group(2) is not None else match.group(3))
        elif stripped.startswith("- "):
            unrecognized.append(stripped)
    if unrecognized:
        raise AssertionError(
            "unrecognized path-filter entry(ies); the extractor cannot "
            "parse them, so every extractor-based pin below would be bypassable "
            "by the same entry — fix the quoting or move prose to a comment "
            "line: " + "; ".join(unrecognized)
        )
    return paths


def _without_yaml_comments(block):
    return "\n".join(
        line for line in block.splitlines() if not line.lstrip().startswith("#")
    )


def _without_inline_comments(block):
    """整行与行尾 `#` 注释都剥掉(引号内的 `#` 保留)。

    Round-4 review R3 of PR #745: `_without_yaml_comments` 只剥整行注释,
    一个从 node --test 列表里删掉、却仍被某条活行行尾注提及的套件
    (`... a.test.mjs # still runs b.test.mjs`)能让成员/全集 pin 继续假绿。
    剥掉行尾注释后,注释里的路径进不了任何提取结果;良性尾注只是被剥掉,
    不会造成假红。
    """
    stripped_lines = []
    for line in block.splitlines():
        if line.lstrip().startswith("#"):
            continue
        quote = None
        cut = len(line)
        for index, char in enumerate(line):
            if quote is not None:
                if char == quote:
                    quote = None
            elif char in ("'", '"'):
                quote = char
            elif char == "#" and index > 0 and line[index - 1] in (" ", "\t"):
                cut = index
                break
        stripped_lines.append(line[:cut].rstrip())
    return "\n".join(stripped_lines)


def _is_covered_by_trigger(entry, trigger_paths):
    """entry 被 trigger path 覆盖:完全相同,或 trigger 是其上层 `/**` 目录 glob。

    与 dorny/paths-filter 的 some-with-excludes 语义对齐:至少一条正向
    pattern 命中,且没有任何 `!` 排除条目命中(排除优先于命中)。忽略
    排除条目会让路由锁在 filter 组新增排除时仍虚报覆盖(fail-open)。
    """

    def _matches(pattern):
        return entry == pattern or (
            pattern.endswith("/**") and entry.startswith(pattern[:-2])
        )

    if not any(_matches(p) for p in trigger_paths if not p.startswith("!")):
        return False
    excludes = [p[1:] for p in trigger_paths if p.startswith("!")]
    return not any(_matches(p) for p in excludes)


def _extract_contract_read_targets(contract_test):
    """从 multiagent_plan_normalize.test.mjs 源码派生全部 src-tauri 读取目标。

    `read('src-tauri', ...)` 产出单文件目标,`path.join(here, '..', 'src-tauri',
    ...)` 产出 readdirSync 整目录拼接的 `/**` 目标。单引号与双引号形式都被
    接受:此前正则只匹配单引号,双引号的 `read("src-tauri", ...)` 会整体绕过
    路由锁(fail-open,经变异验证)。
    """
    targets = []
    for args in re.findall(
        r"read\(['\"]src-tauri['\"],\s*([^)]*)\)", contract_test
    ):
        parts = re.findall(r"['\"]([^'\"]*)['\"]", args)
        if not parts:
            raise AssertionError(f"无法解析的契约测试 read 目标: {args}")
        targets.append("pinvou3-app/src-tauri/" + "/".join(parts))
    for args in re.findall(
        r"path\.join\(here, ['\"]\.\.['\"], ['\"]src-tauri['\"],\s*([^)]*)\)",
        contract_test,
    ):
        parts = re.findall(r"['\"]([^'\"]*)['\"]", args)
        targets.append("pinvou3-app/src-tauri/" + "/".join(parts) + "/**")
    return targets


def _extract_consent_marker_read_targets(consent_test):
    """从 consent_marker_frontend.test.mjs 源码派生全部 src-tauri 读取目标。

    该测试的读取形态与 multiagent_plan_normalize 不同:`join(rustDir,
    '<rel>.rs')`(rustDir = src-tauri/src/features)与
    `join(root, 'src-tauri/<rel>.rs')`
    (root = pinvou3-app)均为单文件读取。统一按「以 .rs 结尾的
    字符串字面量即一个 src-tauri 源读取」归一化——未来新增读取自动
    纳入路由锁,不会 fail-open。
    """
    targets = set()
    for literal in re.findall(r"['\"]([^'\"]*\.rs)['\"]", consent_test):
        if literal.startswith("src-tauri/"):
            # join(root, 'src-tauri/<rel>.rs'):root 即 pinvou3-app/。
            targets.add("pinvou3-app/" + literal)
        else:
            targets.add("pinvou3-app/src-tauri/src/features/" + literal)
    return sorted(targets)


def _extract_install_contract_script_reads(contract_test):
    """从 connector_online_install_contract.test.js 源码派生 scripts/ 读取目标。

    `read("scripts", "x.sh")` 与任意段数的 `read("scripts", "a", "b", …)` 字面量
    (单双引号、跨行皆可)各成一个 pinvou3-app/scripts/ 读取目标;`read("scripts", …)`
    调用里解析不出字符串字面量即红——与 _extract_contract_read_targets 同款
    fail-closed:提取器看不见的读取,路由锁就拦不住删除路由的回归。
    """
    targets = []
    for args in re.findall(
        r"read\(\s*['\"]scripts['\"]\s*,?\s*([^)]*)\)", contract_test, re.S
    ):
        parts = re.findall(r"['\"]([^'\"]*)['\"]", args)
        if not parts:
            raise AssertionError(f"无法解析的 scripts 读取目标: {args}")
        targets.append("pinvou3-app/scripts/" + "/".join(parts))
    # 非字面量首参 fail-closed:变量/模板串/表达式首参的 read(…) 主正则看不见,
    # 派生结果却因其他字面量读取仍非空——新增读取静默逃出路由锁(经变异验证
    # fail-open)。枚举每个 read( 调用,首参必须是完整的引号字面量(整个表达式,
    # 不只是开头引号):quote 打头的拼接首参(如 "scr"+"ipts")骗得过开头引号
    # 检查,主正则却照旧看不见它,读取静默逃出路由锁(评审指出的残余 fail-open;
    # 与下方其余参数的 fullmatch 同口径)。宁可误红也不 fail-open;注释里提及
    # read( 形态时同样须保持字面量示例形态。
    for call_head in re.findall(
        r"(?<![A-Za-z0-9_$.])read\s*\(\s*([^,)]*)", contract_test, re.S
    ):
        first = call_head.strip()
        if not re.fullmatch(r"'[^']*'|\"[^\"]*\"", first):
            raise AssertionError(
                "read() 首参必须是完整的字符串字面量(开头引号检查放得过拼接/"
                "插值首参,读取会静默逃出路由锁,改为字面量或同步本提取器): "
                f"{first}"
            )
    # scripts 首参调用的其余参数必须各自是完整的引号字面量(整个表达式,不只是
    # 开头引号):拼接/插值/计算后参会被主正则切成错误目标,而派生结果因其他
    # 字面量读取仍非空——`read("scripts", "a" + name + ".sh")` 会静默派生出
    # 不存在的路径,真实读取逃出路由锁(fail-open)。逐参数 fullmatch 完整引号
    # 形态,解析不出即红(宁可误红也不 fail-open;注释里提及 read( 形态时同样
    # 须保持字面量示例形态)。空参数(尾逗号)跳过。
    for rest in re.findall(
        r"read\s*\(\s*['\"]scripts['\"]\s*,\s*([^)]*)\)", contract_test, re.S
    ):
        for arg in rest.split(","):
            arg = arg.strip()
            if arg and not re.fullmatch(r"'[^']*'|\"[^\"]*\"", arg):
                raise AssertionError(
                    "read(\"scripts\", …) 的每个参数都必须是完整的字符串字面量"
                    "(拼接/插值/变量参数会派生出错误目标而静默逃出路由锁,"
                    f"改为字面量或同步本提取器): read(\"scripts\", {rest})"
                )
    if not targets:
        raise AssertionError(
            "connector_online_install_contract.test.js 未派生出任何 scripts/ 读取目标"
            "——读取形态变更须同步本提取器"
        )
    return sorted(set(targets))


def _changes_block(workflow_text):
    """pr-check.yml 的 changes 块(去注释),各 filter 组提取的公共上游。"""
    return _without_yaml_comments(
        workflow_text.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
    )


def _frontend_filter_entries(workflow_text):
    """从 pr-check.yml 的 changes 块提取 frontend filter 的路径条目列表。"""
    changes = _changes_block(workflow_text)
    return _extract_quoted_paths(
        changes.split("            frontend:", maxsplit=1)[1].split(
            "            relay:", maxsplit=1
        )[0]
    )


def _rust_full_filter_entries(workflow_text):
    """从 pr-check.yml 的 changes 块提取 rust_full filter 的路径条目列表。"""
    changes = _changes_block(workflow_text)
    return _extract_quoted_paths(
        changes.split("            rust_full:", maxsplit=1)[1].split(
            "            cli_rust:", maxsplit=1
        )[0]
    )


def _rust_embedded_telemetry_inputs(source, source_path):
    """Resolve the literal and manifest-concat forms used by Rust schema consumers."""
    targets = set()
    manifest = ROOT / "pinvou3-app/src-tauri"
    for match in re.finditer(
        r'include_(?:str|bytes)!\(\s*'
        r'(?P<manifest>concat!\(\s*env!\(\s*"CARGO_MANIFEST_DIR"\s*\)\s*,\s*)?'
        r'"(?P<path>[^"\r\n]+)"',
        source,
    ):
        literal = match.group("path")
        target = ((manifest / literal.lstrip("/")) if match.group("manifest")
                  else (source_path.parent / literal)).resolve()
        if target.parent == (ROOT / "docs/schemas").resolve() and target.name.startswith("behavior-telemetry-"):
            targets.add(target.relative_to(ROOT).as_posix())
    return targets


def _macos_job(workflow_text):
    return workflow_text.split("\n  macos-rust-check:", maxsplit=1)[1].split(
        "\n  windows-codex-runtime-test:", maxsplit=1
    )[0]


def _matches_paths_filter(path, patterns):
    """Model paths-filter v4 some-with-excludes routing for policy examples."""
    included = any(
        fnmatchcase(path, pattern)
        for pattern in patterns
        if not pattern.startswith("!")
    )
    excluded = any(
        fnmatchcase(path, pattern[1:])
        for pattern in patterns
        if pattern.startswith("!")
    )
    return included and not excluded


def _evaluate_github_condition(
    expression,
    *,
    event_name,
    draft,
    mode,
    rust_full=False,
    rust_code=False,
    cli_rust=False,
    knowledge_rust=False,
    loongarch64_check_enabled="",
    full_label=False,
    trusted_backflow=False,
    action="synchronize",
):
    """Replay the checked-in GitHub expression for a deterministic truth table."""

    replacements = {
        "!cancelled()": "True",
        "github.event_name": repr(event_name),
        "github.event.action": repr(action),
        "github.event.pull_request.draft": repr(draft),
        "needs.changes.outputs.backflow_mode": repr(mode),
        "needs.changes.outputs.rust_full": repr(str(rust_full).lower()),
        "needs.changes.outputs.rust_code": repr(str(rust_code).lower()),
        "needs.changes.outputs.cli_rust": repr(str(cli_rust).lower()),
        "needs.changes.outputs.knowledge_rust": repr(str(knowledge_rust).lower()),
        "vars.PINVOU_LOONGARCH64_CHECK_ENABLED": repr(loongarch64_check_enabled),
        "needs.changes.outputs.trusted_backflow": repr(str(trusted_backflow).lower()),
        "contains(github.event.pull_request.labels.*.name, 'ci:full-rust')": repr(
            full_label
        ),
    }
    evaluated = expression
    for source, value in replacements.items():
        evaluated = evaluated.replace(source, value)
    evaluated = evaluated.replace("&&", " and ").replace("||", " or ")
    evaluated = re.sub(r"\btrue\b", "True", evaluated, flags=re.IGNORECASE)
    evaluated = re.sub(r"\bfalse\b", "False", evaluated, flags=re.IGNORECASE)
    evaluated = " ".join(evaluated.split())
    return bool(eval(evaluated, {"__builtins__": {}}, {}))


def _changes_block(text):
    """changes job 的完整块(含注释);按路径路由的 pin 在其上继续切片。"""
    return _extract_job_block(text, "changes")


def _frontend_filter_paths(changes):
    """frontend filter 组的条目切片(frontend: 键到 relay: 键之间)。"""
    return changes.split("            frontend:", maxsplit=1)[1].split(
        "            relay:", maxsplit=1
    )[0]


def _dorny_filter_group(changes, group):
    """按 12 空格缩进锚定 dorny filter 组,只取该组的条目行。"""
    return re.search(
        rf"\n            {group}:\n((?:              .*(?:\n|$))+)", changes
    ).group(1)


JOB_HEADER = re.compile(r"^  (\S.*):\s*$")


def _extract_job_block(text, job):
    """提取单个 job 的完整 YAML 块:从它的 2 空格缩进键到下一个 job 键。

    本文件里 job 键只出现在 2 空格缩进,且每个 job 名只允许定义一次。
    """
    lines = text.splitlines()
    starts = [
        number
        for number, line in enumerate(lines)
        if JOB_HEADER.match(line) and JOB_HEADER.match(line).group(1) == job
    ]
    if len(starts) != 1:
        raise AssertionError(
            f"expected exactly one {job} job definition, found {len(starts)}"
        )
    start = starts[0]
    end = next(
        (
            number
            for number in range(start + 1, len(lines))
            if JOB_HEADER.match(lines[number])
        ),
        len(lines),
    )
    return "\n".join(lines[start:end]) + "\n"


def _extract_job_level_if(job_block):
    """提取 job 级 `if:` 的值并按行 strip 归一化:单行 `${{ }}` 或折叠 `>-` 块。

    只认 4 空格缩进的 `    if:`——job 级键;步骤级 `if:` 在 8 空格缩进,
    不会命中。折叠块取其后所有缩进 >= 5 空格的行,遇到空行或下一个 job 级
    键(缩进 <= 4)即止:截断会让调用方的全文比对红掉(fail closed),
    而不是把残缺的值当作完整 pin 静默通过。
    """
    lines = job_block.splitlines()
    starts = [number for number, line in enumerate(lines) if re.match(r"^    if:", line)]
    if len(starts) != 1:
        raise AssertionError(
            "expected exactly one job-level `if:` in the job block, "
            f"found {len(starts)}"
        )
    start = starts[0]
    value = lines[start][len("    if:"):].strip()
    if value != ">-":
        return value
    folded = []
    for line in lines[start + 1:]:
        stripped = line.strip()
        if not stripped:
            break
        if len(line) - len(line.lstrip(" ")) <= 4:
            break
        folded.append(stripped)
    return "\n".join(folded)


def _step_if_value(step_text):
    """Value of a step-level `if:` (8-space key) with whitespace normalized
    to single spaces — a single-line `${{ ... }}` and a folded `>-`/`>`
    block compare equal. Round-23: the deny-step routing pins compared the
    raw single-line text, so a semantics-preserving fold of the condition
    (the shape main adopted for its job-level ifs in #745) false-redded
    them. Returns None when the step carries no `if:`."""
    lines = step_text.splitlines()
    for index, line in enumerate(lines):
        match = re.match(r"^        if:(.*)$", line)
        if match is None:
            continue
        value = match.group(1).strip()
        if value in (">-", ">", "|", "|-"):
            folded = []
            for following in lines[index + 1 :]:
                if not following.strip():
                    break
                if len(following) - len(following.lstrip(" ")) <= 8:
                    break
                folded.append(following.strip())
            value = " ".join(folded)
        return " ".join(value.split())
    return None


_STEP_NAME = re.compile(r"^      - name: (.*)$")


def _steps_of(job_block):
    """Split a job block into ordered (name, step-text) pairs on exact name lines.

    Each step anchors on its own `      - name: ` line rather than slicing at
    the first occurrence of a pinned name: a decoy step whose name merely
    *contains* a pinned name can neither capture a first-occurrence slice
    nor evade the line-exact uniqueness pin — the two pin styles must be
    used as a pair.
    """
    lines = job_block.splitlines()
    indices = [i for i, line in enumerate(lines) if _STEP_NAME.match(line)]
    steps = []
    for pos, i in enumerate(indices):
        end = indices[pos + 1] if pos + 1 < len(indices) else len(lines)
        steps.append((_STEP_NAME.match(lines[i]).group(1), "\n".join(lines[i:end])))
    return steps


def _step_named(job_block, name):
    """Return the one step whose name equals `name`.

    A missing or duplicate name raises (fail closed).
    """
    matches = [text for step_name, text in _steps_of(job_block) if step_name == name]
    if len(matches) != 1:
        raise AssertionError(
            f"expected exactly one step named {name!r}, found {len(matches)}"
        )
    return matches[0]


def _step_if_tokens(step_body):
    """Normalize a step-level `if:` value into a token stream.

    Accepts folded (`if: >-`) and inline (`if: ${{ ... }}`) spellings:
    pinning the folded scalar's layout shape would false-fail a
    semantically identical YAML reflow — exactness lives in the token
    stream, not the line shape. Whitespace/paren normalization also
    applies inside string literals; fine for the pinned literals
    ('push', 'true'), revisit before reusing this on literals containing
    spaces or parens.
    """
    lines = step_body.splitlines()
    idx = next(i for i, line in enumerate(lines) if line.strip().startswith("if:"))
    first = lines[idx].strip()
    head = first[len("if:"):]
    if head.strip() not in (">-", "|-", ">", "|"):
        return " ".join(head.split())
    indent = len(lines[idx]) - len(lines[idx].lstrip(" "))
    chunk = []
    for line in lines[idx + 1:]:
        if not line.strip() or (len(line) - len(line.lstrip(" "))) <= indent:
            break
        chunk.append(line)
    return " ".join(" ".join(chunk).split())


# Round-52 review: 每个 required-gate 消费其结果的 job 的 job 级 `if:` 全文。
# 折叠块(cli-test/cli-lint 等)的 `${{ }}` 末行(或单行形式的行尾)被追加
# `&& false` 时,该腿静默 skip 而 required-gate 的 success|skipped 循环照单
# 全收——下方 assertIn 级别的路由片段 pin 全绿。这里逐 job 断言提取出的
# `if:` 值与钉死文本完全相等:任何追加、删支、改写都红。(commit-message/
# version-consistency/changes 不在表内:它们的 skip 走的是 required-gate 的
# `!= success` 基础检查,skip 即红,不属于本 bypass 类。)
_CLI_LEG_JOB_IF = (
    "${{\n"
    "!cancelled() &&\n"
    "github.event.action != 'closed' &&\n"
    "(\n"
    "github.event_name == 'push' ||\n"
    "(\n"
    "github.event_name == 'merge_group' &&\n"
    "(\n"
    "needs.changes.outputs.cli_rust == 'true' ||\n"
    "needs.changes.outputs.rust_full == 'true'\n"
    ")\n"
    ") ||\n"
    "(\n"
    "github.event_name == 'pull_request' &&\n"
    "github.event.pull_request.draft == false &&\n"
    "(\n"
    "needs.changes.outputs.cli_rust == 'true' ||\n"
    "needs.changes.outputs.rust_full == 'true'\n"
    ")\n"
    ")\n"
    ")\n"
    "}}"
)
# rust-loongarch64-check 的路由与 cli-test/macos-cli-check 同形(push 无条件、
# merge_group/ready PR 按 rust_full/cli_rust/knowledge_rust),差异只在输出
# 集合:该 leg 交叉检查三个 workspace,所以知识库变更也触发它。
_LOONGARCH64_JOB_IF = (
    "${{\n"
    "vars.PINVOU_LOONGARCH64_CHECK_ENABLED != 'false' &&\n"
    "!cancelled() &&\n"
    "github.event.action != 'closed' &&\n"
    "(\n"
    "github.event_name == 'push' ||\n"
    "(\n"
    "github.event_name == 'merge_group' &&\n"
    "(\n"
    "needs.changes.outputs.rust_full == 'true' ||\n"
    "needs.changes.outputs.cli_rust == 'true' ||\n"
    "needs.changes.outputs.knowledge_rust == 'true'\n"
    ")\n"
    ") ||\n"
    "(\n"
    "github.event_name == 'pull_request' &&\n"
    "github.event.pull_request.draft == false &&\n"
    "(\n"
    "needs.changes.outputs.rust_full == 'true' ||\n"
    "needs.changes.outputs.cli_rust == 'true' ||\n"
    "needs.changes.outputs.knowledge_rust == 'true'\n"
    ")\n"
    ")\n"
    ")\n"
    "}}"
)
_RUST_FULL_JOB_IF = (
    "${{\n"
    "!cancelled() &&\n"
    "(\n"
    "github.event_name == 'push' ||\n"
    "(\n"
    "github.event_name == 'pull_request' &&\n"
    "github.event.pull_request.draft == false &&\n"
    "(\n"
    "needs.changes.outputs.rust_full == 'true' ||\n"
    "needs.changes.outputs.cli_rust == 'true' ||\n"
    "(\n"
    "needs.changes.outputs.rust_code == 'true' &&\n"
    "contains(github.event.pull_request.labels.*.name, 'ci:full-rust')\n"
    ")\n"
    ")\n"
    ")\n"
    ")\n"
    "}}"
)
# Official 的 windows-rust-test 在上述结构上叠加可信回流分级:push 无条件
# 累计;Ready PR 按 backflow_mode 分级(full/windows_full 直接跑,ordinary
# 回到 rust_full/cli_rust/rust_code+ci:full-rust);Merge Queue 仅在
# mode=full 时补跑。逐字 pin(经 _extract_job_level_if 行归一化)。
_WINDOWS_RUST_JOB_IF = (
    "${{\n"
    "!cancelled() &&\n"
    "(\n"
    "github.event_name == 'push' ||\n"
    "(\n"
    "github.event_name == 'pull_request' &&\n"
    "github.event.pull_request.draft == false &&\n"
    "(\n"
    "needs.changes.outputs.backflow_mode == 'full' ||\n"
    "needs.changes.outputs.backflow_mode == 'windows_full' ||\n"
    "(\n"
    "needs.changes.outputs.backflow_mode == 'ordinary' &&\n"
    "(\n"
    "needs.changes.outputs.rust_full == 'true' ||\n"
    "needs.changes.outputs.cli_rust == 'true' ||\n"
    "(\n"
    "needs.changes.outputs.rust_code == 'true' &&\n"
    "contains(github.event.pull_request.labels.*.name, 'ci:full-rust')\n"
    ")\n"
    ")\n"
    ")\n"
    ")\n"
    ") ||\n"
    "(\n"
    "github.event_name == 'merge_group' &&\n"
    "needs.changes.outputs.backflow_mode == 'full'\n"
    ")\n"
    ")\n"
    "}}"
)
GATE_JOB_LEVEL_IFS = {
    "fast-gate": (
        "${{ (github.event_name == 'pull_request' && "
        "github.event.action != 'closed') || "
        "github.event_name == 'merge_group' }}"
    ),
    "frontend-test": (
        "${{ !cancelled() && github.event.action != 'closed' && "
        "needs.changes.outputs.pure_release_backflow != 'true' && "
        "(github.event_name == 'pull_request' || "
        "github.event_name == 'merge_group') && "
        "(needs.changes.outputs.frontend == 'true' || "
        "needs.changes.outputs.pet == 'true') && !(github.event_name == 'merge_group' && needs.changes.outputs.reuse_verified == 'true' && contains(fromJSON(needs.changes.outputs.reused_jobs || '[]'), 'frontend-test')) }}"
    ),
    "relay-test": (
        "${{ !cancelled() && needs.changes.outputs.relay == 'true' && "
        "(github.event_name == 'merge_group' || github.event_name == 'pull_request') }}"
    ),
    "release-contract-test": (
        "${{ !cancelled() && github.event.action != 'closed' && "
        "needs.changes.outputs.release_contract == 'true' && "
        "(github.event_name != 'pull_request' || "
        "github.event.pull_request.draft == false) }}"
    ),
    # knowledge-rust/cli-test share the push-unconditional shape since
    # 2026-10: main push refreshes the cache namespace even on
    # non-knowledge merges (PRs never save; a paths-gated writer ages out).
    # Drafts skip like every other heavy leg.
    "knowledge-rust": (
        # knowledge-rust gains the push-unconditional arm on the Official tree's
        # existing guard (pure_release_backflow): main push refreshes the
        # knowledge-rust-v2 namespace even on non-knowledge merges (PRs never
        # save; a paths-gated writer ages out). PR/Queue routing stays
        # paths-driven and Official's existing draft behavior is unchanged.
        "${{\n"
        "!cancelled() &&\n"
        "github.event.action != 'closed' &&\n"
        "needs.changes.outputs.pure_release_backflow != 'true' &&\n"
        "(\n"
        "github.event_name == 'push' ||\n"
        "needs.changes.outputs.knowledge_rust == 'true' ||\n"
        "needs.changes.outputs.knowledge_dependencies == 'true'\n"
        ")\n"
        "}}"
    ),
    # rust-lint got the same main-push refresh, but keeps its draft
    # fmt-only fast pass: no draft clause here (RUN_HEAVY_RUST_CHECKS
    # degrades drafts instead).
    "rust-lint": (
        # rust-lint gains the same main-push refresh while keeping Official's
        # guard; the draft fmt + bounded-regression fast pass is unchanged
        # (RUN_HEAVY_RUST_CHECKS degrades drafts, no draft clause here).
        "${{\n"
        "!cancelled() &&\n"
        "github.event.action != 'closed' &&\n"
        "needs.changes.outputs.pure_release_backflow != 'true' &&\n"
        "(\n"
        "github.event_name == 'push' ||\n"
        "needs.changes.outputs.rust_code == 'true' ||\n"
        "needs.changes.outputs.rust_dependencies == 'true'\n"
        ")\n"
        "}}"
    ),
    "rust-test": (
        "${{\n"
        "!cancelled() &&\n"
        "github.event.action != 'closed' &&\n"
        "(\n"
        "github.event_name == 'push' ||\n"
        "(\n"
        "needs.changes.outputs.trusted_backflow != 'true' &&\n"
        "(\n"
        "(\n"
        "github.event_name == 'merge_group' &&\n"
        "needs.changes.outputs.rust_full == 'true'\n"
        ") ||\n"
        "(\n"
        "github.event_name == 'pull_request' &&\n"
        "github.event.pull_request.draft == false &&\n"
        "(\n"
        "needs.changes.outputs.rust_full == 'true' ||\n"
        "(\n"
        "needs.changes.outputs.rust_code == 'true' &&\n"
        "contains(github.event.pull_request.labels.*.name, 'ci:full-rust')\n"
        ")\n"
        ")\n"
        ")\n"
        ")\n"
        ")\n"
        ")\n"
        "}}"
    ),
    "cli-test": _CLI_LEG_JOB_IF,
    "cli-lint": _CLI_LEG_JOB_IF,
    "windows-rust-test": _WINDOWS_RUST_JOB_IF,
    "macos-rust-check": _RUST_FULL_JOB_IF,
    "macos-codex-runtime-test": (
        "${{ !cancelled() && github.event.action != 'closed' && "
        "needs.changes.outputs.pure_release_backflow != 'true' && "
        "needs.changes.outputs.acp_runtime == 'true' && "
        "(github.event_name != 'pull_request' || "
        "github.event.pull_request.draft == false) }}"
    ),
    "macos-cli-check": _CLI_LEG_JOB_IF,
    "rust-loongarch64-check": _LOONGARCH64_JOB_IF,
    "windows-codex-runtime-test": (
        "${{ !cancelled() && github.event.action != 'closed' && "
        "needs.changes.outputs.pure_release_backflow != 'true' && "
        "(needs.changes.outputs.windows_codex == 'true' || "
        "needs.changes.outputs.acp_runtime == 'true') && "
        "(github.event_name != 'pull_request' || "
        "github.event.pull_request.draft == false) }}"
    ),
    "windows-rustup-repair-test": (
        "${{ !cancelled() && github.event.action != 'closed' && "
        "needs.changes.outputs.pure_release_backflow != 'true' && "
        "needs.changes.outputs.windows_rustup_repair == 'true' && "
        "(github.event_name != 'pull_request' || "
        "github.event.pull_request.draft == false) }}"
    ),
}

# Round-52 review: 套件以 name+`if` assertIn 或 split 首个匹配钉住这些步骤,
# 同名的诱饵副本会满足 pin 而真步骤的路由被关掉(经变异验证的 bypass 类)。
# 每个被钉的步骤名必须在其 job 块内恰好出现一次。
# windows-rust-test 不在表内:它的步骤名唯一性已由
# test_windows_rust_test_parallel_phases_preserve_routing_and_coverage 的
# round-45 duplicate-step 断言覆盖。
# Round-6 review (#1518): 本 PR 新钉的三枚步骤(锁文件 registry 卫生门禁/
# test-registry guard/Prepare Rust disk capacity)同样按 name+if 钉住,
# 一并登记;rust-lint 的磁盘与 zram 步骤为本 PR 新增,同表钉住防诱饵替换。
PINNED_GATE_STEP_NAMES = {
    "fast-gate": (
        "Checkout (含 submodule,fork 为 public 直接拉)",
        "workflow lint (actionlint)",
        "锁文件 registry 卫生门禁",
    ),
    "cli-test": (
        "Install Tauri Linux system dependencies",
        "Set up zram and swap",
        "cargo test (all targets, no-fail-fast)",
        "cargo compile check (push)",
        "cargo test (adapter-gaia test-support)",
        "cargo compile check (push, adapter-gaia test-support)",
    ),
    "cli-lint": (
        "Prepare CLI disk capacity before swap",
        "Check CLI disk capacity before cache",
        "Check CLI disk capacity after cache",
        "Install Tauri Linux system dependencies",
        "Guard CLI cache post-save",
    ),
    "rust-test": (
        "cargo test --lib --no-run（编译链接测试二进制）",
        "cargo test --lib（含 strict_mode 回归；真 bge-m3/vLLM 测试已 #[ignore]）",
        "cargo test --lib --no-run (push main 仅编译暖 cache)",
        "test-registry guard (duplicates + required pins)",
    ),
    "rust-lint": (
        "Prepare Rust disk capacity",
        "Set up zram and swap",
        "cargo-deny (advisories/licenses/bans/sources)",
        "cargo deny check (hard gate)",
    ),
    # PR #1518 round-11: 复活的 test:windows-runtime-manifest 契约步是全仓
    # 唯一执行器,同样按 name 钉住防诱饵/防静默删除。
    "windows-codex-runtime-test": (
        "Windows runtime manifest 契约",
    ),
    "knowledge-rust": (
        "cargo clippy (hard gate, lib/bins/tests + server)",
        "cargo-deny",
        "cargo deny check",
    ),
    "rust-loongarch64-check": (
        "Set up the pinned loong64 cross toolchain (sha256-pinned asset)",
        "Set up the deepin glibc-2.38 loong64 sysroot",
        "cargo check src-tauri (loongarch64 cross)",
        "cargo check pinvou-knowledge (loongarch64 cross, incl. tests)",
        "cargo check pinvou-cli (loongarch64 cross)",
    ),
    "macos-rust-check": (
        "macOS Rust all-targets check",
        "macOS computer_use unit tests",
        "macOS full lib tests (native regression, push only)",
        "Install both targets (universal bundle smoke, push only)",
        "Node.js (universal bundle smoke, push only)",
        "Install frontend deps (universal bundle smoke, push only)",
        "Cargo build (release-fast smoke, push only)",
        "Tauri bundle smoke (verify dmg/app packaging, bundle-chain changes only)",
        "Prune release artifacts before the cache save (push only)",
        "Verify script (plist/usage-key/bundle targets checks, push only)",
        "Probe and export the macOS lld link flags",
    ),
}


class CiGatePolicyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = default_ci_routing(PR_WORKFLOW.read_text(encoding="utf-8"))
        cls.pr_workflow = cls.workflow
        cls.release_workflow = default_ci_routing(RELEASE_WORKFLOW.read_text(encoding="utf-8"))

    def test_dynamic_directory_globs_do_not_hide_dead_literal_directory_entries(self):
        original = self.pr_workflow
        # The real release group contains dynamic remote-control dir globs.
        # They must stay reachable, while a misspelled literal dir stays red.
        self.test_cli_crate_has_its_own_required_gate()
        try:
            self.pr_workflow = original.replace(
                "            release_contract:\n",
                "            release_contract:\n"
                "              - 'scripts/pinvou-nonexistent-guard-fixture/**'\n",
                1,
            )
            self.assertNotEqual(self.pr_workflow, original)
            with self.assertRaisesRegex(AssertionError, "pinvou-nonexistent-guard-fixture"):
                self.test_cli_crate_has_its_own_required_gate()
        finally:
            self.pr_workflow = original

    def test_windows_relay_tools_run_on_native_windows_for_draft_ready_and_queue(self):
        job = self.workflow.split("\n  windows-relay-tools-test:", 1)[1].split("\n  team-relay-hub-test:", 1)[0]
        self.assertIn("runs-on: windows-latest", job)
        self.assertIn("needs: changes", job)
        self.assertIn("node scripts/run-windows-relay-tools-tests.mjs", job)
        self.assertIn('node-version: "24"', job)
        self.assertIn("timeout-minutes: 10", job)
        expression = re.search(r"if: \$\{\{ (.*?) \}\}", job).group(1)
        for event in ("pull_request", "merge_group", "push"):
            for draft in (True, False):
                for changed in (True, False):
                    for action in ("synchronize", "closed"):
                        condition = expression.replace("!cancelled()", "True")
                        condition = condition.replace("github.event.action", repr(action))
                        condition = condition.replace("github.event_name", repr(event))
                        condition = condition.replace("github.event.pull_request.draft", repr(draft))
                        condition = condition.replace("needs.changes.outputs.relay", repr(str(changed).lower()))
                        condition = condition.replace("&&", " and ").replace("||", " or ")
                        self.assertEqual(bool(eval(condition, {"__builtins__": {}}, {})),
                                         changed and action != "closed" and event in ("pull_request", "merge_group"))
        filters = re.search(r"\n            relay:\n((?:              .*\n)+)", self.workflow).group(1)
        paths = _extract_quoted_paths(filters)
        for path in ("remote-control-relay/test/local-selftest-ownership.test.js",
                     "remote-control-relay/test/local-selftest-identity.test.js",
                     "remote-control-relay/test/endpoint-launcher.test.js",
                     "remote-control-relay/scripts/check-local-signing-key.cjs",
                     "scripts/remote-control-local-selftest.ps1", "scripts/run-remote-control-endpoint.ps1",
                     "scripts/run-windows-relay-tools-tests.mjs", "scripts/windows-relay-tools-test-summary.mjs",
                     "scripts/windows-relay-tools-preflight.mjs",
                     ".github/workflows/pr-check.yml"):
            self.assertTrue(_matches_paths_filter(path, paths), path)

    def test_required_gate_rejects_skipped_or_failed_applicable_relay_legs(self):
        gate = self.workflow.split("\n  required-gate:", 1)[1]
        self.assertIn("- windows-relay-tools-test", gate)
        self.assertIn("WINDOWS_RELAY_RESULT: ${{ needs.windows-relay-tools-test.result }}", gate)
        self.assertIn("RELAY_REQUIRED_BY_POLICY: ${{ needs.changes.outputs.relay == 'true' }}", gate)
        self.assertIn('"windows-relay-tools-test:$WINDOWS_RELAY_RESULT"', gate)
        body = _step_named(gate, "汇总所有适用检查").split("        run: |\n", 1)[1]
        shell = "\n".join(line[10:] for line in body.splitlines())
        environment = {**os.environ, "GITHUB_EVENT_NAME": "pull_request", "BACKFLOW_MODE": "full",
                       "WINDOWS_RUST_REQUIRED_BY_POLICY": "false"}
        # [A-Z0-9_]: leg 名可含数字(rust-loongarch64-check → RUST_LOONGARCH64_CHECK_RESULT),纯字母类会漏抓、门禁模拟随之漏喂。
        for name in re.findall(r"^          ([A-Z0-9_]+_RESULT):", gate, re.MULTILINE):
            environment[name] = "success"
        for required in ("true", "false"):
            for linux in ("success", "skipped", "failure", "cancelled", ""):
                for windows in ("success", "skipped", "failure", "cancelled", ""):
                    result = subprocess.run(["bash", "-c", shell], capture_output=True, timeout=10,
                        env={**environment, "RELAY_REQUIRED_BY_POLICY": required,
                             "RELAY_RESULT": linux, "WINDOWS_RELAY_RESULT": windows})
                    accepted = (linux == windows == "success" if required == "true"
                                else linux in ("success", "skipped") and windows in ("success", "skipped"))
                    self.assertEqual(result.returncode == 0, accepted, (required, linux, windows, result.stderr))

    def test_required_gate_requires_actual_success_for_applicable_windows_dacl(self):
        gate = self.workflow.split("\n  required-gate:", 1)[1]
        body = _step_named(gate, "汇总所有适用检查").split("        run: |\n", 1)[1]
        shell = "\n".join(line[10:] for line in body.splitlines())
        environment = {**os.environ, "BACKFLOW_MODE": "full",
                       "WINDOWS_RUST_REQUIRED_BY_POLICY": "false"}
        # [A-Z0-9_]: leg 名可含数字(rust-loongarch64-check → RUST_LOONGARCH64_CHECK_RESULT),纯字母类会漏抓、门禁模拟随之漏喂。
        for name in re.findall(r"^          ([A-Z0-9_]+_RESULT):", gate, re.MULTILINE):
            environment[name] = "success"
        for event in ("pull_request", "merge_group"):
            for required in ("true", "false"):
                for status in ("success", "skipped", "failure", "cancelled", ""):
                    result = subprocess.run(["bash", "-c", shell], capture_output=True, timeout=10,
                        env={**environment, "GITHUB_EVENT_NAME": event,
                             "RELAY_REQUIRED_BY_POLICY": required,
                             "WINDOWS_RELAY_DACL_RESULT": status})
                    accepted = (status == "success" if required == "true"
                                else status in ("success", "skipped"))
                    self.assertEqual(result.returncode == 0, accepted,
                                     (event, required, status, result.stderr))

    def test_release_workflow_does_not_reference_retired_web_template(self):
        for retired_reference in (
            "test:web-template-packaging",
            "prepare:web-template",
            "resources/common/web-template",
            "网页模板发布前冒烟",
        ):
            self.assertNotIn(
                retired_reference,
                self.release_workflow,
                f"发布流程仍引用已退役网页模板: {retired_reference}",
            )

    def test_pull_request_has_lightweight_release_contract_gate(self):
        self.assertIn("release_contract:", self.pr_workflow)
        self.assertIn("  release-contract-test:", self.pr_workflow)
        self.assertIn(
            "needs.changes.outputs.release_contract == 'true'",
            self.pr_workflow,
        )
        required_gate = self._required_gate()
        self.assertIn("- release-contract-test", required_gate)
        self.assertIn(
            '"release-contract-test:$RELEASE_CONTRACT_RESULT"',
            required_gate,
        )

    def test_pr_verifier_checks_registered_history_and_keeps_release_closure(self):
        verifier = PUBLIC_SUBMODULE_VERIFIER.read_text(encoding="utf-8")
        verifier_gate = self.pr_workflow.split(
            "- name: 公开底座 gitlink 可达性", maxsplit=1
        )[1].split("- name: 初始化公共底座 submodule", maxsplit=1)[0]
        self.assertRegex(verifier_gate, r"(?m)^\s+run: \./scripts/verify-public-submodule\.sh\s*$")
        self.assertNotIn("--registered-snapshot", verifier_gate)
        self.assertNotIn("registered_snapshot", verifier)
        self.assertNotIn("--allow-registered-candidate", verifier_gate)
        self.assertNotIn("LOCAL_SECURITY_HEAD", verifier)
        self.assertIn('[[ "$tag_target" != "$gitlink" ]]', verifier)
        self.assertIn('PINVOU_CODEWHALE_BRANCH="pinvou3-clean"', verifier)
        self.assertIn('PINVOU_CODEWHALE_TAG="pinvou-v0.9.12-r3"', verifier)
        self.assertIn('[[ "$branch_target" != "$gitlink" ]]', verifier)
        self.assertIn('[[ "$branch_target" != "$tag_target" ]]', verifier)
        self.assertIn("unknown argument", verifier)

    def test_pr_modes_and_stacked_pr_triggers_are_explicit(self):
        trigger = self.workflow.split("\non:", maxsplit=1)[1].split(
            "\npermissions:", maxsplit=1
        )[0]
        pull_request_trigger = trigger.split("\n  pull_request:", maxsplit=1)[
            1
        ].split("\n  merge_group:", maxsplit=1)[0]
        active_trigger = "\n".join(
            line
            for line in pull_request_trigger.splitlines()
            if not line.lstrip().startswith("#")
        )
        self.assertNotIn("branches:", active_trigger)
        self.assertIn("ready_for_review", pull_request_trigger)
        self.assertIn("converted_to_draft", pull_request_trigger)

        frontend = self.workflow.split("\n  frontend-test:", maxsplit=1)[1].split(
            "\n  relay-test:", maxsplit=1
        )[0]
        self.assertIn("github.event.pull_request.draft == false", frontend)
        self.assertIn("Ready PR 定向浏览器 smoke", frontend)
        self.assertIn("Merge Queue diff-selected browser smoke", frontend)
        self.assertIn("github.event.merge_group.base_sha", frontend)
        self.assertIn("github.event.merge_group.head_sha", frontend)
        self.assertEqual(frontend.count("select-frontend-smokes.mjs"), 2)
        self.assertNotIn("npm run test:browser-smoke", frontend)
        self.assertEqual(frontend.count("npm run test:markdown"), 0)

    def test_static_analysis_gate_configs_route_to_frontend_test(self):
        # The static-analysis gates (oxlint/Biome/knip/jsconfig/audit-compat)
        # only run inside frontend-test, so their config files must be in the
        # frontend path filter; otherwise a config-only PR skips every gate
        # that consumes the file it changed.
        changes = _without_yaml_comments(_changes_block(self.pr_workflow))
        frontend_paths = _frontend_filter_paths(changes)
        for path in (
            "pinvou3-app/.oxlintrc.json",
            "pinvou3-app/biome.jsonc",
            "pinvou3-app/knip.json",
            "pinvou3-app/jsconfig.json",
            "pinvou3-app/eslint.config.mjs",
            "pinvou3-app/scripts/audit-compat.mjs",
        ):
            self.assertIn(
                path,
                frontend_paths,
                f"静态门禁配置 {path} 不在 frontend filter 中,config-only PR 会静默跳过 frontend-test",
            )

    def test_webui_wrapper_routes_to_frontend_test_with_suite_wired(self):
        # run-webui-smoke.mjs is consumed only inside frontend-test (the
        # diff-selected test:webui entry), and its behavior suite
        # scripts/tests/run-webui-smoke.test.mjs is executed by
        # version-consistency from an explicit file list. Miss either
        # registration and a wrapper-only PR loses every gate over the file
        # it changed (round-2 review of PR #745 found both holes: the
        # selector branch existed but the filter never routed the path, and
        # the suite only ever ran locally).
        changes = _without_yaml_comments(_changes_block(self.pr_workflow))
        # Extracted entries, not raw substrings: a trailing-comment decoy
        # ("- 'other' # was 'run-webui-smoke.mjs'") must not satisfy this
        # pin (round-42 extractor discipline; unrecognized entry shapes
        # fail closed). Round-3 review of PR #745.
        frontend_paths = _extract_quoted_paths(_frontend_filter_paths(changes))
        self.assertIn(
            "pinvou3-app/scripts/run-webui-smoke.mjs",
            frontend_paths,
            "webui wrapper 不在 frontend filter 中,wrapper-only PR 会静默跳过 frontend-test",
        )
        # Inline-stripped (round-4 review R3 of PR #745): whole-line
        # stripping alone lets a suite that was removed from the live list
        # survive as a trailing comment on another list line, keeping this
        # membership pin green while version-consistency never runs it.
        version_consistency = _without_inline_comments(
            self.pr_workflow.split(
                "\n  version-consistency:", maxsplit=1
            )[1].split("\n  changes:", maxsplit=1)[0]
        )
        self.assertIn(
            "scripts/tests/run-webui-smoke.test.mjs",
            version_consistency,
            "wrapper 套件未接入 version-consistency 的显式 node --test 列表,CI 永不执行",
        )

    def test_node_suites_under_scripts_tests_are_all_wired(self):
        # Nothing auto-discovers scripts/tests/*.test.mjs. Most suites use
        # version-consistency; the Account contracts also have dedicated
        # jobs with their own runtime dependencies and path routing.
        # Pin their live commands and filters before including those suites.
        # Pin both directions so the class stays closed:
        # every suite on disk must be listed (a forgotten new suite
        # silently never runs), and every listed scripts/tests entry must
        # exist (a renamed suite must not leave a dead entry green).
        # Inline-stripped (round-4 review R3 of PR #745): a trailing
        # comment on a live list line must not count a removed suite as
        # "listed" — whole-line stripping alone left that hole open.
        version_consistency = _without_inline_comments(
            self.pr_workflow.split("\n  version-consistency:", maxsplit=1)[1].split(
                "\n  changes:", maxsplit=1
            )[0]
        )
        listed = set(
            re.findall(
                r"scripts/tests/[A-Za-z0-9_./-]+\.test\.mjs", version_consistency
            )
        )
        dedicated = {
            "token-management-authorization-coverage.test.mjs": (
                "account-platform-test", "account_platform",
                "Verify Token management authorization contracts",
            ),
            "token-management-phase-coverage.test.mjs": (
                "account-platform-test", "account_platform",
                "Verify Token management authorization contracts",
            ),
            "account-platform-authorization-coverage.test.mjs": (
                "account-platform-test", "account_platform",
                "Verify Account Platform authorization contracts",
            ),
            "f18-hold-recovery-owned-fixture.test.mjs": (
                "account-platform-test", "account_platform",
                "Verify F18 owned fixture contract",
            ),
            "account-platform-invocation-environment.test.mjs": (
                "frontend-test", "frontend",
                "Verify Account invocation environment contracts",
            ),
        }
        changes = _changes_block(self.pr_workflow)
        for filename, (job_name, group, step_name) in dedicated.items():
            path = f"scripts/tests/{filename}"
            job = _without_inline_comments(_extract_job_block(self.pr_workflow, job_name))
            step = _step_named(job, step_name)
            command_path = f"../{path}" if job_name == "frontend-test" else path
            self.assertRegex(
                step, rf"(?m)^\s+(?:run:\s*)?node --test {re.escape(command_path)}\s*$",
                f"{path} must have a live node --test command in {job_name}",
            )
            self.assertNotRegex(step, r"(?m)^        (?:if|continue-on-error):")
            entries = _extract_quoted_paths(_dorny_filter_group(changes, group))
            self.assertIn(path, entries, f"{path} must trigger its {job_name} runner")
            self.assertTrue(_matches_paths_filter(path, entries), f"{path} must not be excluded")
            self.assertIn('node-version: "24"', job)
            if job_name == "frontend-test":
                self.assertIn("working-directory: pinvou3-app", job)
                install = _step_named(job, "安装前端依赖")
                self.assertIn("run: npm ci --prefer-offline --no-audit", install)
                self.assertNotRegex(install, r"(?m)^        (?:if|continue-on-error):")
                self.assertLess(job.index(install), job.index(step))
                for name in ("personal", "models", "invocation"):
                    source = f"account-platform/deploy/production/compose.{name}.yaml"
                    self.assertIn(source, entries)

                    self.assertTrue(_matches_paths_filter(source, entries))
            self.assertNotIn(path, listed, f"{path} must not be redundantly run twice")
            listed.add(path)
        on_disk = {
            f"scripts/tests/{path.name}"
            for path in (ROOT / "scripts" / "tests").glob("*.test.mjs")
        }
        self.assertEqual(
            listed,
            on_disk,
            "scripts/tests node 套件与已核验的 CI runner 集合不一致"
            "(缺失=永不执行;多余=死条目)",
        )

    def test_account_closure_ci_steps_are_precisely_pinned(self):
        """Keep the closure source and isolated-PG gates live and fail-closed."""
        job = _without_inline_comments(_extract_job_block(self.pr_workflow, "account-platform-test"))

        def assert_contract(text):
            source = _step_named(text, "Verify Account closure source and API contracts")
            self.assertIn("npm --prefix account-platform run test:closure-contracts", source)
            self.assertNotRegex(source, r"(?m)^        (?:if|continue-on-error):")
            postgres = _step_named(text, "Account closure contracts on isolated PostgreSQL")
            self.assertIn("python3 -m unittest discover -s scripts/tests -p test_account_closure_postgres.py", postgres)
            self.assertIn("python3 scripts/test-account-closure-postgres.py", postgres)
            self.assertIn("PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL", postgres)
            self.assertNotRegex(postgres, r"(?m)^        (?:if|continue-on-error):")

        assert_contract(job)
        source = _step_named(job, "Verify Account closure source and API contracts")
        postgres = _step_named(job, "Account closure contracts on isolated PostgreSQL")
        for mutated in (
            job.replace(source, "", 1),
            job.replace(postgres, "", 1),
            job.replace("npm --prefix account-platform run test:closure-contracts", "true", 1),
            job.replace("python3 scripts/test-account-closure-postgres.py", "true", 1),
        ):
            with self.assertRaises(AssertionError):
                assert_contract(mutated)

    def test_deployment_contract_artifacts_are_attempt_scoped(self):
        job = _without_inline_comments(
            _extract_job_block(self.pr_workflow, "account-platform-test")
        )
        step = _step_named(
            job, "Production history and managed PostgreSQL release contracts"
        )
        self.assertRegex(
            step,
            r'--artifacts "\$RUNNER_TEMP/account-platform-deployment-contracts-\$postgres_version-\$\{GITHUB_RUN_ID\}-\$\{GITHUB_RUN_ATTEMPT\}"',
        )
        self.assertNotIn(
            '--artifacts "$RUNNER_TEMP/account-platform-deployment-contracts-$postgres_version"',
            step,
        )

    def test_dedicated_node_suite_wiring_rejects_missing_or_commented_commands(self):
        original = self.pr_workflow
        try:
            for name in (
                "token-management-authorization-coverage",
                "token-management-phase-coverage",
                "account-platform-authorization-coverage",
                "account-platform-invocation-environment",
            ):
                path = f"scripts/tests/{name}.test.mjs"
                command = f"node --test {'../' if 'invocation' in name else ''}{path}"
                for replacement in ("echo controlled-command-removed", "# " + command):
                    with self.subTest(suite=name, replacement=replacement):
                        self.assertEqual(original.count(command), 1)
                        self.pr_workflow = original.replace(command, replacement, 1)
                        with self.assertRaises(AssertionError):
                            self.test_node_suites_under_scripts_tests_are_all_wired()
                with self.subTest(suite=name, missing_filter=True):
                    entry = f"              - '{path}'\n"
                    self.assertEqual(original.count(entry), 1)
                    self.pr_workflow = original.replace(entry, "", 1)
                    with self.assertRaises(AssertionError):
                        self.test_node_suites_under_scripts_tests_are_all_wired()
            for name in ("personal", "models", "invocation"):
                with self.subTest(missing_compose_filter=name):
                    entry = f"              - 'account-platform/deploy/production/compose.{name}.yaml'\n"
                    self.assertEqual(original.count(entry), 1)
                    self.pr_workflow = original.replace(entry, "", 1)
                    with self.assertRaises(AssertionError):
                        self.test_node_suites_under_scripts_tests_are_all_wired()
        finally:
            self.pr_workflow = original

    def test_approval_center_browser_regression_is_registered(self):
        package = json.loads((ROOT / "pinvou3-app/package.json").read_text(encoding="utf-8"))
        finance = package["scripts"]["test:account-platform-finance-ui"]
        self.assertIn("node tests/account_platform_approval_center_smoke.mjs", finance)
        selector = (ROOT / "scripts/select-frontend-smokes.mjs").read_text(encoding="utf-8")
        self.assertIn('command("npm", "test:account-platform-finance-ui")', selector)

    def test_organization_member_smokes_resolve_to_real_browser_files(self):
        package = json.loads((ROOT / "pinvou3-app/package.json").read_text(encoding="utf-8"))
        selector = (ROOT / "scripts/select-frontend-smokes.mjs").read_text(encoding="utf-8")
        for feature, filenames in (
            ("organization", ("account_platform_organization_ui_smoke.mjs", "account_platform_member_units_ui_smoke.mjs", "account_platform_organization_boundaries_ui_smoke.mjs", "account_platform_organization_recharge_ui_smoke.mjs")),
            ("member-units", ("account_platform_member_units_ui_smoke.mjs",)),
        ):
            command = f"test:account-platform-{feature}-ui"
            self.assertEqual(package["scripts"][command], " && ".join(f"node tests/{filename}" for filename in filenames))
            for filename in filenames:
                self.assertTrue((ROOT / "pinvou3-app/tests" / filename).is_file())
            self.assertGreaterEqual(selector.count(f'command("npm", "{command}")'), 2)

    def test_entitlement_correction_production_smoke_is_registered_for_full_and_feature_runs(self):
        package = json.loads((ROOT / "pinvou3-app/package.json").read_text(encoding="utf-8"))
        command = "test:account-platform-entitlement-correction-ui"
        self.assertEqual(package["scripts"][command], "node tests/account_platform_entitlement_production_smoke.mjs")
        self.assertTrue((ROOT / "pinvou3-app/tests/account_platform_entitlement_production_smoke.mjs").is_file())
        selector = (ROOT / "scripts/select-frontend-smokes.mjs").read_text(encoding="utf-8")
        self.assertGreaterEqual(selector.count(f'command("npm", "{command}")'), 2)

    def test_registration_bonus_smokes_resolve_to_both_compiled_browser_suites(self):
        package = json.loads((ROOT / "pinvou3-app/package.json").read_text(encoding="utf-8"))
        command = "test:account-platform-registration-bonus-ui"
        files = ("admin_registration_bonus_ui_smoke.mjs", "admin_registration_bonus_precision_ui_smoke.mjs")
        self.assertEqual(package["scripts"][command], " && ".join(f"node tests/{name}" for name in files))
        for name in files:
            self.assertTrue((ROOT / "pinvou3-app/tests" / name).is_file())
        selector = (ROOT / "scripts/select-frontend-smokes.mjs").read_text(encoding="utf-8")
        self.assertGreaterEqual(selector.count(f'command("npm", "{command}")'), 2)

    def test_closure_smokes_include_production_recovery_and_original_contract(self):
        package = json.loads((ROOT / "pinvou3-app/package.json").read_text(encoding="utf-8"))
        command = "test:account-platform-closure-ui"
        # The composite must retain the original and production suites and also
        # execute the polling regression through the real frontend CI selector.
        files = (
            "account_platform_closure_ui_smoke.mjs",
            "account_platform_closure_production_smoke.mjs",
            "account_platform_closure_polling_smoke.mjs",
        )
        self.assertEqual(package["scripts"][command], " && ".join(f"node tests/{name}" for name in files))
        for name in files:
            self.assertTrue((ROOT / "pinvou3-app/tests" / name).is_file())
        selector = (ROOT / "scripts/select-frontend-smokes.mjs").read_text(encoding="utf-8")
        self.assertGreaterEqual(selector.count(f'command("npm", "{command}")'), 2)

        # Installed-host coverage has its own full/account selector lane; it must
        # remain present without running twice through the closure composite.
        installed_command = "test:account-platform-installed-hosts-ui"
        installed_file = "account_platform_installed_hosts_ui_smoke.mjs"
        self.assertEqual(package["scripts"][installed_command], f"node tests/{installed_file}")
        self.assertTrue((ROOT / "pinvou3-app/tests" / installed_file).is_file())
        self.assertGreaterEqual(selector.count(f'command("npm", "{installed_command}")'), 2)

    def test_embedded_account_portal_assets_are_rebuilt_and_checked(self):
        changes = _without_yaml_comments(
            self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
                "\n  fast-gate:", maxsplit=1
            )[0]
        )
        paths = _extract_quoted_paths(
            changes.split("            frontend:", maxsplit=1)[1].split(
                "            relay:", maxsplit=1
            )[0]
        )
        self.assertTrue(_matches_paths_filter("account-platform/internal/portal/static/admin.html", paths))
        self.assertTrue(_matches_paths_filter("account-platform/internal/portal/static/assets/admin.js", paths))
        frontend = self.pr_workflow.split("\n  frontend-test:", maxsplit=1)[1].split(
            "\n  relay-test:", maxsplit=1
        )[0]
        gate = frontend.split("      - name: Verify embedded Account Portal assets are current", maxsplit=1)[1].split(
            "      - name:", maxsplit=1
        )[0]
        self.assertNotIn("if:", gate)
        self.assertIn("npm run build:account-portal", gate)
        self.assertIn("git diff --exit-code -- ../account-platform/internal/portal/static", gate)
        self.assertIn("git ls-files --others --exclude-standard", gate)
        self.assertIn('test -z "$(git ls-files', gate)

    def test_cross_language_contract_rust_sources_route_to_frontend_test(self):
        # multiagent_plan_normalize.test.mjs reads the Rust sources below for
        # cross-language contract pins (swarm contract text, same-snapshot
        # invariant, edit-resend replay, roster caps). If they are absent from
        # the frontend path filter, a Rust-only PR silently skips that node
        # gate — the same structural blind spot the static-analysis configs
        # above guard against.
        changes = _without_yaml_comments(_changes_block(self.pr_workflow))
        frontend_paths = _frontend_filter_paths(changes)
        for path in (
            "pinvou3-app/src-tauri/src/features/assistant/**",
            "pinvou3-app/src-tauri/src/features/multiagent/transcripts.rs",
            "pinvou3-app/src-tauri/src/features/remote_control/manager/**",
            "pinvou3-app/src-tauri/src/features/sessions/**",
            "pinvou3-app/src-tauri/src/features/personas/mod.rs",
            "pinvou3-app/src-tauri/src/features/files/file_ingest.rs",
            "pinvou3-app/src-tauri/src/app/commands/multiagent.rs",
            "pinvou3-app/src-tauri/src/app/commands/chat.rs",
            "pinvou3-app/src-tauri/src/app/commands/memory.rs",
            "pinvou3-app/src-tauri/src/app/commands/interaction.rs",
            "pinvou3-app/src-tauri/src/app/commands/remote_control.rs",
            "pinvou3-app/src-tauri/src/app/commands/personas.rs",
            "pinvou3-app/src-tauri/src/lib.rs",
        ):
            self.assertIn(
                path,
                frontend_paths,
                f"跨语言契约测试读取的 Rust 源 {path} 不在 frontend filter 中,Rust-only PR 会静默跳过该 node 门禁",
            )

    def test_cross_language_contract_reads_fully_routed(self):
        # 上面的静态清单会随 .mjs 演进漂移:这里从
        # multiagent_plan_normalize.test.mjs 本身派生它读取的全部 src-tauri
        # 目标(单文件 read(...) 与 readdirSync 整目录拼接,单/双引号形式
        # 均归一化匹配),逐一断言 frontend filter 覆盖。给契约测试新增
        # Rust read 而不路由、或把已路由文件挪走,都会在这里失败(本套件
        # 在 fast-gate 每个 PR 必跑)。
        changes = _without_yaml_comments(_changes_block(self.pr_workflow))
        frontend_paths = _extract_quoted_paths(_frontend_filter_paths(changes))
        contract_test = (
            ROOT / "pinvou3-app/tests/multiagent_plan_normalize.test.mjs"
        ).read_text(encoding="utf-8")

        targets = _extract_contract_read_targets(contract_test)

        self.assertTrue(
            targets,
            "未能从 multiagent_plan_normalize.test.mjs 解析出 src-tauri 读取目标",
        )
        for target in targets:
            self.assertTrue(
                _is_covered_by_trigger(target, frontend_paths),
                f"跨语言契约测试读取的 {target} 未被 frontend filter 覆盖,"
                "Rust-only 改动会静默跳过该 node 门禁",
            )

    def test_consent_marker_contract_reads_fully_routed(self):
        # consent_marker_frontend.test.mjs 读取四个 Rust 源做 marker 跨语言
        # 契约断言——Rust-only 改动把 marker 连同其 Rust 值 pin 一起 reword
        # 时,rust-test 与该 node 门禁自身都全绿,而 ToolStoreView 的
        # includes() 静默失配,告警降级为通用文案。从测试源码派生全部
        # .rs 读取目标逐一断言 frontend filter 覆盖;新增读取而未路由、
        # 或已路由文件被挪走,都会在这里失败(本套件在 fast-gate 每个 PR
        # 必跑)。
        frontend_paths = _frontend_filter_entries(self.pr_workflow)
        consent_test = (
            ROOT / "pinvou3-app/tests/consent_marker_frontend.test.mjs"
        ).read_text(encoding="utf-8")

        targets = _extract_consent_marker_read_targets(consent_test)

        # The quoted-literal regex is the parser's contract —
        # a refactor to template literals would silently shrink the target set
        # and unroute a source. A bare count fails just as loudly on that
        # shrinkage or regex rot, while legitimate growth stays fail-closed in
        # the coverage loop below until the new target is routed.
        self.assertEqual(
            len(targets),
            4,
            f"consent marker 契约测试读取目标漂移，实际解析出： {sorted(targets)}",
        )
        for target in targets:
            self.assertTrue(
                _is_covered_by_trigger(target, frontend_paths),
                f"consent marker 契约测试读取的 {target} 未被 frontend filter 覆盖,"
                "Rust-only 改动会静默跳过该 node 门禁",
            )

    def test_install_contract_script_reads_routed(self):
        # connector_online_install_contract.test.js 读取 pinvou3-app/scripts/
        # 下的 smoke 脚本做 floor 钉扎(tauri/build.js 的读取由
        # pinvou3-app/scripts/tauri/** 路由)——scripts-only PR 若不在
        # frontend filter 中,frontend-test 被静默跳过,smoke floor 漂移在该
        # node 门禁不可见。从测试源码派生全部 scripts/ 读取目标逐一断言
        # frontend filter 覆盖;新增读取而未路由会在这里失败(本套件在
        # fast-gate 每个 PR 必跑)。
        frontend_paths = _frontend_filter_entries(self.pr_workflow)
        contract_test = (
            ROOT / "pinvou3-app/tests/connector_online_install_contract.test.js"
        ).read_text(encoding="utf-8")

        targets = _extract_install_contract_script_reads(contract_test)

        self.assertTrue(
            targets,
            "未能从 connector_online_install_contract.test.js 解析出 scripts/"
            " 读取目标(read(\"scripts\", …) 形态变更须同步"
            " _extract_install_contract_script_reads)",
        )
        for target in targets:
            self.assertTrue(
                _is_covered_by_trigger(target, frontend_paths),
                f"install contract 测试读取的 {target}"
                " 未被 frontend filter 覆盖,scripts-only 改动会静默跳过该"
                " node 门禁",
            )

    def test_script_read_derivation_covers_quote_and_shape_variants(self):
        # 回归锁:scripts 派生正则此前只认双引号、单行、恰好 1~2 段的
        # `read("scripts", …)`;单引号 / 跨行 / 三段写法派生出零目标,路由锁
        # 随之静默失效(经变异验证 fail-open)。喂最小 fixture:三种变体都必
        # 须派生出来;字面量解析不出(变量参数)必须抛错而不是静默跳过。
        fixture = (
            "const a = read('scripts', 'future-smoke.sh');\n"
            "const b = read(\n  \"scripts\",\n  \"tauri\",\n  \"build.js\",\n);\n"
            "const c = read(\"scripts\", \"deep\", \"nested\", \"tool.mjs\");\n"
        )
        targets = _extract_install_contract_script_reads(fixture)
        self.assertEqual(
            targets,
            [
                "pinvou3-app/scripts/deep/nested/tool.mjs",
                "pinvou3-app/scripts/future-smoke.sh",
                "pinvou3-app/scripts/tauri/build.js",
            ],
        )
        with self.assertRaises(AssertionError):
            _extract_install_contract_script_reads("read(\"scripts\", scriptName)")
        with self.assertRaises(AssertionError):
            _extract_install_contract_script_reads("const x = 1;")
        # 回归锁:参数表达式只查开头引号会放过 quote 打头的拼接/插值——主
        # 正则把表达式切成错误目标而派生照常非空(fail-open)。逐参数完整字
        # 面量检查下必须抛错;全字面量的多段读取照常派生(上方 fixture 已断言)。
        with self.assertRaises(AssertionError):
            _extract_install_contract_script_reads(
                "const a = read(\"scripts\", \"future-\" + name + \".sh\");\n"
                "const b = read(\"scripts\", \"tauri\", \"build.js\");\n"
            )
        with self.assertRaises(AssertionError):
            _extract_install_contract_script_reads(
                "const a = read(\"scripts\", `future-${name}.sh`);\n"
            )
        with self.assertRaises(AssertionError):
            _extract_install_contract_script_reads(
                "const a = read(\"scripts\", \"future-smoke.sh\" + suffix);\n"
            )

    def test_script_read_derivation_fails_closed_on_non_literal_first_arg(self):
        # 回归锁:变量/模板串首参的 read(…) 主正则看不见,派生结果却因其他
        # 字面量读取仍非空——新增读取静默逃出路由锁(经变异验证 fail-open)。
        # 首参非引号字面量必须抛错;真实测试源(全部字面量首参)必须照常派生。
        # quote 打头的拼接首参骗得过开头引号检查,同样必须抛错。
        contract_test = (
            ROOT
            / "pinvou3-app/tests/connector_online_install_contract.test.js"
        ).read_text(encoding="utf-8")
        self.assertIn(
            "pinvou3-app/scripts/dingtalk-smoke.sh",
            _extract_install_contract_script_reads(contract_test),
        )
        with self.assertRaises(AssertionError):
            _extract_install_contract_script_reads(
                "const dir = 'scripts';\n"
                "const a = read(dir, 'future-smoke.sh');\n"
                "const b = read(\"scripts\", \"tauri\", \"build.js\");\n"
            )
        with self.assertRaises(AssertionError):
            _extract_install_contract_script_reads(
                "const a = read(`scripts`, \"future-smoke.sh\");\n"
            )
        with self.assertRaises(AssertionError):
            _extract_install_contract_script_reads(
                "const a = read(\"scr\" + \"ipts\", \"future-smoke.sh\");\n"
                "const b = read(\"scripts\", \"tauri\", \"build.js\");\n"
            )

    def test_contract_target_derivation_covers_double_quoted_reads(self):
        # 回归锁:派生正则此前只匹配单引号形式,双引号的
        # `read("src-tauri", ...)` 会整体绕过上面的路由锁(经变异验证)。
        # 对派生函数喂最小 fixture:双引号的单文件与整目录目标都必须被
        # 解析出来,且不被缺少该条目的 frontend filter 覆盖——即未来出现
        # 未路由的双引号读取时,路由锁必定失败而不是静默通过。
        fixture = (
            "const direct = read(\"src-tauri\", \"src\", \"features\", "
            "\"future_feature\", \"mod.rs\");\n"
            "const dir = path.join(here, \"..\", \"src-tauri\", \"src\", "
            "\"features\", \"future_module\");\n"
        )
        targets = _extract_contract_read_targets(fixture)
        self.assertIn(
            "pinvou3-app/src-tauri/src/features/future_feature/mod.rs",
            targets,
            "双引号 read 目标必须被派生出来(单引号正则 fail-open 回归)",
        )
        self.assertIn(
            "pinvou3-app/src-tauri/src/features/future_module/**",
            targets,
            "双引号 path.join 整目录目标必须被派生出来",
        )
        unrouted_filter = ["pinvou3-app/src-tauri/src/lib.rs"]
        for target in targets:
            self.assertFalse(
                _is_covered_by_trigger(target, unrouted_filter),
                f"未路由的双引号读取 {target} 不应被无关 filter 覆盖",
            )

    def test_behavior_telemetry_schema_contract_fully_routed(self):
        # Derive embedded inputs from real consumers instead of pinning a stale
        # claim that the v1 schema has no Rust caller. Both production and
        # standalone test consumers must keep schema-only changes on Rust CI.
        workflow = self.pr_workflow
        frontend_paths = _frontend_filter_entries(workflow)
        rust_full_paths = _rust_full_filter_entries(workflow)
        consumers = {}
        for directory in (ROOT / "pinvou3-app/src-tauri/src", ROOT / "pinvou3-app/src-tauri/tests"):
            for source_path in directory.rglob("*.rs"):
                for schema in _rust_embedded_telemetry_inputs(source_path.read_text(encoding="utf-8"), source_path):
                    consumers.setdefault(schema, set()).add(source_path.relative_to(ROOT).as_posix())
        self.assertIn(
            "pinvou3-app/src-tauri/src/features/behavior_telemetry/diagnostics.rs",
            consumers.get("docs/schemas/behavior-telemetry-v1.schema.json", set()),
        )
        self.assertIn(
            "pinvou3-app/src-tauri/src/features/behavior_telemetry_tests.rs",
            consumers.get("docs/schemas/behavior-telemetry-registries.json", set()),
        )
        schemas = {path.relative_to(ROOT).as_posix()
                   for path in (ROOT / "docs/schemas").glob("behavior-telemetry-*.json")}
        self.assertTrue(set(consumers).issubset(schemas), "embedded telemetry input must exist")
        for schema in schemas:
            self.assertTrue(
                _is_covered_by_trigger(schema, frontend_paths),
                f"遥测 schema {schema} 未被 frontend filter 覆盖,"
                "schema-only 改动会静默跳过 behavior_telemetry node 门禁",
            )
        rust_expression = _extract_job_level_if(_extract_job_block(workflow, "rust-test"))[3:-2]
        for schema, sources in consumers.items():
            self.assertTrue(_matches_paths_filter(schema, rust_full_paths),
                            f"{schema} is compiled by {sorted(sources)} but skips Rust CI")
            for event, draft, expected in (("pull_request", False, True),
                                           ("merge_group", False, True),
                                           ("pull_request", True, False)):
                with self.subTest(schema=schema, event=event, draft=draft):
                    self.assertEqual(_evaluate_github_condition(
                        rust_expression, event_name=event, draft=draft, mode="ordinary",
                        rust_full=_matches_paths_filter(schema, rust_full_paths),
                    ), expected)
        self.assertFalse(_matches_paths_filter("docs/schemas/unrelated.schema.json", rust_full_paths),
                         "do not widen schema routing beyond actual Rust compile inputs")

    def test_diagnostic_redaction_parity_runs_for_both_production_implementations(self):
        workflow = self.pr_workflow
        job = _extract_job_block(workflow, "rust-test")
        step = _step_named(job, "Native and Relay diagnostic redaction parity")
        self.assertIn("PINVOU_TEST_NATIVE_REDACTION: '1'", step)
        self.assertIn("run: node --test remote-control-relay/test/diagnostic-redaction.test.js", step)
        self.assertIn("if: ${{ github.event_name != 'push' }}", step)
        self.assertNotIn("continue-on-error", step)
        paths = _rust_full_filter_entries(workflow)
        expression = _extract_job_level_if(job)[3:-2]
        for path in [
            "pinvou3-app/src-tauri/src/features/behavior_telemetry/diagnostic_redaction.rs",
            "remote-control-relay/diagnostic-redaction.js",
            "remote-control-relay/failure-diagnostics.js",
            "remote-control-relay/test/diagnostic-redaction.test.js",
            "remote-control-relay/eval/failure-redaction/native-runner.mjs",
            "remote-control-relay/eval/failure-redaction/native-runner.Cargo.lock",
        ]:
            self.assertTrue((ROOT / path).is_file())
            self.assertTrue(_matches_paths_filter(path, paths), f"parity input skips Rust CI: {path}")
            for event in ["pull_request", "merge_group"]:
                self.assertTrue(_evaluate_github_condition(expression, event_name=event,
                    draft=False, mode="ordinary", rust_full=_matches_paths_filter(path, paths)))

    def test_diagnostic_parity_route_and_opt_in_cannot_be_removed(self):
        original = self.pr_workflow
        try:
            for old in ["              - 'remote-control-relay/diagnostic-redaction.js'\n",
                        "              - 'remote-control-relay/failure-diagnostics.js'\n",
                        "          PINVOU_TEST_NATIVE_REDACTION: '1'\n"]:
                self.pr_workflow = original.replace(old, "", 1)
                self.assertNotEqual(self.pr_workflow, original)
                with self.assertRaises(AssertionError):
                    self.test_diagnostic_redaction_parity_runs_for_both_production_implementations()
        finally:
            self.pr_workflow = original

    def test_diagnostic_native_runner_lock_uses_existing_production_dependencies(self):
        fixture = ROOT / "remote-control-relay/eval/failure-redaction/native-runner.Cargo.lock"
        lock = tomllib.loads(fixture.read_text(encoding="utf-8"))
        app = tomllib.loads((ROOT / "pinvou3-app/src-tauri/Cargo.lock").read_text(encoding="utf-8"))
        identity = lambda p: (p["name"], p["version"], p.get("source"), p.get("checksum"))
        production = {identity(p) for p in app["package"]}
        native = {identity(p) for p in lock["package"] if p.get("source")}
        self.assertTrue(native and native.issubset(production))
        runner = (fixture.parent / "native-runner.mjs").read_text(encoding="utf-8")
        self.assertIn('"--offline", "--locked"', runner)
        self.assertIn('readFileSync(new URL("./native-runner.Cargo.lock", import.meta.url))', runner)

    def test_rust_telemetry_input_extractor_tracks_both_embedding_forms(self):
        source = ROOT / "pinvou3-app/src-tauri/src/features/behavior_telemetry_tests.rs"
        self.assertEqual(_rust_embedded_telemetry_inputs('''
            include_str!("../../../../docs/schemas/behavior-telemetry-registries.json");
            include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../docs/schemas/behavior-telemetry-v1.schema.json"
            ));
            include_str!("../../../../docs/unrelated.md");
        ''', source), {
            "docs/schemas/behavior-telemetry-registries.json",
            "docs/schemas/behavior-telemetry-v1.schema.json",
        })

    def test_schema_only_rust_route_cannot_be_removed_while_frontend_route_remains(self):
        original = self.pr_workflow
        try:
            self.pr_workflow = original.replace(
                "              - 'docs/schemas/behavior-telemetry-v1.schema.json'\n", "", 1,
            )
            self.assertNotEqual(self.pr_workflow, original)
            with self.assertRaisesRegex(AssertionError, "skips Rust CI"):
                self.test_behavior_telemetry_schema_contract_fully_routed()
        finally:
            self.pr_workflow = original

    def test_path_extractor_sees_trailing_comment_entries(self):
        # Round-41 review M8 回归锁:行尾同列注释曾让整条条目对提取器
        # 不可见——`- '!pinvou-cli/**' # trim scope` 对 dorny 是一条真实
        # 排除项,而旧提取器要求行以引号收尾,于是 cli_rust 的可达性、
        # 必需成员与 workflow 排除 pin 全部被同一条目绕过(经变异验证,
        # 43 个测试全绿)。引号闭包正则必须把带注释的条目原样解析出来,
        # 且引用形式却无法解析的形状必须让断言红掉而不是被静默忽略。
        block = (
            "          paths:\n"
            "            - 'pinvou-cli/**'\n"
            "            - '!pinvou-cli/docs/**' # trim the docs subtree\n"
            '            - "pinvou-cli/Cargo.lock"  # lock pin\n'
            "            - 'pinvou-cli/a#b/**' # hash inside the quotes is literal\n"
            # 行尾注释以撇号收尾:贪婪引号闭包曾把注释里的引号一并吞进
            # 路径,解析出一条 dorny 根本看不见的垃圾条目。
            "            - 'pinvou-cli/rename/**' # was 'pinvou-cli/old/**'\n"
        )
        entries = _extract_quoted_paths(block)
        self.assertEqual(
            entries,
            [
                "pinvou-cli/**",
                "!pinvou-cli/docs/**",
                "pinvou-cli/Cargo.lock",
                "pinvou-cli/a#b/**",
                "pinvou-cli/rename/**",
            ],
            "带行尾注释的条目必须被完整解析(dorny 看得见,提取器也必须看得见)",
        )
        with self.assertRaises(AssertionError):
            # 引用形式但引号在本行不闭合:该行对 YAML 是坏的,对旧提取器
            # 是"静默忽略",对现在必须红。
            _extract_quoted_paths("            - 'pinvou-cli/unclosed\n")

    def test_path_extractor_fails_closed_on_folded_scalar_and_bare_entries(self):
        # Round-51 review 回归锁:折叠标量(`- >-` + 缩进文本)与裸词条目
        # (`- pinvou-cli/**`)对 dorny 都是真实的过滤条目(dorny 把 filters 块
        # 重新按 YAML 解析),旧提取器却对"不是引用形式"的列表行静默忽略——
        # 一条折叠标量的 `!pinvou-cli/**` 排除即可让全部 CLI 腿静默跳过,
        # 而下方所有基于提取结果的 pin 全绿(经变异验证)。tight 模式(默认)
        # 下这类行必须让断言红掉,而不是被吞掉。
        folded = (
            "          paths:\n"
            "            - 'pinvou-cli/**'\n"
            "            - >-\n"
            "              !pinvou-cli/**\n"
        )
        with self.assertRaises(AssertionError):
            _extract_quoted_paths(folded)
        bare = (
            "          paths:\n"
            "            - 'pinvou-cli/**'\n"
            "            - !pinvou3-app/src-tauri/**\n"
        )
        with self.assertRaises(AssertionError):
            _extract_quoted_paths(bare)
        # 逃生门(tight=False)已删除:它是零生产调用方的死参数,却给
        # "提取器看不见 → 下方全部 pin 可绕过"留了一扇门。作业步骤行
        # (`- name:`、`- uses:`)等非引号条目形状一律 fail-closed;覆盖
        # 整个 job/文件的调用方必须先把这些行排除在切片之外。
        whole_job = (
            "    needs: changes\n"
            "    steps:\n"
            "      - name: Cargo cache\n"
            "        uses: Swatinem/rust-cache@v2\n"
        )
        with self.assertRaises(AssertionError):
            _extract_quoted_paths(whole_job)
        # 条目行缩进不限:更深缩进的折叠标量排除同样必须红。
        deep_folded = "            - >-\n                !pinvou-cli/docs/**\n"
        with self.assertRaises(AssertionError):
            _extract_quoted_paths(deep_folded)

    def test_trigger_coverage_respects_exclusions(self):
        # dorny/paths-filter(some-with-excludes)的语义是"至少一条正向
        # pattern 命中且没有任何 `!` 排除条目命中"。此前
        # _is_covered_by_trigger 忽略排除条目:frontend 组若新增排除,
        # 路由锁会虚报覆盖而 CI 实际不触发该门禁。
        positive = ["pinvou3-app/src-tauri/src/features/**"]
        self.assertTrue(
            _is_covered_by_trigger(
                "pinvou3-app/src-tauri/src/features/assistant/engine.rs",
                positive,
            )
        )
        with_exclude = positive + [
            "!pinvou3-app/src-tauri/src/features/assistant/**"
        ]
        self.assertFalse(
            _is_covered_by_trigger(
                "pinvou3-app/src-tauri/src/features/assistant/engine.rs",
                with_exclude,
            ),
            "正向命中但被 `!` 排除条目命中的路径不得视为覆盖",
        )
        # 排除只作用于其命中范围,同组其余路径仍被覆盖。
        self.assertTrue(
            _is_covered_by_trigger(
                "pinvou3-app/src-tauri/src/features/sessions/mode_state.rs",
                with_exclude,
            )
        )
        # 精确条目形式的排除同样生效。
        exact_exclude = [
            "pinvou3-app/src-tauri/src/lib.rs",
            "!pinvou3-app/src-tauri/src/lib.rs",
        ]
        self.assertFalse(
            _is_covered_by_trigger(
                "pinvou3-app/src-tauri/src/lib.rs", exact_exclude
            )
        )
        # 只有排除条目、没有任何正向 pattern 时不得视为覆盖。
        self.assertFalse(
            _is_covered_by_trigger(
                "pinvou3-app/src-tauri/src/lib.rs",
                ["!pinvou3-app/src-tauri/src/lib.rs"],
            )
        )

    def test_merge_queue_uses_path_filtering_and_product_gates(self):
        changes = self.workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
        self.assertIn("uses: dorny/paths-filter@v4", changes)
        self.assertIn("github.event_name == 'merge_group'", changes)
        for output in (
            "rust_code",
            "rust_dependencies",
            "rust_full",
            "cli_rust",
            "knowledge_rust",
            "knowledge_dependencies",
            "release_contract",
            "pet",
            "frontend",
            "relay",
            "team_relay_hub",
            "update_release_console",
            "acp_runtime",
            "windows_codex",
            "windows_runtime",
            "windows_rustup_repair",
            "backflow_infra",
            "bundle_chain",
        ):
            self.assertIn(
                f"{output}: ${{{{ steps.filter.outputs.{output} }}}}",
                changes,
            )
        for output, fallback in (
            ("backflow_mode", "full"),
            ("pure_release_backflow", "false"),
        ):
            self.assertIn(f"{output}:", changes)
            self.assertIn(f"|| '{fallback}'", changes)

        required_gate = self.workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertNotIn("完整门禁已在 PR 入队前验证", required_gate)
        self.assertIn("Merge Queue 基础检查失败", required_gate)
        for job in (
            "team-relay-hub-test",
            "update-release-console-test",
            "backflow-portability",
            "release-contract-test",
            "windows-runtime-test",
            "windows-rustup-repair-test",
        ):
            self.assertIn(f"- {job}", required_gate)
            self.assertIn(f'"{job}:$', required_gate)

        hub_job = self.workflow.split(
            "\n  team-relay-hub-test:", maxsplit=1
        )[1].split("\n  release-contract-test:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.team_relay_hub == 'true'", hub_job)
        self.assertRegex(hub_job, r"image: mirror\.gcr\.io/library/postgres@sha256:[a-f0-9]{64}")
        self.assertIn("TEAM_RELAY_TEST_DATABASE_URL", hub_job)
        self.assertIn("npm --prefix team-relay-hub ci", hub_job)
        self.assertIn("npm --prefix team-relay-hub test", hub_job)
        self.assertIn(
            "npm --prefix team-relay-hub audit "
            "--registry=https://registry.npmjs.org --audit-level=high",
            hub_job,
        )

        console_job = self.workflow.split(
            "\n  update-release-console-test:", maxsplit=1
        )[1].split("\n  release-contract-test:", maxsplit=1)[0]
        self.assertIn(
            "needs.changes.outputs.update_release_console == 'true'",
            console_job,
        )
        self.assertIn("npm --prefix update-release-console ci", console_job)
        self.assertIn("npm --prefix update-release-console test", console_job)
        self.assertIn('node-version: ["20", "24"]', console_job)
        self.assertIn("node-version: ${{ matrix.node-version }}", console_job)
        self.assertIn(
            "npm --prefix update-release-console audit "
            "--registry=https://registry.npmjs.org --audit-level=high",
            console_job,
        )
        self.assertIn("bash scripts/ci-update-release-console-container.sh", console_job)
        self.assertIn("bash scripts/ci-update-release-console-container.sh --cleanup", console_job)
        self.assertIn("always() && matrix.node-version == '20'", console_job)
        script = (ROOT / "scripts/ci-update-release-console-container.sh").read_text(encoding="utf-8")
        for command in ("compose config --quiet", "compose up --detach --build", "compose down --volumes --remove-orphans"):
            self.assertIn(command, script)
        self.assertIn('docker build --build-arg NODE_IMAGE="$PINVOU_UPDATE_NODE_IMAGE" ', script)
        self.assertIn("PINVOU_UPDATE_NODE_IMAGE: mirror.gcr.io/library/node@sha256:", console_job)
        self.assertRegex(script, r"mirror\.gcr\.io/library/nginx@sha256:[a-f0-9]{64} nginx -t")
        self.assertIn("$base/healthz", script)
        self.assertIn("$base/api/status", script)
        self.assertNotIn("/var/www/pinvou3", script)

    def test_trusted_backflow_has_parallel_three_platform_light_gate(self):
        portability = self.workflow.split(
            "\n  backflow-portability:", maxsplit=1
        )[1].split("\n  fast-gate:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.trusted_backflow == 'true'", portability)
        self.assertIn("needs.changes.outputs.backflow_infra == 'true'", portability)
        self.assertIn("github.event_name == 'pull_request'", portability)
        self.assertIn("github.event_name == 'merge_group'", portability)
        self.assertIn("os: [ubuntu-22.04, windows-latest, macos-15]", portability)
        self.assertIn("runs-on: ${{ matrix.os }}", portability)
        self.assertIn("fail-fast: false", portability)
        self.assertIn("scripts/community-backflow.mjs preflight", portability)
        self.assertIn("scripts/community-backflow.mjs verify", portability)
        self.assertIn(
            "github.event_name == 'pull_request' && needs.changes.outputs.trusted_backflow == 'true'",
            portability,
        )
        self.assertNotIn("npm ci", portability)
        self.assertNotIn("cargo ", portability)
        self.assertNotIn("submodule update", portability)
        self.assertIn("timeout-minutes: 10", portability)
        self.assertIn("fetch-depth: 1", portability)
        self.assertNotIn("fetch-depth: 0", portability)
        self.assertIn('git fetch --no-tags --depth=1 origin "$BASE_SHA" "$HEAD_SHA"', portability)

    def test_windows_runtime_gate_tracks_python_mcp_manifests(self):
        changes = self.workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  fast-gate:", maxsplit=1
        )[0]
        windows_runtime = changes.split("\n            windows_runtime:", maxsplit=1)[
            1
        ].split("\n            windows_rustup_repair:", maxsplit=1)[0]
        self.assertIn(
            "pinvou3-app/resources/mcp-servers/**/manifest.json",
            windows_runtime,
        )

    def test_standalone_knowledge_crate_has_its_own_required_gate(self):
        changes = _without_yaml_comments(_changes_block(self.pr_workflow))
        self.assertIn("knowledge_rust:", changes)
        self.assertIn("knowledge_dependencies:", changes)
        knowledge_paths = changes.split(
            "            knowledge_rust:", maxsplit=1
        )[1].split("            knowledge_dependencies:", maxsplit=1)[0]
        self.assertIn("- 'pinvou-knowledge/**/*.rs'", knowledge_paths)
        self.assertIn("- 'pinvou-knowledge/deploy/**'", knowledge_paths)
        # Round-51 review: the knowledge-rust alignment step declares
        # pinvou-knowledge/rust-toolchain.toml the single source of truth for
        # the toolchain it resolves, yet the file was routed by no filter
        # group (rust_full routes only the app twin under src-tauri), so a
        # version-bump-only PR skipped the job that aligns to it. Membership
        # is pinned in the same style as the entries above.
        self.assertIn(
            "- 'pinvou-knowledge/rust-toolchain.toml'",
            knowledge_paths,
        )

        knowledge = _without_yaml_comments(
            _extract_job_block(self.pr_workflow, "knowledge-rust")
        )
        self.assertIn("needs.changes.outputs.knowledge_rust == 'true'", knowledge)
        self.assertIn(
            "cargo fmt --manifest-path pinvou-knowledge/Cargo.toml -- --check",
            knowledge,
        )
        self.assertIn(
            "cargo clippy --manifest-path pinvou-knowledge/Cargo.toml --all-targets --all-features --no-deps",
            knowledge,
        )
        # The -D-warnings hard gate must stay in this job (single shared
        # cache); rust-lint must not compile the workspace a second time.
        self.assertIn(
            "cargo clippy --manifest-path pinvou-knowledge/Cargo.toml --lib --bins --no-deps --features server --locked -- -D warnings",
            knowledge,
        )
        # Round-52 review: that step's `if` was unpinned — flipping it to a
        # draft-gated (or constant-false) expression silently skips the
        # -D-warnings clippy on PRs while the command pin above stays
        # green. Pin the name+`if` pair end-anchored (trailing newline),
        # the same pattern as the cli-test step pins.
        self.assertIn(
            "- name: cargo clippy (hard gate, lib/bins/tests + server)\n"
            "        if: ${{ github.event_name != 'pull_request' || "
            "github.event.pull_request.draft == false }}\n",
            knowledge,
            "the -D-warnings hard-gate step's routing drifted; an `if` "
            "rewritten to never fire skips the clippy hard gate on PRs "
            "while the run-command pins stay green",
        )
        self.assertNotIn("cargo clippy pinvou-knowledge", self.pr_workflow)
        self.assertIn(
            "cargo test --manifest-path pinvou-knowledge/Cargo.toml --all-features",
            knowledge,
        )
        self.assertIn("bash -n pinvou-knowledge/deploy/install.sh", knowledge)
        self.assertIn("needs.changes.outputs.knowledge_dependencies == 'true'", knowledge)
        self.assertIn("--manifest-path pinvou-knowledge/Cargo.toml", knowledge)

        required_gate = self._required_gate()
        self.assertIn("- knowledge-rust", required_gate)
        self.assertIn('"knowledge-rust:$KNOWLEDGE_RUST_RESULT"', required_gate)

    def test_fast_gate_actionlint_is_pinned_and_checksum_verified(self):
        fast_gate = self.pr_workflow.split("\n  fast-gate:", maxsplit=1)[1].split(
            "\n  frontend-test:", maxsplit=1
        )[0]
        step = fast_gate.split(
            "- name: workflow lint (actionlint)", maxsplit=1
        )[1].split("\n      - name:", maxsplit=1)[0]
        # The release artifact is fetched from the pinned tag and verified
        # against the release checksums.txt digest. Executing an installer
        # fetched from a mutable ref (e.g. raw.githubusercontent .../main/)
        # would let third-party code drift under a green gate.
        self.assertIn(
            "https://github.com/rhysd/actionlint/releases/download/v1.7.12/actionlint_1.7.12_linux_amd64.tar.gz",
            step,
        )
        self.assertIn(
            "8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8  actionlint.tar.gz",
            step,
        )
        self.assertIn("| sha256sum --check -", step)
        self.assertNotIn("download-actionlint.bash", step)

    def test_fast_gate_lock_registry_guard_is_pinned(self):
        # PR #1518: the lockfile registry-hygiene guard runs in fast-gate on
        # every PR — pin the exact command so deleting the step (or
        # neutralizing it to `|| true`) fails here instead of silently
        # letting mirror URLs and npm-10 libc strips back into the locks.
        # Round-6 review: the command must appear as an executable
        # (non-comment) line — a commented-out command otherwise satisfies
        # assertIn while the guard never runs — and the step must carry no
        # `if:` at all (a decoy replacement with `if: false` would otherwise
        # pass every assertion here while never executing).
        fast_gate = self.pr_workflow.split("\n  fast-gate:", maxsplit=1)[1].split(
            "\n  frontend-test:", maxsplit=1
        )[0]
        step = fast_gate.split(
            "- name: 锁文件 registry 卫生门禁", maxsplit=1
        )[1].split("\n      - name:", maxsplit=1)[0]
        # Round-22 review: substring-on-non-comment-line is echo-neutralizable
        # (`run: echo skip # python3 …` passes) — pin the full exact line.
        self.assertTrue(
            any(
                line.strip() == "run: python3 scripts/check-lock-registry.py"
                for line in step.splitlines()
            ),
            "the lock-guard step must invoke the guard as its whole run line "
            "— a weakened or echoed form runs nothing while passing every "
            "other assertion here",
        )
        self.assertNotIn("|| true", step)
        self.assertNotIn("continue-on-error", step)
        self.assertNotIn(
            "if:",
            _without_yaml_comments(step),
            "the lock-guard step must run unconditionally in fast-gate",
        )
        # Round-11 review: also pin its POSITION before the submodule
        # checkout — the guard's scan scope is THIS repo's own lockfiles
        # (CI sees 5 before submodule init); run after it, the CodeWhale
        # locks enter the tree and the scope silently changes.
        self.assertLess(
            fast_gate.find("- name: 锁文件 registry 卫生门禁"),
            fast_gate.find("- name: 初始化公共底座 submodule"),
            "the lock guard must scan before the submodule checkout brings "
            "in the CodeWhale lockfiles",
        )

    def test_rust_test_registry_guard_step_is_pinned(self):
        # PR #1518 round-5 review: the test-registry guard step runs inside
        # the rust-test leg, but unlike its fast-gate sibling above nothing
        # pinned its existence — deleting or neutralizing the step would keep
        # every test here green. Pin name + command + the push exclusion (the
        # push leg is compile-only warm-cache and must not run the guard).
        # Round-6 review: also pin its POSITION after the PR-leg build step —
        # the guard reuses that step's artifacts, so moving it before the
        # build keeps every assertion here green while silently paying a
        # full ~7-minute recompile on every PR.
        build_step = "cargo test --lib --no-run（编译链接测试二进制）"
        guard_step = "test-registry guard (duplicates + required pins)"
        rust_test = _extract_job_block(self.pr_workflow, "rust-test")
        self.assertLess(
            rust_test.find(build_step),
            rust_test.find(guard_step),
            "test-registry guard must run after the PR-leg build step "
            "(artifact reuse); moving it earlier silently recompiles",
        )
        # Round-23: pin the other side of the window too — the reclaim step
        # deletes the pinvou3_lib-* test binaries the guard's `--list`
        # consumes; moving the guard below it stayed green while silently
        # paying the documented ~4-minute relink on every PR.
        reclaim_step = "- name: 回收已完成的主测试产物"
        self.assertIn(reclaim_step, rust_test)
        self.assertLess(
            rust_test.find(guard_step),
            rust_test.find(reclaim_step),
            "test-registry guard must run before the artifact-reclaim step "
            "(it lists the test binary that step deletes); moving it later "
            "silently relinks",
        )
        step = rust_test.split(
            "- name: test-registry guard (duplicates + required pins)", maxsplit=1
        )[1].split("\n      - name:", maxsplit=1)[0]
        # Round-22 review: substring-on-non-comment-line is echo-neutralizable
        # (`run: echo skip # python3 …` passes) — pin the full exact line.
        self.assertTrue(
            any(
                line.strip() == "run: python3 scripts/test-registry-guard.py"
                for line in step.splitlines()
            ),
            "the registry-guard step must invoke the guard as its whole run "
            "line — a weakened or echoed form runs nothing while passing "
            "every other assertion here",
        )
        self.assertNotIn("|| true", step)
        self.assertNotIn("continue-on-error", step)
        # Round-13: match the push exclusion on executable lines only — a
        # commented copy of the `if:` satisfied the raw-text assertIn while
        # the (deleted) real condition silently widened the guard's run
        # set. Direction is over-run, not bypass, but the same fix as the
        # disk step keeps the file's convention uniform.
        self.assertIn(
            "if: ${{ github.event_name != 'push' }}",
            "\n".join(
                line
                for line in step.splitlines()
                if not line.lstrip().startswith("#")
            ),
            "test-registry guard must stay push-excluded (executable line)",
        )

    def test_windows_runtime_manifest_contract_step_is_pinned(self):
        # PR #1518 round-11 review: the revived test:windows-runtime-manifest
        # contract has exactly one CI executor — this step. The PR's other
        # two new steps were pinned in rounds 5/6; this one shipped without
        # any pin, so deleting the step (or commenting out its command)
        # kept every gate-policy test green while the contract silently
        # stopped running (deletion verified green before this pin).
        job = _extract_job_block(self.pr_workflow, "windows-codex-runtime-test")
        self.assertIn("- name: Windows runtime manifest 契约", job)
        step = job.split(
            "- name: Windows runtime manifest 契约", maxsplit=1
        )[1].split("\n      - name:", maxsplit=1)[0]
        # Round-22 review: substring-on-non-comment-line is echo-neutralizable
        # (`run: echo skip # npm …` passes) — pin the full exact line.
        self.assertTrue(
            any(
                line.strip()
                == "run: npm --prefix pinvou3-app run test:windows-runtime-manifest"
                for line in step.splitlines()
            ),
            "the manifest contract step must invoke the contract suite as "
            "its whole run line — a weakened or echoed form runs nothing "
            "while passing every other assertion here",
        )
        self.assertNotIn("|| true", step)
        self.assertNotIn("continue-on-error", step)
        # Round-12 review: without the `if:` ban an `if: false` self-decoy
        # (or any conditional that evaluates false on the leg) keeps every
        # assertion here green while the contract's only CI executor
        # silently stops — the same decoy class the sibling pins (lock
        # registry, test-registry guard) ban explicitly.
        self.assertNotIn(
            "if:",
            _without_yaml_comments(step),
            "the manifest contract step must run unconditionally on its leg",
        )

    def test_cargo_deny_gate_steps_are_pinned(self):
        # Round-12 review: the deny gate (advisories/yanked) is the leg most
        # exposed to ecosystem drift (the 2026-10 zip yank + RUSTSEC wave
        # landed the day after this branch's last green run), yet its four
        # steps carried no pins — a version drift or a deleted check step
        # would pass every gate-policy test. Pin both legs' install (fixed
        # 0.20.2: the advisory-ignore routing and
        # unused-ignored-advisory semantics are version-sensitive) and the
        # check command (must keep `advisories` and target the shared
        # deny.toml), on executable lines only.
        # Round-13 review: the check step's `if:` itself was unpinned —
        # flipping it to `false` (or dropping one conjunct) silently
        # disarms the hard gate while every assertion here stays green.
        # These steps carry legitimate routing conditions, so instead of
        # the sibling steps' no-`if:` ban, pin the exact condition per
        # leg on non-comment lines.
        for job, install_name, check_name, route_condition in (
            (
                "knowledge-rust",
                "cargo-deny",
                "cargo deny check",
                "needs.changes.outputs.knowledge_dependencies == 'true'",
            ),
            (
                "rust-lint",
                "cargo-deny (advisories/licenses/bans/sources)",
                "cargo deny check (hard gate)",
                "env.RUN_HEAVY_RUST_CHECKS == 'true' && "
                "needs.changes.outputs.rust_dependencies == 'true'",
            ),
        ):
            with self.subTest(job=job):
                job_block = _extract_job_block(self.pr_workflow, job)
                install = job_block.split(f"- name: {install_name}", maxsplit=1)[
                    1
                ].split("\n      - name:", maxsplit=1)[0]
                install_code = "\n".join(
                    line
                    for line in install.splitlines()
                    if not line.lstrip().startswith("#")
                )
                self.assertTrue(
                    any(
                        "tool: cargo-deny@0.20.2" in line
                        for line in install_code.splitlines()
                    ),
                    f"{job}: cargo-deny must stay pinned to 0.20.2 (the "
                    "advisory-ignore routing and unused-ignored-advisory "
                    "semantics are version-sensitive)",
                )
                self.assertEqual(
                    _step_if_value(install_code),
                    " ".join(f"${{{{ {route_condition} }}}}".split()),
                    f"{job}: the deny install step's routing condition "
                    "drifted — the gate must run exactly when the shared "
                    "deny.toml's inputs change",
                )
                check = job_block.split(f"- name: {check_name}", maxsplit=1)[
                    1
                ].split("\n      - name:", maxsplit=1)[0]
                code = "\n".join(
                    line for line in check.splitlines()
                    if not line.lstrip().startswith("#")
                )
                self.assertIn("advisories", code)
                self.assertTrue(
                    # knowledge-rust names the shared config explicitly;
                    # rust-lint reaches it via its working-directory
                    "deny.toml" in code
                    or "working-directory: pinvou3-app/src-tauri" in code,
                    f"{job}: the check must target the shared src-tauri "
                    "deny.toml (explicitly or via working-directory)",
                )
                self.assertEqual(
                    _step_if_value(code),
                    " ".join(f"${{{{ {route_condition} }}}}".split()),
                    f"{job}: the deny check step's `if:` drifted — flipping "
                    "it (to `false` or a weaker condition) would silently "
                    "disarm the advisories/yanked hard gate",
                )
                self.assertNotIn("|| true", check)
                self.assertNotIn("continue-on-error", check)

    def test_deny_toml_keeps_strict_yanked_deny(self):
        # Round-13 review: the whole zip yank episode (rounds 8-12) made
        # `yanked = "deny"` load-bearing — ANY future yank of any pinned
        # crate must fail CI. Nothing machine-checked the value: downgrading
        # to "warn" passed every gate-policy test, the guard suites, and
        # cargo-deny itself. Pin it as a non-comment line of the shared
        # config.
        deny_toml = (
            ROOT / "pinvou3-app/src-tauri/deny.toml"
        ).read_text(encoding="utf-8")
        deny_code = "\n".join(
            line
            for line in deny_toml.splitlines()
            if not line.lstrip().startswith("#")
        )
        # Round-23: match whole executable LINES — a substring check could
        # be satisfied by the literal text inside a TOML string elsewhere
        # while the real key was absent.
        yanked_lines = [
            line.split("#", 1)[0].strip()
            for line in deny_code.splitlines()
            if line.split("#", 1)[0].strip().startswith("yanked")
        ]
        self.assertEqual(
            yanked_lines,
            ['yanked = "deny"'],
            "deny.toml must carry exactly one executable `yanked = \"deny\"` "
            "line — a silent downgrade to warn, a second yanked line, or a "
            "removed key reopens the ecosystem-drift hole",
        )

    def test_deny_toml_unused_ignore_self_enforcement_pinned(self):
        # Round-20 review: `unused-ignored-advisory = "deny"` is the field
        # that makes an expired exception LOUD — once the graph no longer
        # matches an id-level ignore (e.g. the RUSTSEC-2026-0387
        # crypto-bigint exception after an elliptic-curve ecosystem bump),
        # cargo-deny itself fails until the stale entry is removed.
        # Dropping the field passed every gate-policy test (only comments
        # mentioned it), so a future expired exception would sit green
        # forever. Pin it on an executable line next to the yanked value.
        deny_toml = (
            ROOT / "pinvou3-app/src-tauri/deny.toml"
        ).read_text(encoding="utf-8")
        deny_code = "\n".join(
            line
            for line in deny_toml.splitlines()
            if not line.lstrip().startswith("#")
        )
        unused_lines = [
            line.split("#", 1)[0].strip()
            for line in deny_code.splitlines()
            if line.split("#", 1)[0].strip().startswith("unused-ignored-advisory")
        ]
        self.assertEqual(
            unused_lines,
            ['unused-ignored-advisory = "deny"'],
            "deny.toml must carry exactly one executable "
            "`unused-ignored-advisory = \"deny\"` line — without it an "
            "advisory ignore outliving its advisory is silent instead of "
            "failing the deny gate (round-23: line-anchored and unique, like "
            "the yanked pin)",
        )

    def test_deny_toml_bans_the_crypto_bigint_vulnerable_window(self):
        # The retired exception must not leave 0.5.5 outside the independent
        # ban: every affected version below the published fix stays denied.
        policy = tomllib.loads(
            (ROOT / "pinvou3-app/src-tauri/deny.toml").read_text(encoding="utf-8")
        )
        bans = [
            entry for entry in policy["bans"]["deny"]
            if entry.get("name") == "crypto-bigint"
        ]
        self.assertEqual(
            [entry.get("version") for entry in bans], ["<0.7.6"],
            "the complete vulnerable range, including 0.5.5, must stay banned",
        )

    def test_retired_crypto_exception_matches_all_reviewed_lock_graphs(self):
        policy = tomllib.loads(
            (ROOT / "pinvou3-app/src-tauri/deny.toml").read_text(encoding="utf-8")
        )
        ignored = [
            item if isinstance(item, str) else item.get("id")
            for item in policy["advisories"].get("ignore", [])
        ]
        self.assertNotIn("RUSTSEC-2026-0387", ignored)
        self.assertEqual(policy["advisories"]["unused-ignored-advisory"], "deny")
        for lock in (
            "pinvou3-app/src-tauri/Cargo.lock",
            "pinvou-cli/Cargo.lock",
            "pinvou-knowledge/Cargo.lock",
        ):
            with self.subTest(lock=lock):
                packages = tomllib.loads((ROOT / lock).read_text(encoding="utf-8"))["package"]
                self.assertFalse(
                    {"crypto-bigint", "hpke", "p256", "elliptic-curve"}
                    & {package["name"] for package in packages},
                    "reintroduction requires a fresh security assessment",
                )

    def test_zip_lockfiles_sit_inside_the_assessed_advisory_window(self):
        # Round-20 review, re-baselined in round-22: deny.toml retired the
        # zip id-level advisory ignores on the premise that the graph
        # converged on a single zip 9.0.5 (calamine 0.37) — a downgrade
        # below zip 9 would re-arm RUSTSEC-2026-0367/0368/0369, and any
        # other in-band drift must be reassessed against the deny posture
        # in the same change. Nothing else pins the version: pin every repo
        # lock's zip to the exact assessed baseline so a bump fails HERE
        # first.
        zip_locks = []
        for lock in sorted(ROOT.glob("**/Cargo.lock")):
            if "CodeWhale" in str(lock):
                continue  # the public base governs its own locks
            versions = re.findall(
                r'name = "zip"\nversion = "([^"]+)"',
                lock.read_text(encoding="utf-8"),
            )
            if versions:
                zip_locks.append((lock, versions))
        self.assertTrue(
            zip_locks,
            "expected at least one repo lock to carry zip — if the "
            "dependency is gone, remove this pin",
        )
        for lock, versions in zip_locks:
            for version in versions:
                self.assertEqual(
                    version,
                    "9.0.5",
                    f"{lock}: zip {version} is outside the assessed 9.0.5 "
                    "baseline — deny.toml retired the zip advisory ignores "
                    "on the single-zip-9.0.5 premise, so reassess the deny "
                    "posture for the new version and update this pin in the "
                    "same change",
                )

    def test_rust_lint_disk_step_is_draft_gated_and_precedes_zram(self):
        # PR #1518 round-5 review: the rust-lint disk cleanup is only needed
        # for the heavy (non-draft) compile, so it must stay draft-gated —
        # drafts run fmt-only and would otherwise pay ~1-3 minutes per push —
        # and must keep the sibling jobs' disk-before-zram order (swap
        # allocation consumes 8-16 GiB disk before the cleanup runs).
        rust_lint = _extract_job_block(self.pr_workflow, "rust-lint")
        self.assertIn("- name: Prepare Rust disk capacity", rust_lint)
        self.assertIn("- name: Set up zram and swap", rust_lint)
        self.assertLess(
            rust_lint.find("- name: Prepare Rust disk capacity"),
            rust_lint.find("- name: Set up zram and swap"),
            "rust-lint must clean disk before allocating swap (sibling-job order)",
        )
        disk_step = rust_lint.split(
            "- name: Prepare Rust disk capacity", maxsplit=1
        )[1].split("\n      - name:", maxsplit=1)[0]
        # Round-13: executable-lines-only (a commented copy of the `if:`
        # satisfied the raw assertIn while the real gate was dropped).
        self.assertIn(
            "if: ${{ env.RUN_HEAVY_RUST_CHECKS == 'true' }}",
            "\n".join(
                line
                for line in disk_step.splitlines()
                if not line.lstrip().startswith("#")
            ),
            "rust-lint disk cleanup must stay draft-gated (RUN_HEAVY_RUST_CHECKS)",
        )
        self.assertTrue(
            any(
                "python3 scripts/ci-rust-disk.py" in line
                for line in disk_step.splitlines()
                if not line.lstrip().startswith("#")
            ),
            "pinned command must appear as an executable (non-comment) line",
        )
        # Round-20 review: only the command's existence was pinned, not its
        # arguments — appending `--min-free-gib 1` (ENOSPC gate at an
        # empty-disk level) kept every gate-policy test green. Pin the
        # exact recipe the way the rust-test sibling steps do. Round-23:
        # the floor is the rust-test value (24 GiB) — swap lands on disk
        # first (8-16 GiB) and this leg's all-targets clippy/doc target is
        # rust-test-sized, so the 12 GiB default could ENOSPC at cache
        # restore on a zram-degraded runner.
        body = "\n".join(
            line
            for line in disk_step.splitlines()
            if not line.lstrip().startswith("#")
        )
        self.assertIn(
            "run: |\n"
            '          sudo env GITHUB_WORKSPACE="$GITHUB_WORKSPACE" \\\n'
            '            RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \\\n'
            "            python3 scripts/ci-rust-disk.py --min-free-gib 24",
            body,
            "rust-lint's disk step must keep the sibling recipe shape "
            "(sudo env prefix, 24 GiB floor like rust-test)",
        )
        for line in body.splitlines():
            if "ci-rust-disk.py" in line:
                self.assertEqual(
                    line.strip(),
                    "python3 scripts/ci-rust-disk.py --min-free-gib 24",
                    "the disk-clean command must keep the exact 24 GiB floor "
                    "— retuning the ENOSPC gate (lower, or argument-free "
                    "12 GiB default) must land as a deliberate change here",
                )
        self.assertNotIn("|| true", disk_step)

    def test_fast_gate_runner_line_is_pinned(self):
        # Round-47 review: this one line is the ONLY place the gate-policy
        # suite itself executes in CI. Deleting it (or narrowing the `-p`
        # pattern) disarms every pin in this file while each test stays
        # green wherever it still happens to run — so the runner is pinned
        # by its exact command here, making the rest of the suite
        # load-bearing instead of decorative.
        fast_gate = self.pr_workflow.split("\n  fast-gate:", maxsplit=1)[1].split(
            "\n  frontend-test:", maxsplit=1
        )[0]
        # Round-6 review: assert the runner line is EXECUTABLE, not merely
        # present — a commented-out runner satisfies assertIn while the whole
        # suite silently stops running in CI (the meta-bypass of this file).
        self.assertTrue(
            any(
                "python3 -m unittest discover -s scripts/tests -p 'test_*.py'" in line
                for line in fast_gate.splitlines()
                if not line.lstrip().startswith("#")
            ),
            "the gate-policy suite's only CI runner must not be deletable in "
            "silence",
        )

    def assert_run_filtered_filter(self, steps, package, filter, msg=None):
        """Round-50 review: pin a run_filtered invocation's filter token
        EXACTLY. The old substring assertIn stayed green under a renamed
        filter (the old text remained a substring of the mutated line);
        this asserts a run_filtered line naming `-p <package>` exists AND
        ends with `--locked <filter>` as its final token, so a rename —
        or a deletion — fails the policy suite."""
        lines = [
            line.strip()
            for line in steps.splitlines()
            if line.strip().startswith("run_filtered test") and f"-p {package} " in line
        ]
        matches = [line for line in lines if line.endswith(f"--locked {filter}")]
        # The selection above is itself the exact-token check: a line only
        # lands in `matches` when `--locked <filter>` is its FINAL text, so a
        # renamed (or deleted) filter fails the assertTrue — no separate
        # re-assertion of the same property is needed.
        self.assertTrue(
            matches,
            msg or f"no run_filtered line pins -p {package} --locked {filter}",
        )

    def _required_gate(self, text=None):
        # Split off everything from the required-gate job onward. Required-
        # gate's display `name:` is the branch-protection check name; the
        # suite locates the job only by its YAML key, so a display-name
        # rename would silently orphan the required check. Pin it in every
        # test that asserts on this block. `text` lets the comment-stripped
        # body variants reuse the same split and the same name pin.
        required_gate = (self.pr_workflow if text is None else text).split(
            "\n  required-gate:", maxsplit=1
        )[1]
        self.assertIn("name: required-gate", required_gate)
        return required_gate

    def assert_required_gate_wiring(self, job, result_env):
        """断言 required-gate 对单个 leg 的三重接线:needs 条目、`<JOB>_RESULT:`
        env 回填、汇总循环条目。评论剥离后断言:只活在注释里的同名行不满足接线。
        """
        required_gate = self._required_gate(_without_yaml_comments(self.pr_workflow))
        self.assertIn(
            f"- {job}",
            required_gate,
            f"{job} must stay in required-gate's needs list: without "
            "the wire the gate cannot consume the leg at all",
        )
        self.assertIn(
            f"{result_env}: ${{{{ needs.{job}.result }}}}",
            required_gate,
            f"{job}'s result must be backfilled into required-gate's "
            "summary env; a dropped line orphans the loop entry",
        )
        self.assertIn(
            f'"{job}:${result_env}"',
            required_gate,
            f"{job} must enter the failure loop "
            "(success|skipped accepted so path-filtered skips do "
            "not false-fail)",
        )

    def test_cli_crate_has_its_own_required_gate(self):
        changes = _without_yaml_comments(_changes_block(self.pr_workflow))
        self.assertIn("cli_rust:", changes)
        cli_paths = changes.split("            cli_rust:", maxsplit=1)[1].split(
            "            knowledge_rust:", maxsplit=1
        )[0]
        # Round-42 review: membership pins assert against the EXTRACTOR's
        # parsed entries, not the raw slice — a trailing comment quoting the
        # same path (`# was '- 'pinvou-cli/**/Cargo.lock''`) satisfied the
        # raw assertIn while the extractor (and dorny) saw no entry, so a
        # deleted entry kept every pin green. The extractor also fails
        # closed on unrecognized quoting shapes.
        cli_rust_entries_probe = _extract_quoted_paths(cli_paths)

        def assert_extracted_entry(entry: str, message: str) -> None:
            self.assertIn(entry, cli_rust_entries_probe, message)

        assert_extracted_entry(
            "pinvou-cli/**/*.rs",
            "cli_rust must match the real crate directory (pinvou-cli)",
        )
        assert_extracted_entry("pinvou-cli/**/Cargo.toml", "cli_rust must route the Cargo.toml set")
        assert_extracted_entry(
            "pinvou-cli/**/Cargo.lock",
            "every CLI leg builds --locked, so a lockfile-only change (a "
            "dependency bump, a resolver rewrite) changes exactly what they "
            "compile; without this entry such a PR skips cli-test, "
            "windows-rust-test AND macos-cli-check and required-gate passes "
            "on 'skipped'",
        )
        # Round-28: the CLI's remaining build inputs are routed like its
        # sources. .cargo/config.toml feeds EVERY CLI build (a resolver,
        # target, or rustflags change compiles differently everywhere), and
        # build.rs embeds the exe manifest into the Windows binary.
        assert_extracted_entry(
            "pinvou-cli/.cargo/**",
            "cli_rust must route pinvou-cli/.cargo: config.toml changes what "
            "every CLI leg compiles; without this entry such a PR skips "
            "cli-test, cli-lint, windows-rust-test AND macos-cli-check on "
            "'skipped'",
        )
        assert_extracted_entry(
            "pinvou-cli/**/*.manifest",
            "cli_rust must route the exe manifest: build.rs embeds it into "
            "the Windows binary, and without this entry a manifest-only PR "
            "runs no CLI leg at all",
        )
        # Round-47 review: cli-test and cli-lint (the Linux CLI legs)
        # execute this setup script; routed only via rust_code, a
        # script-only PR reached the post-merge push before any CLI leg
        # could catch a break.
        assert_extracted_entry(
            "scripts/ci-libpipewire-build.sh",
            "the PipeWire prefix build is a setup step of the Linux CLI "
            "legs; a script-only PR must trigger them",
        )
        assert_extracted_entry("CodeWhale", "cli_rust must route the foundation gitlink")
        # Round-38: every literal cli_rust filter entry must name a path that
        # EXISTS in the repository. The round-37 mcp-servers entry shipped as
        # `pinvoy3-app/...` (a typo), which no glob ever matches — the entry
        # was dead, the gap it claims to close stayed open, and this suite
        # pinned the dead spelling as if it were coverage. The reachability
        # sweep below (assert_group_paths_reachable, applied to cli_rust by
        # the group_bounds loop) keeps the pin from outliving the path again.
        self.assertTrue(cli_rust_entries_probe, "cli_rust paths 解析为空")
        # Round-40 review M4: cli_rust must route on POSITIVE entries only.
        # The filter runs with dorny's `predicate-quantifier: some-with-excludes`,
        # where one negated entry (`- '!pinvou-cli/**'`) makes cli_rust false
        # for every matching change — and rust_full does not cover CLI `.rs`
        # files, so a single added line would skip cli-test, cli-lint,
        # windows-rust-test and macos-cli-check for all CLI-only PRs while
        # required-gate passes on `skipped`. The extractor above now also
        # sees double-quoted entries, so the quote style cannot hide a
        # negation from this pin either. Exclusions belong in rust_full,
        # which owns them today (feedback/personas/pet).
        for entry in cli_rust_entries_probe:
            self.assertFalse(
                entry.startswith("!"),
                f"cli_rust must not carry negation entries (one '!...' line "
                f"silently disables every CLI gate): {entry}",
            )
        # Submodule gitlinks are not checked out everywhere this suite runs
        # (fast-gate needs no CodeWhale tree), so their existence is pinned
        # by .gitmodules instead of the working tree.
        submodule_paths = set()
        gitmodules = ROOT / ".gitmodules"
        if gitmodules.exists():
            for line in gitmodules.read_text(encoding="utf-8").splitlines():
                stripped = line.strip()
                if stripped.startswith("path = "):
                    submodule_paths.add(stripped[len("path = "):].strip())
        # The connector lock tables are compiled into the CLI with include_str!
        # from src-tauri/src/platform/connector_lock.rs, so editing or
        # deleting one is a CLI source change in all but name — and
        # macos-cli-check, the leg whose reason for existing is exactly those
        # per-target files, is the first thing skipped without this entry.
        # Round-43 review: these pins join the round-42 extractor discipline —
        # a raw `assertIn("- 'path'", slice)` is satisfied by a trailing
        # comment quoting the same path while dorny sees no entry, so the
        # deleted entry kept every pin green.
        assert_extracted_entry(
            "pinvou3-app/src-tauri/resources/platforms/**",
            "the connector lock tables are include_str!'d into the CLI; "
            "without this entry a lock-table PR runs no CLI leg at all",
        )
        # The wider include_str!/build.rs input class: build.rs reads four
        # bundle instruction files + deny_sensitive_paths.sh unconditionally
        # (panicking when absent) and native_installer.rs include_str!s the
        # dws LICENSE — a bundle-resource-only rename must not skip the CLI
        # legs any more than a lock-table edit does.
        assert_extracted_entry(
            "pinvou3-app/src-tauri/resources/common/bundle/**",
            "the bundle resources are build.rs/include_str! inputs of every "
            "pinvou3-tauri compile; without this entry a bundle-resource PR "
            "runs no CLI leg at all",
        )
        # Round-37 review: the remaining include_str! inputs of every CLI-leg
        # compile — mcp_catalog.rs embeds the APP-LEVEL mcp-servers tree (not
        # src-tauri/resources) and marketplace.rs embeds the plugin package
        # spec doc. `pub mod marketplace;` is unconditional, so a rename in
        # either must not skip every compile leg while required-gate passes
        # on skipped.
        assert_extracted_entry(
            "pinvou3-app/resources/mcp-servers/**",
            "mcp_catalog.rs include_str!s the app-level mcp-servers tree; "
            "without this entry an mcp-servers PR runs no CLI leg at all",
        )
        assert_extracted_entry(
            "docs/plugin-package-spec.md",
            "marketplace.rs include_str!s the plugin package spec; without "
            "this entry a spec-doc PR runs no CLI leg at all",
        )
        # Round-49 review: cli_contract.rs include_str!s the gaia benchmark
        # doc and asserts its level table, so a doc-only edit must run a CLI
        # leg. The routing entry alone cannot protect itself — this pin is
        # what makes deleting the entry fail the suite instead of silently
        # re-opening the skipped-legs hole the round-48 entry closed.
        assert_extracted_entry(
            "docs/gaia-benchmark.md",
            "cli_contract.rs include_str!s the gaia benchmark doc; without "
            "this entry a doc-only edit runs no CLI leg at all",
        )
        # The CLI path-depends on the app crate, so the leaf features that
        # rust_full exempts still gate through the CLI suite (a change confined
        # to features/feedback or features/personas would otherwise run NO rust
        # gate at all).
        assert_extracted_entry(
            "pinvou3-app/src-tauri/src/features/feedback/**",
            "feedback changes must gate through the CLI suite",
        )
        assert_extracted_entry(
            "pinvou3-app/src-tauri/src/features/personas/**",
            "personas changes must gate through the CLI suite",
        )
        assert_extracted_entry(
            "pinvou3-app/src-tauri/src/features/pet/**",
            "pet is exempted from rust_full like feedback/personas, so pet Rust "
            "changes must gate through the CLI suite too",
        )
        # Policy: the workflow file itself is deliberately excluded from
        # cli_rust — workflow edits must not link the full-app test suites
        # (this test enforces that). cli-test changes are instead validated
        # by the next cli_rust PR / the merge queue. Extractor-based so a
        # trailing-comment-disguised ENTRY fails this pin (fail closed) the
        # same way a deleted one does.
        self.assertNotIn(
            ".github/workflows/pr-check.yml",
            cli_rust_entries_probe,
            "cli_rust must not include the workflow file: workflow edits must "
            "not link the full-app test suites",
        )

        # Round-40 review: the dir/**-rot class the cli_rust reachability
        # sweep closes is not cli_rust-specific. The same dead-spelling
        # shape in knowledge_rust or windows_codex silently skips the
        # knowledge and Windows codex legs for a change-set they own. The
        # same sweep runs over both groups (existence + dir/** prefix):
        # knowledge_rust gets an explicit call below because the
        # group_bounds pairs bound its neighbors (cli_rust,
        # knowledge_dependencies) instead of knowledge_rust itself, and
        # windows_codex rides the group_bounds list (round-51: its
        # standalone slice was folded into that sweep — same helper, and
        # its `private-runtimes/windows` gitlink resolves through the same
        # .gitmodules exemption the sweep applies).
        def assert_group_paths_reachable(group_block: str, group_name: str) -> None:
            entries = _extract_quoted_paths(group_block)
            self.assertTrue(entries, f"{group_name} paths 解析为空")
            for entry in entries:
                candidate = entry[1:] if entry.startswith("!") else entry
                if candidate.endswith("/**") and not any(
                    char in candidate[:-3] for char in "*?[]{}"
                ):
                    # Round-39 review: the round-37 dead entry was exactly this
                    # shape (`pinvoy3-app/resources/mcp-servers/**` — a typo'd
                    # directory prefix), so the dir/** form must be reachability-
                    # checked too: the prefix directory must exist. The glob tail
                    # itself stays the paths-filter's business.
                    prefix = candidate[: -len("/**")]
                    self.assertTrue(
                        (ROOT / prefix).is_dir(),
                        f"{group_name} 过滤条目的目录前缀在仓库中不存在(死条目): {entry}",
                    )
                    continue
                if any(char in candidate for char in "*?[]{}"):
                    # Glob metachars beyond the dir/** form are resolved by the
                    # paths-filter itself; only literal and dir/** entries can
                    # rot in a checkable way.
                    continue
                if candidate in submodule_paths:
                    continue
                self.assertTrue(
                    (ROOT / candidate).exists(),
                    f"{group_name} 过滤路径在仓库中不存在(死条目): {entry}",
                )

        knowledge_rust_paths = changes.split(
            "            knowledge_rust:", maxsplit=1
        )[1].split("            knowledge_dependencies:", maxsplit=1)[0]
        assert_group_paths_reachable(knowledge_rust_paths, "knowledge_rust")
        # Round-49 review: bundle_chain — the changes block's terminal group —
        # gets its own explicit sweep. The standalone windows_codex slice used
        # to split at `pet:` — a job declared BEFORE it — so the slice ran to
        # EOF and bundle_chain's coverage was an accident of that unbounded
        # sweep. Round-51 review: this explicit sweep is bounded at the next
        # job header (fast-gate) instead of running to EOF of the whole file,
        # so the slice stays a pure filter-entry list and the extractor's
        # fail-closed entry-shape check applies to it; a whole-file slice
        # would need the lenient extractor and could hide a folded-scalar
        # exclusion from this sweep.
        bundle_chain_paths = self.pr_workflow.split(
            "            bundle_chain:", maxsplit=1
        )[1]
        # Round-51 review: this explicit sweep is bounded at the next job
        # header (the first 2-space-indented job key after the changes block —
        # official orders backflow-portability here, the community tree ordered
        # fast-gate) instead of running to EOF of the whole file, so the slice
        # stays a pure filter-entry list and the extractor's fail-closed
        # entry-shape check applies to it; a whole-file slice would need the
        # lenient extractor and could hide a folded-scalar exclusion from this
        # sweep.
        bundle_chain_paths = re.split(
            r"\n  [A-Za-z0-9_.-]+:", bundle_chain_paths, maxsplit=1
        )[0]
        assert_group_paths_reachable(bundle_chain_paths, "bundle_chain")

        # Round-42 review: the sweep now covers EVERY filter group in the
        # changes block, not just the three that originally motivated it —
        # a dead dir/** spelling in rust_code, release_contract, pet,
        # frontend, relay, acp_runtime, rust_full or the dependency groups
        # silently skipped the legs that group owns, exactly like the
        # round-37 pinvoy3-app class. Groups are split sequentially in
        # declaration order, so a NEW group appended without updating this
        # list still gets covered as long as it sits between two known
        # neighbors. Round-49 review: the terminal group's own entries are
        # swept explicitly to EOF (see bundle_chain above) instead of riding
        # the old unbounded windows_codex slice; a group appended after
        # bundle_chain is likewise inside that terminal sweep, but add its
        # pair here anyway so the reachability message names the right
        # group. The pairs mirror the workflow's order.
        group_bounds = [
            ("rust_code", "rust_dependencies"),
            ("rust_dependencies", "rust_full"),
            ("rust_full", "cli_rust"),
            ("cli_rust", "knowledge_rust"),
            ("knowledge_dependencies", "release_contract"),
            ("release_contract", "pet"),
            ("pet", "frontend"),
            ("frontend", "relay"),
            ("relay", "acp_runtime"),
            ("acp_runtime", "windows_codex"),
            ("windows_codex", "bundle_chain"),
        ]
        for group, next_group in group_bounds:
            block = changes.split(f"            {group}:", maxsplit=1)[1].split(
                f"            {next_group}:", maxsplit=1
            )[0]
            assert_group_paths_reachable(block, group)

        cli_test = _without_yaml_comments(
            _extract_job_block(self.pr_workflow, "cli-test")
        )
        self.assertIn("needs.changes.outputs.cli_rust == 'true'", cli_test)
        self.assertIn(
            "github.event.pull_request.draft == false",
            cli_test,
            "draft PRs must skip the heavy CLI leg like the other rust jobs",
        )
        self.assertIn("- name: Set up zram and swap", cli_test)
        self.assertIn("scripts/ci-memory-setup.sh", cli_test)
        self.assertIn(
            "cargo fmt --all --check --manifest-path pinvou-cli/Cargo.toml",
            cli_test,
            "pinvou-cli is a virtual workspace: plain --manifest-path fmt fails "
            "with 'Failed to find targets', --all is required",
        )
        self.assertIn(
            "cargo test --manifest-path pinvou-cli/Cargo.toml --locked --no-fail-fast",
            cli_test,
        )
        self.assertIn(
            "cargo test -p adapter-gaia --features test-support --locked --no-fail-fast",
            cli_test,
            "dataset_contract is required-features-gated and silently skipped by "
            "the workspace run; the gaia timeout pins live there",
        )
        # Round-47 review: the EXECUTION steps' `if` conditions are pinned as
        # name+`if` pairs. The run-command substrings above stay green if a
        # step's `if` flips or dies (e.g. `!= 'push'` → `false`), which would
        # make cli-test report success while running NO tests on PRs or in
        # the Merge Queue — the exact silent-skip class the windows phase
        # map's end-anchored pins close for the Windows leg.
        # Round-51 review: the pairs are END-ANCHORED — each pinned string
        # includes the line's trailing "\n" (the windows phase map uses the
        # regex `(?:\n|$)` for the same guarantee). A bare substring stayed
        # green when `&& false` (or a trailing comment) was appended to the
        # `if:` value, so a leg could skip its tests while the map/pin
        # reported it routed; the trailing newline makes any suffix mutation
        # fail the pin.
        self.assertIn(
            "- name: cargo test (all targets, no-fail-fast)\n"
            "        if: github.event_name != 'push'\n",
            cli_test,
        )
        self.assertIn(
            "- name: cargo compile check (push)\n"
            "        if: github.event_name == 'push'\n",
            cli_test,
        )
        self.assertIn(
            "- name: cargo test (adapter-gaia test-support)\n"
            "        if: github.event_name != 'push'\n",
            cli_test,
        )
        self.assertIn(
            "- name: cargo compile check (push, adapter-gaia test-support)\n"
            "        if: github.event_name == 'push'\n",
            cli_test,
        )
        self.assertIn("cache-targets: false", cli_test)

        # The Windows leg also compile-checks the pinvou-cli workspace: the
        # CLI's cfg(target_os = "windows") branches (exe/cmd shims, taskkill,
        # CREATE_NO_WINDOW) only type-check on a Windows runner, and cli_rust
        # must trigger that leg exactly like it triggers cli-test.
        windows_rust_test = _extract_job_block(
            self.pr_workflow, "windows-rust-test"
        )
        self.assertIn(
            "needs.changes.outputs.cli_rust == 'true'",
            windows_rust_test,
            "windows-rust-test must be triggered by cli_rust: its pinvou-cli "
            "compile check is the only Windows leg for CLI code",
        )
        windows_rust_steps = _without_yaml_comments(windows_rust_test)
        self.assertIn(
            "- name: pinvou-cli Windows compile check",
            windows_rust_steps,
        )
        self.assertIn(
            "cargo check --manifest-path pinvou-cli/Cargo.toml",
            windows_rust_steps,
        )
        self.assertIn("--workspace --all-targets --locked", windows_rust_steps)
        # The adapter-gaia test-support target compiles on the Windows leg:
        # required-features hide it from the workspace-wide steps above, and
        # deleting this step must fail the policy suite.
        self.assertIn(
            "-p adapter-gaia --features test-support --all-targets --locked",
            windows_rust_steps,
        )
        # Round-42 review: the pinvou-cli workspace's own cfg(windows) unit
        # tests EXECUTE here, filtered. Round-43 review: pin the STEP and all
        # three filters — deleting the step (or one filter) must fail the
        # policy suite instead of silently orphaning the Windows pins again
        # (the ACL filter's absence is exactly how a third cfg(windows) test
        # ended up running on no leg at all).
        self.assertIn(
            "- name: pinvou-cli Windows-gated unit tests",
            windows_rust_steps,
        )
        # Round-50 review: the filter pins below assert the run_filtered
        # line's FILTER TOKEN exactly (line-anchored, not substring): the
        # old bare assertIn stayed green when the yml-side filter was
        # RENAMED (the old text remained a substring of the mutated line),
        # so a renamed test orphaned the pin silently. The workflow's
        # runtime zero-match guard is the tripwire that fails the leg; this
        # pin holds the line itself.
        self.assert_run_filtered_filter(windows_rust_steps, "pinvou-cli", "windows_batch_tests")
        self.assert_run_filtered_filter(
            windows_rust_steps,
            "adapter-gaia",
            "dataset_windows",
            "the dataset Windows symlink/reparse rejections must stay gated "
            "to this leg",
        )
        self.assert_run_filtered_filter(
            windows_rust_steps,
            "adapter-gaia",
            "fetch_windows_acl",
            "the Windows ACL privacy round-trip test must stay gated to this "
            "leg — it is the crate's only cfg(windows) ACL pin and ran on no "
            "leg before this filter existed",
        )
        # Round-47 review: the round-46 rebind case-fold filter is the one
        # run_filtered line the suite did NOT pin (all five siblings above
        # were pinned) — deleting the whole line passed the policy suite,
        # the exact silent-orphan failure mode this suite exists to catch.
        self.assert_run_filtered_filter(
            windows_rust_steps,
            "pinvou-cli",
            "rebind_nesting_rejection_folds_case_where_the_os_folds",
            "the rebind case-fold half executes only on this leg; deleting "
            "the filter must fail the suite like its siblings' pins",
        )
        # Round-53 review: the rebind no-op fold's Windows alias half is the
        # same orphan class — pin its run_filtered line so deleting the
        # filter cannot silently strand the cfg(windows) control on no leg.
        self.assert_run_filtered_filter(
            windows_rust_steps,
            "pinvou-cli",
            "rebind_noop_fold_still_collapses_windows_alias_spellings",
            "the rebind no-op fold's Windows alias half executes only on "
            "this leg; deleting the filter must fail the suite like its "
            "siblings' pins",
        )
        # Round-44 review: benchmark-core's cfg(windows) security pins (the
        # DPAPI fail-closed blob tests and the Windows ACL parse pin) ran on
        # NO leg either — `-p benchmark-core` appeared in no workflow, the
        # same orphan class the ACL filter's absence created. Pin both
        # module filters: deleting the step or one filter must fail the
        # policy suite instead of silently orphaning the pins again.
        self.assert_run_filtered_filter(
            windows_rust_steps,
            "benchmark-core",
            "private_prediction::tests::windows_",
        )
        self.assert_run_filtered_filter(
            windows_rust_steps,
            "benchmark-core",
            "windows_private_acl::tests::",
        )
        # The zero-match guard bodies are load-bearing — a renamed test must
        # FAIL the step, not pass silently — so pin them alongside the
        # filters they protect (round-44 review).
        self.assertIn("grep -qE 'running [1-9][0-9]* tests?'", windows_rust_steps)
        self.assertIn('grep -q "^test result: ok"', windows_rust_steps)
        self.assertIn(
            "a renamed test must not become a silent pass", windows_rust_steps
        )
        # Round-53 review (T-ci-2): the job-level routing is pinned verbatim
        # by GATE_JOB_LEVEL_IFS, but that pin cannot see a NEW step added
        # INSIDE the job. The Windows regression leg never runs on
        # merge_group (Merge Queue does not repeat it; Linux validates the
        # combined tree), so a step conditioned on that event would be dead
        # code that still shows green here — assert no step-level `if:`
        # references the event. The job-header chunk (job-level `if:`) is
        # excluded; folded `if: >-` continuations are folded in so the token
        # cannot hide past the `if:` line.
        for step in windows_rust_steps.split("\n      - name:")[1:]:
            step_lines = step.splitlines()
            ifs = [
                number
                for number, line in enumerate(step_lines)
                if re.match(r"^\s+if:", line)
            ]
            for start in ifs:
                condition = [step_lines[start]]
                if step_lines[start].strip().endswith(">-"):
                    for line in step_lines[start + 1:]:
                        if not line.strip() or len(line) - len(line.lstrip(" ")) <= 6:
                            break
                        condition.append(line.strip())
                self.assertNotIn(
                    "merge_group",
                    "\n".join(condition),
                    "windows-rust-test steps must not be conditioned on "
                    "merge_group: the Windows regression leg never runs on "
                    "that event (Linux validates the combined tree), so the "
                    "step would be silently dead on every run",
                )

        # macOS-gated CLI code must type-check somewhere: cli-test is
        # ubuntu-only, so the dedicated macos-cli-check leg mirrors the
        # Windows compile check and gates through required-gate. It runs for
        # ready cli_rust/rust_full PRs, the matching Merge Queue combined tree
        # (green-alone PRs can still combine into a macOS-only compile break),
        # and main push (cumulative); see
        # test_gate_job_level_ifs_are_pinned_verbatim.
        macos_cli = _extract_job_block(self.pr_workflow, "macos-cli-check")
        # Round-46 review: the needs wire stays pinned here too, so a
        # bypass of the wiring test cannot silently drop it.
        self.assertIn("needs: changes", macos_cli)
        self.assertIn("runs-on: macos-15", macos_cli)
        self.assertIn(
            "needs.changes.outputs.cli_rust == 'true'",
            macos_cli,
            "macos-cli-check must use the same cli_rust trigger as cli-test",
        )
        self.assertIn(
            "needs.changes.outputs.rust_full == 'true'",
            macos_cli,
            "macos-cli-check must cover rust_full like cli-test does",
        )
        self.assertIn(
            "github.event.pull_request.draft == false",
            macos_cli,
            "draft PRs must skip the macOS compile leg like the other rust jobs",
        )
        macos_cli_steps = _without_yaml_comments(macos_cli)
        self.assertIn(
            "- name: pinvou-cli macOS compile check",
            macos_cli_steps,
        )
        self.assertIn(
            "cargo check --manifest-path pinvou-cli/Cargo.toml",
            macos_cli_steps,
        )
        self.assertIn("--workspace --all-targets --locked", macos_cli_steps)
        # Mirror of the Windows pin: the adapter-gaia test-support target is
        # invisible to required-features-filtered workspace steps, so the
        # dedicated compile step must stay pinned.
        self.assertIn(
            "-p adapter-gaia --features test-support --all-targets --locked",
            macos_cli_steps,
        )
        self.assertIn(
            "rustup show active-toolchain",
            macos_cli_steps,
            "the macOS leg must run the toolchain pinned by rust-toolchain.toml",
        )

        required_gate = self._required_gate()
        self.assertIn("- cli-test", required_gate)
        self.assertIn('"cli-test:$CLI_TEST_RESULT"', required_gate)
        self.assert_required_gate_wiring("macos-cli-check", "MACOS_CLI_RESULT")

    def test_native_pipewire_builds_install_libclang(self):
        # bindgen needs the shared libclang library even when PipeWire headers
        # and clang itself are present. Hosted images/caches can mask omissions.
        for workflow in (PR_WORKFLOW, RELEASE_WORKFLOW):
            blocks = re.findall(
                r"sudo apt-get install\b.*?(?=\n\s*#|\n\s*if |\n\n)",
                _without_inline_comments(workflow.read_text(encoding="utf-8")),
                re.DOTALL,
            )
            pipewire_blocks = [b for b in blocks if "libpipewire-0.3-dev" in b]
            self.assertTrue(pipewire_blocks, workflow.name)
            for block in pipewire_blocks:
                with self.subTest(workflow=workflow.name, block=block):
                    self.assertRegex(block, r"\blibclang-dev\b")

    def test_cli_lint_tauri_deps_stay_identical_with_cli_test(self):
        # Round-45 review: cli-test and cli-lint carry the same "Install Tauri
        # Linux system dependencies" setup (the workspaces are identical: the
        # CLI compiles pinvou3-tauri as a path dependency, so both legs need
        # the same system libraries). A package fixed or added in one leg only
        # hides which entries are load-bearing. The blocks are deliberately
        # NOT deduplicated into a composite action — the structural pins above
        # read this workflow's literal text — so this pin holds the copies
        # equal instead. Prose comments may differ between the jobs; the
        # executable lines may not.
        marker = "- name: Install Tauri Linux system dependencies"

        def tauri_dep_stanza(job):
            job_text = self.pr_workflow.split(f"\n  {job}:", maxsplit=1)[1]
            _, _, rest = job_text.partition(marker)
            if not rest:
                raise AssertionError(
                    f"{job} no longer carries the {marker!r} step the "
                    "cli-test/cli-lint parity pin needs"
                )
            # Cut at the step's end (the next blank-line-separated step
            # header) rather than at a specific following step name:
            # official's cli-test leg legitimately carries extra steps
            # (e.g. the bge-m3 placeholder) between the apt setup and the
            # PipeWire build, and the parity pin covers only the apt block.
            return _without_yaml_comments(
                marker + rest.split("\n\n", maxsplit=1)[0]
            )

        self.assertEqual(
            tauri_dep_stanza("cli-test"),
            tauri_dep_stanza("cli-lint"),
            "the Tauri Linux system-dependency setup must stay identical "
            "between cli-test and cli-lint: an entry that drifts in one leg "
            "silently hides which packages are load-bearing",
        )

    def test_macos_cli_check_configures_the_sibling_restorable_cache(self):
        # Round-17 close-out, fixed in round 18: the job header claimed "no
        # macOS cache exists in this workflow that a refs/pull/N/merge run
        # could restore", which was false — macos-rust-check configures
        # exactly one, saved only on main, restorable by every PR through
        # rust-cache's prefix fallback (the workflow header's own documented
        # warm-cache design). This leg must keep that restorable treatment:
        # it is the long pole of the serialized macOS runner queue and every
        # uncached run pays a cold compile of the whole workspace.
        macos_cli = _extract_job_block(self.pr_workflow, "macos-cli-check")
        self.assertIn("runs-on: macos-15", macos_cli)
        cache_step = _without_yaml_comments(macos_cli).split(
            "- name: Cargo cache", maxsplit=1
        )[1].split("- name: pinvou-cli macOS compile check", maxsplit=1)[0]
        self.assertIn("uses: Swatinem/rust-cache@v2", cache_step)
        self.assertIn("workspaces: pinvou-cli", cache_step)
        self.assertIn("shared-key: macos-cli-check", cache_step)
        self.assertIn(
            "save-if: ${{ github.ref == 'refs/heads/main' }}",
            cache_step,
            "PR-side runs must stay read-only on the 10GB quota; main is the "
            "sole writer of every warm cache in this workflow (same policy as "
            "macos-rust-check and cli-test)",
        )
        # The sibling-policy half of the round-18 decision, pinned so a future
        # "consolidation" cannot silently alias the keys: macOS artifacts are
        # not interchangeable with the Linux legs' (cli-test compiles the same
        # workspace but its cache is ~/.cargo-only under the standing incident
        # directive, and clippy artifacts differ from rustc ones anyway).
        self.assertNotIn("shared-key: macos-rust-check", cache_step)
        self.assertNotIn("shared-key: cli-test", cache_step)
        # Unlike the Linux legs this cache keeps the target directory: the
        # cache-targets: false directive exists because RESTORED target/
        # entries that needed linking took runners down, and a check-only leg
        # links nothing (rmeta-size artifacts). Regressing to false would
        # silently re-introduce the cold compile this round removed.
        self.assertNotIn("cache-targets:", cache_step)

    def test_cli_lint_disk_capacity_is_admitted_at_each_writing_stage(self):
        # A successful Clippy/check is insufficient when main's cache post
        # still needs disk. Pin executable recipes and order in this job,
        # not a comment, a similarly named decoy, or a sibling's cleanup.
        job = _extract_job_block(self.pr_workflow, "cli-lint")
        initial = "Prepare CLI disk capacity before swap"
        precache = "Check CLI disk capacity before cache"
        postcache = "Check CLI disk capacity after cache"
        stages = ((initial, " --min-free-gib 24"), (precache, ""), (postcache, ""))
        for name, arguments in stages:
            body = _without_inline_comments(_step_named(job, name))
            self.assertEqual(
                [line.strip() for line in body.splitlines() if line.strip()],
                [
                    "- name: " + name,
                    "run: |",
                    'sudo env GITHUB_WORKSPACE="$GITHUB_WORKSPACE" \\',
                    'RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \\',
                    "python3 scripts/ci-rust-disk.py" + arguments,
                ],
                name + ": keep the unconditional base-tier, fail-closed recipe",
            )
        names = [name for name, _ in _steps_of(job)]
        self.assertEqual(names[:2], ["Checkout", initial])
        expected = [
            "Checkout", initial, "Set up zram and swap",
            "Init CodeWhale submodule", "Install Tauri Linux system dependencies",
            "Build PipeWire 1.0.7 dev prefix (jammy SPA headers too old)",
            "Rust toolchain", "Align the toolchain pinned by rust-toolchain.toml",
            precache, "Cargo cache", postcache,
            "cargo clippy (workspace, all targets)",
            "cargo check (workspace, no default features, all targets)",
        ]
        self.assertEqual([name for name in names if name in expected], expected)
        self.assertEqual(names.index("Set up zram and swap"), names.index(initial) + 1)
        self.assertEqual(names.index("Cargo cache"), names.index(precache) + 1)
        self.assertEqual(names.index(postcache), names.index("Cargo cache") + 1)
        self.assertEqual(
            names.index("cargo clippy (workspace, all targets)"),
            names.index(postcache) + 1,
        )

    def test_cli_lint_cache_post_is_gated_after_cargo(self):
        job = _extract_job_block(self.pr_workflow, "cli-lint")
        cache = _without_inline_comments(_step_named(job, "Cargo cache"))
        self.assertEqual([line.strip() for line in cache.splitlines() if line.strip()], [
            "- name: Cargo cache",
            "id: cli_lint_cache",
            "uses: Swatinem/rust-cache@d1527c8f67ef9af357290475d4af4b0fab8f12e0",
            "with:", "workspaces: pinvou-cli", "shared-key: cli-lint",
            "cache-on-failure: false", "save-if: ${{ github.ref == 'refs/heads/main' }}",
        ])
        guard = _without_inline_comments(_step_named(job, "Guard CLI cache post-save"))
        lines = [line.strip() for line in guard.splitlines() if line.strip()]
        self.assertEqual(lines, [
            "- name: Guard CLI cache post-save",
            "if: ${{ always() && !cancelled() && steps.cli_lint_cache.outcome == 'success' }}",
            "run: |",
            'sudo env GITHUB_WORKSPACE="$GITHUB_WORKSPACE" \\',
            'RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \\',
            "python3 scripts/ci-rust-disk.py",
            "printf '%s\\n' 'CACHE_ON_FAILURE=true' >> \"$GITHUB_ENV\"",
        ])
        names = [name for name, _ in _steps_of(job)]
        self.assertEqual(names[-1], "Guard CLI cache post-save")
        self.assertGreater(names.index("Guard CLI cache post-save"), names.index("cargo check (workspace, no default features, all targets)"))
        self.assertNotIn("continue-on-error", guard)
        uncommented = _without_yaml_comments(job)
        self.assertEqual(uncommented.count("CACHE_ON_FAILURE"), 1)
        self.assertEqual(uncommented.count("GITHUB_ENV"), 1)
        # Job/step env overrides GITHUB_ENV for the post condition. This job
        # currently has no env mapping; pin that fact, including quoted keys.
        self.assertNotRegex(uncommented, r"(?m)^\s*(?:env|['\"]env['\"])\s*:")

    def test_cli_lint_cache_guard_rejects_step_env_override(self):
        original = self.pr_workflow
        self.test_cli_lint_cache_post_is_gated_after_cargo()
        try:
            marker = "        id: cli_lint_cache\n"
            self.assertEqual(original.count(marker), 1)
            for added in (
                '        env:\n          CACHE_ON_FAILURE: "true"\n',
                '        env: { CACHE_ON_FAILURE: "true" }\n',
                '        env:\n          "CACHE_ON_FAILURE": "true"\n',
                '        "env": { CACHE_ON_FAILURE: "true" }\n',
            ):
                self.pr_workflow = original.replace(marker, marker + added, 1)
                with self.assertRaises(AssertionError, msg=added):
                    self.test_cli_lint_cache_post_is_gated_after_cargo()
        finally:
            self.pr_workflow = original

    def test_cli_lint_cache_guard_enables_failure_cache_only_after_disk_check(self):
        job = _extract_job_block(self.pr_workflow, "cli-lint")
        guard = _step_named(job, "Guard CLI cache post-save")
        script = textwrap.dedent(guard.split("run: |", maxsplit=1)[1]).strip()
        with tempfile.TemporaryDirectory() as directory:
            env_file = Path(directory) / "github-env"
            env = dict(os.environ, GITHUB_ENV=env_file.as_posix(),
                       GITHUB_WORKSPACE=directory, RUNNER_ENVIRONMENT="github-hosted")
            for disk_exit in (0, 7):
                env_file.unlink(missing_ok=True)
                env["FAKE_DISK_EXIT"] = str(disk_exit)
                # The real workflow commands and shell failure semantics run;
                # only the privileged disk helper is replaced by this probe.
                result = subprocess.run(
                    ["bash", "-eo", "pipefail", "-c",
                     'sudo() { return "$FAKE_DISK_EXIT"; }\n' + script],
                    env=env, capture_output=True, text=True, timeout=10,
                )
                self.assertEqual(result.returncode, disk_exit, result.stderr)
                if disk_exit == 0:
                    self.assertEqual(env_file.read_text(encoding="utf-8"), "CACHE_ON_FAILURE=true\n")
                else:
                    self.assertFalse(env_file.exists(), "failure must not enable cache post")

    def test_cli_lint_job_is_wired_into_required_gate(self):
        # Round-18 review §3: "The CI leg doesn't lint the CLI" — no clippy on
        # any lane for ~50k lines while the src-tauri [lints] bans do not
        # apply to the pinvou-cli workspace. The new lint leg must satisfy the
        # same three-wiring rule as every other gate job (needs entry, env
        # backfill, summary-loop entry); removing the job or unwiring it from
        # required-gate must turn this suite red.
        body = _without_yaml_comments(self.pr_workflow)
        cli_lint = _extract_job_block(body, "cli-lint")
        self.assertIn("needs: changes", cli_lint)
        self.assertIn("runs-on: ubuntu-22.04", cli_lint)
        # The trigger set must mirror cli-test exactly: main push (cumulative,
        # paths-filter independent), the Merge Queue combined tree gated by
        # cli_rust/rust_full, and ready non-draft PRs (drafts skip the heavy
        # leg). That routing lives entirely in the job-level `if:`, which is
        # pinned verbatim by GATE_JOB_LEVEL_IFS["cli-lint"] — any appended
        # `&& false`, deleted branch, or flipped draft gate fails there, so
        # this test keeps only the non-if wiring and command pins.
        # Fail-closed on compile errors and nothing else may soften it: the
        # warn-visible clippy policy (debt cleanup pending the [lints] table,
        # see the job comment) must never grow a bypass here.
        self.assertNotIn("continue-on-error", cli_lint)
        self.assertIn("components: clippy", cli_lint)
        self.assertIn(
            "cargo clippy --manifest-path pinvou-cli/Cargo.toml",
            cli_lint,
            "the CLI lint leg must run clippy via the same --manifest-path "
            "convention as every other CLI leg",
        )
        # --all-targets: tests are linted too; --no-deps: dependencies and the
        # CodeWhale submodule are never linted; --locked like every CLI build.
        self.assertIn("--workspace --all-targets --no-deps --locked", cli_lint)
        # Round-27 review: the warn-visible clippy policy can be defeated by
        # APPENDING lint suppressions after the pinned prefix (`-- -A
        # clippy::all`, `--cap-lints`, a RUSTFLAGS export in the same step).
        # Substring asserts above cannot see an appended tail, so pin the
        # absence of the known bypasses explicitly.
        self.assertNotIn("-A clippy", cli_lint)
        self.assertNotIn("-Aclippy", cli_lint)
        self.assertNotIn("--allow clippy", cli_lint)
        self.assertNotIn("--cap-lints", cli_lint)
        self.assertNotIn("RUSTFLAGS", cli_lint)
        # The featureless build (product-backend off) is exercised nowhere
        # else — cargo test always runs default features — so without this
        # check step the `#[cfg(not(feature = "product-backend"))]` refusal
        # arms in the cli crate could rot silently.
        self.assertIn(
            "cargo check --manifest-path pinvou-cli/Cargo.toml",
            cli_lint,
        )
        self.assertIn(
            "--workspace --all-targets --no-default-features --locked",
            cli_lint,
            "cli-lint must keep compiling the featureless build — lib, bins, "
            "and test targets alike — so the product-backend-off cfg arms "
            "cannot rot silently",
        )
        # Round-42 review: the substring pin above is satisfied by an
        # APPENDED feature toggle — `--no-default-features --features
        # product-backend` keeps the pinned text while compiling the
        # feature-on config, silently un-guarding the refusal arms. Pin the
        # absence of the re-enable on the featureless step.
        # Round-48 review: the steps are folded (`>-`) multi-line run blocks,
        # and the per-LINE check below let the toggle hide on a DIFFERENT
        # line of the same command (cargo accepts `--features` alongside
        # `--no-default-features`), satisfying every assertion while
        # compiling the feature-on config. Fold each run block into one
        # string first; the message keeps the whole block for diagnosis.
        # Round-49 review: the original fold pattern hard-coded a 12-space
        # continuation indent and matched ZERO blocks in this workflow,
        # silently degrading every run to the per-line fallback. The pattern
        # now captures the first continuation line's indent and requires the
        # block's remaining lines to share it, and the fold shape itself is
        # mandatory: a differently-shaped step must fail loudly here instead
        # of reviving the per-line blind spot.
        featureless_steps = [
            " ".join(block.split("\n"))
            for block, _indent in re.findall(
                r"run: >-\n(( +)[^\n]*\n?(?:\2[^\n]*\n?)*)", cli_lint
            )
            if "--no-default-features" in block
        ]
        self.assertTrue(
            featureless_steps,
            "the featureless check step must exist as a folded (>-) run "
            "block; update this pin if the step's YAML shape changes, the "
            "fold is what keeps a sibling-line `--features` toggle visible",
        )
        for block in featureless_steps:
            self.assertNotIn(
                "--features",
                block,
                # Round-43 review: this message interpolates the offending
                # text — without the f-prefix a real failure printed a
                # literal "{block}" instead of the step.
                "the featureless check must not re-enable features on the "
                "same invocation (on any line of the folded run block): "
                "`--no-default-features --features "
                "product-backend` satisfies the substring pin while "
                f"compiling the feature-on config: {block}",
            )
            # Round-51 review: cargo's short spelling of --features slipped
            # past the pin above — `-F product-backend` appended after
            # --locked compiled the feature-on config with every assertion
            # green (mutation-verified bypass). The featureless invocation
            # never legitimately contains `-F`, so one literal covers both
            # the space form and the adjacent `-F<feature>` spelling.
            self.assertNotIn(
                "-F",
                block,
                "the featureless check must not re-enable features via "
                "cargo's short flag (`-F<feature>`, space or adjacent "
                "form, on any line of the folded run block): "
                "`--no-default-features --locked -F product-backend` "
                "satisfies the --features pin while compiling the "
                f"feature-on config: {block}",
            )
        # Secondary net over any unfolded single-line invocation of the same
        # check (defense in depth; the folded assertion above is the
        # load-bearing one).
        for line in cli_lint.splitlines():
            if "--no-default-features" in line:
                self.assertNotIn(
                    "--features",
                    line,
                    "the featureless check must not re-enable features on "
                    f"the same line: {line}",
                )
        # Independent cache keyed to the compiler mode (clippy-driver
        # artifacts are not reusable by the rustc test compilers — same
        # parallel-job split as rust-lint vs rust-test).
        self.assertIn("shared-key: cli-lint", cli_lint)
        self.assertIn(
            "save-if: ${{ github.ref == 'refs/heads/main' }}",
            cli_lint,
        )

        self.assert_required_gate_wiring("cli-lint", "CLI_LINT_RESULT")

    def test_required_gate_accepts_only_success_or_skipped(self):
        # Round-28: the `case "$result" in success|skipped) ;;` line is the
        # ONE predicate deciding whether a failed leg blocks the gate — the
        # membership assertions above only pin loop ENTRIES. Widening the
        # pattern to a catch-all (`*) ;;` without failed=1, or adding
        # `failure` to the accepted set) would accept every failed gate
        # while every other test here stays green, so pin the exact
        # accepted set and forbid a bare catch-all accept.
        required_gate = self._required_gate(_without_yaml_comments(self.pr_workflow))
        self.assertIn(
            "success|skipped) ;;",
            required_gate,
            "the accepted set must stay exactly success|skipped",
        )
        self.assertNotIn(
            "*) ;;",
            required_gate,
            "a catch-all case arm would accept every failed gate result",
        )
        for soft in ("failure) ;;", "cancelled) ;;"):
            self.assertNotIn(
                soft,
                required_gate,
                f"{soft} in the accepted set would let a failed leg pass",
            )
        # The verdict line itself is the last thing that can be disarmed
        # (mutation-checked: `[[ "$failed" -eq 0 ]] || true` passed this
        # whole suite before this pin existed). The bare `[[ ]]` must stay
        # bare so a nonzero count fails the step.
        self.assertTrue(
            _step_named(required_gate, "汇总所有适用检查").rstrip().endswith('[[ "$failed" -eq 0 ]]'),
            "required-gate must end in the bare failed-count verdict",
        )
        for suffix in (
            "[[ \"$failed\" -eq 0 ]] || true",
            "[[ \"$failed\" -eq 0 ]] || :",
            "[[ \"$failed\" -eq 0 ]] || exit 0",
        ):
            self.assertNotIn(
                suffix,
                required_gate,
                "a swallowed verdict would accept every failed gate while "
                "every other test here stays green",
            )

    def test_no_job_level_continue_on_error_disarms_a_gate_job(self):
        # The workflow header states this policy in prose (no gate job
        # carries a job-level continue-on-error) with nothing enforcing it,
        # and it is the cheapest
        # fail-open vector in the file: a job-level `continue-on-error: true`
        # makes the job's own failure non-blocking AND makes
        # `needs.<job>.result` report `success`, so required-gate's
        # `success|skipped` loop accepts it. One line would silently disarm
        # cli-test, windows-rust-test, macos-cli-check or rust-test while the
        # required check stays green.
        #
        # Indentation is the discriminator: job keys sit at 4 spaces
        # (`  <job>:` + `    runs-on:`), step keys at 8 (`      - name:` +
        # `        continue-on-error:`). Comments are stripped first so the
        # header's prose and the "no continue-on-error" annotations on the
        # clippy gate do not count as settings.
        body = _without_yaml_comments(self.pr_workflow)
        job_level = []
        step_level = []
        for number, line in enumerate(body.splitlines(), start=1):
            if not line.strip().startswith("continue-on-error"):
                continue
            indent = len(line) - len(line.lstrip(" "))
            (job_level if indent <= 4 else step_level).append((number, line.strip()))
        self.assertEqual(
            job_level,
            [],
            "a job-level continue-on-error makes needs.<job>.result report "
            "'success' to required-gate, so the whole compile/test leg becomes "
            "advisory while the required check stays green",
        )
        # These reviewed diagnostic-only steps cannot suppress a compiler,
        # test or cleanup failure. Artifact transport is bounded separately;
        # adding another advisory step still requires explicit review here.
        reviewed_diagnostics = (
            "PR test success proof transport",
            "Windows 测试二进制导入诊断",
            "Test resource lifecycle receipts",
            "Financial parallel diagnostics",
            "Account test resource lifecycle receipts",
        )
        self.assertEqual(
            len(step_level),
            len(reviewed_diagnostics),
            "only reviewed diagnostic steps may opt out of blocking; "
            f"found {len(step_level)} continue-on-error settings: {step_level}",
        )
        for name in reviewed_diagnostics:
            diagnostic_step = body.split(
                "- name: " + name + "\n", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
            self.assertIn(
                "continue-on-error: true",
                diagnostic_step,
                "continue-on-error belongs only to the reviewed diagnostic "
                "step, not another step that moved under it",
            )
        diagnostic_job = _extract_job_block(body, 'windows-rust-test')
        matches = list(re.finditer(
            r"(?ms)^      - name: Windows 测试二进制导入诊断\n.*?(?=^      - |\Z)", diagnostic_job
        ))
        self.assertEqual(len(matches), 1, "expected one reviewed diagnostic step")
        self.assertEqual(
            matches[0].group(0).rstrip(),
            textwrap.indent(textwrap.dedent("""\
                - name: Windows 测试二进制导入诊断
                  if: ${{ matrix.phase == 'regression' }}
                  continue-on-error: true
                  shell: pwsh
                  run: python scripts/ci-windows-imports-diagnose.py "$env:PINVOU3_TEST_EXE"
            """).rstrip(), "      "),
        )
        diagnostic_job = _extract_job_block(body, 'frontend-test')
        matches = list(re.finditer(
            r"(?ms)^      - name: Test resource lifecycle receipts\n.*?(?=^      - |\Z)", diagnostic_job
        ))
        self.assertEqual(len(matches), 1, "expected one reviewed diagnostic step")
        self.assertEqual(
            matches[0].group(0).rstrip(),
            textwrap.indent(textwrap.dedent("""\
                - name: Test resource lifecycle receipts
                  id: frontend_receipts_upload
                  if: ${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != '' }}
                  timeout-minutes: 3
                  continue-on-error: true
                  uses: actions/upload-artifact@v7
                  with:
                    name: frontend-resource-lifecycle-${{ github.run_attempt }}
                    path: ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/
                    if-no-files-found: error
                    retention-days: 7
            """).rstrip(), "      "),
        )
        transport = _step_named(_extract_job_block(body, 'required-gate'), 'PR test success proof transport')
        self.assertIn('timeout-minutes: 3', transport)
        self.assertIn('uses: actions/upload-artifact@v7', transport)
        self.assertIn('path: ${{ steps.success_proof.outputs.directory }}/proof.json', transport)
        self.assertNotRegex(transport, r'(?m)^        (?:run|env):')
        diagnostic_job = _extract_job_block(body, 'account-platform-test')
        matches = list(re.finditer(
            r"(?ms)^      - name: Account test resource lifecycle receipts\n.*?(?=^      - |\Z)", diagnostic_job
        ))
        self.assertEqual(len(matches), 1, "expected one reviewed diagnostic step")
        self.assertEqual(
            matches[0].group(0).rstrip(),
            textwrap.indent(textwrap.dedent("""\
                - name: Account test resource lifecycle receipts
                  id: account_receipts_upload
                  if: ${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != '' }}
                  timeout-minutes: 3
                  continue-on-error: true
                  uses: actions/upload-artifact@v7
                  with:
                    name: account-resource-lifecycle-${{ github.run_attempt }}
                    path: |
                      ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/
                      ${{ runner.temp }}/account-platform-deployment-contracts-*/resources/
                    if-no-files-found: error
                    retention-days: 7
            """).rstrip(), "      "),
        )
        diagnostic_job = _extract_job_block(body, 'account-platform-test')
        matches = list(re.finditer(
            r"(?ms)^      - name: Financial parallel diagnostics\n.*?(?=^      - |\Z)", diagnostic_job
        ))
        self.assertEqual(len(matches), 1, "expected one reviewed diagnostic step")
        self.assertEqual(
            matches[0].group(0).rstrip(),
            textwrap.indent(textwrap.dedent("""\
                - name: Financial parallel diagnostics
                  id: financial_diagnostics_upload
                  if: ${{ always() }}
                  timeout-minutes: 3
                  continue-on-error: true
                  uses: actions/upload-artifact@v7
                  with:
                    name: account-financial-parallel-${{ github.run_attempt }}
                    path: ${{ runner.temp }}/financial-parallel-*/
                    if-no-files-found: ignore
                    retention-days: 7
            """).rstrip(), "      "),
        )

    def test_diagnostic_exemptions_cannot_move_to_business_tests_or_jobs(self):
        original = self.pr_workflow
        financial_start = "      - name: Financial parallel diagnostics\n"
        financial_step = original.split(financial_start, maxsplit=1)[1].split(
            "\n      - name:", maxsplit=1
        )[0]
        without_upload_exemption = original.replace(
            financial_step,
            financial_step.replace("        continue-on-error: true\n", "", 1),
            1,
        )
        business = "      - name: Test and vet all Account Platform Go modules\n"
        mutations = {
            "advisory whole job": original.replace(
                "  account-platform-test:\n",
                "  account-platform-test:\n    continue-on-error: true\n",
                1,
            ),
            "extra advisory business step": original.replace(
                business, business + "        continue-on-error: true\n", 1
            ),
            "move upload exemption to business with same total": without_upload_exemption.replace(
                business, business + "        continue-on-error: true\n", 1
            ),
            "upload label on a run command": original.replace(
                financial_step,
                financial_step.replace("uses: actions/upload-artifact@v7", "run: echo not-an-upload", 1),
                1,
            ),
        }
        for first_line, remaining in (
            ("run: exit 37", "        name: Required business command\n"),
            ("id: business_guard", '        name: Required business command\n        "run": exit 37\n'),
            ("if: ${{ always() }}", "        run: exit 37\n"),
            ("env:", "          FIXTURE: yes\n        run: exit 37\n"),
        ):
            # Moving the exception must fail even when the next valid YAML
            # step starts with run/id/if/env and the total count stays four.
            mutations[f"move exemption to {first_line} next step"] = without_upload_exemption.replace(
                "      - name: Stop owned temporary PostgreSQL services\n",
                f"      - {first_line}\n{remaining}        continue-on-error: true\n\n"
                "      - name: Stop owned temporary PostgreSQL services\n",
                1,
            )
        try:
            self.test_no_job_level_continue_on_error_disarms_a_gate_job()
            for label, mutant in mutations.items():
                with self.subTest(label=label):
                    self.assertNotEqual(mutant, original, "the negative control must change the workflow")
                    self.pr_workflow = mutant
                    with self.assertRaises(AssertionError):
                        self.test_no_job_level_continue_on_error_disarms_a_gate_job()
        finally:
            self.pr_workflow = original

    def test_gate_jobs_do_not_swallow_their_exit_status(self):
        # The companion fail-open vector to continue-on-error: appending
        # `|| true` (or `|| :`) to a gate command leaves the step, the job and
        # required-gate all green while the compiler or the test binary
        # actually failed. The workflow uses `|| echo "::warning::..."` for its
        # one genuinely best-effort step (ci-memory-setup), which is a
        # different and deliberate shape, so scanning the gate jobs for the
        # unconditional-success idioms has no false positives today.
        gate_jobs = (
            # Round-51 review: fast-gate RUNS this very suite, so an `|| true`
            # on its runner line (`python3 -m unittest discover ...`) disarms
            # every pin in this file while required-gate still requires
            # fast-gate — it must be scanned like any other gate job. Its one
            # ci-memory-setup line is covered by the same per-job `|| echo`
            # exemption as the other legs (verified: exactly one such line).
            "fast-gate",
            "rust-test",
            "rust-lint",
            "cli-test",
            "cli-lint",
            "windows-rust-test",
            "macos-rust-check",
            "macos-cli-check",
            "knowledge-rust",
        )
        for name in gate_jobs:
            block = _without_yaml_comments(_extract_job_block(self.pr_workflow, name))
            # main's #642 lld probe prints its probe errors with
            # `cat ... >&2 || true` inside the branch that then exits 1, so
            # the idiom there cannot mask a gate command's status. Excise
            # exactly that diagnostic line before the scan; every other
            # occurrence of the idioms below still fails the pin.
            block = "\n".join(
                line
                for line in block.splitlines()
                if 'cat "$probe/a.err" "$probe/b.err" >&2 || true' not in line
            )
            # Round-42 review: the `|| echo` scan exempts exactly ONE
            # documented best-effort step per job — the ci-memory-setup
            # (zram) invocation, whose failure legitimately warns instead of
            # failing the leg. The exemption is BY NAME: the excised line
            # must invoke the setup script, so a new `|| echo` on any other
            # line still fails the pin, and if the setup step is renamed the
            # exemption stops matching and its `|| echo` becomes a failure
            # that must be re-adjudicated.
            setup_lines = [
                line
                for line in block.splitlines()
                if "|| echo" in line and "ci-memory-" in line
            ]
            self.assertLessEqual(
                len(setup_lines),
                1,
                f"{name}: at most one ci-memory-setup line may carry `|| echo`",
            )
            block = "\n".join(
                line for line in block.splitlines() if line not in setup_lines
            )
            # Round-42 review: the workflow's own best-effort idiom
            # (`|| echo "::warning::…"`) belongs in this scan too — appended
            # to a gate command it turns any failure into a warning line and
            # a green step, exactly like `|| true`. Each gate command line
            # must not carry ANY `||` redirect of its exit status; the two
            # documented exceptions stay pinned to their own lines (the
            # lld-probe diagnostic above and the ci-memory-setup step, whose
            # name is asserted on main). Round-51 review: the statement-
            # separator coat of the same swallow (`cmd; true` reports the
            # last command's status) is scanned identically — audited
            # zero-for-zero against every gate job's real steps before being
            # added.
            for idiom in (
                "|| true",
                "|| :",
                "|| exit 0",
                "|| echo",
                "; true",
                "; :",
                "; exit 0",
            ):
                self.assertNotIn(
                    idiom,
                    block,
                    f"{name} must not swallow a command's exit status with "
                    f"'{idiom}': the leg would report success on a real "
                    "compile or test failure and required-gate would accept it",
                )
            # Round-52 review: the spaced scan above only matches the spaced
            # spellings, so the no-space swallow variants (`cmd||true`,
            # `cmd||:`, `cmd;true`, `cmd&&true` — same last-command-status
            # semantics, zero matches in every gate job today) slipped past
            # it. Scan them as regexes with no required space.
            for pattern in (r"\|\|true", r"\|\|:", r";true", r"&&true"):
                self.assertNotRegex(
                    block,
                    pattern,
                    f"{name} must not swallow a command's exit status with "
                    f"the no-space idiom /{pattern}/: the leg would report "
                    "success on a real compile or test failure and "
                    "required-gate would accept it",
                )
            # Round-53 review (T-ci-3): both scans above assume the idiom
            # sits on ONE line, but a folded `run: >-` block can split it
            # across the fold — `cargo ... --locked ||\n  true` folds back
            # to `|| true` at runtime while matching neither scan. Catch the
            # line-split coat too: a line ending in `||` (or `;`) whose next
            # line is exactly the swallow token, indented like any folded
            # continuation. Runs on the same excised block, so the
            # documented exemptions above extend to it unchanged; audited
            # zero-for-zero against every gate job's real steps before
            # being added.
            for pattern in (
                r"(?m)\|\|[ \t]*\n[ \t]+(?:true|:|exit 0)[ \t]*$",
                r"(?m);[ \t]*\n[ \t]+(?:true|:|exit 0)[ \t]*$",
            ):
                self.assertNotRegex(
                    block,
                    pattern,
                    f"{name} must not swallow a command's exit status with "
                    f"the line-split idiom /{pattern}/: a folded `run: >-` "
                    "block re-joins it at runtime, the leg would report "
                    "success on a real compile or test failure and "
                    "required-gate would accept it",
                )
            # A `set +e` scan was considered here and deliberately NOT
            # added: the three current sites (windows-rust-test's submodule
            # init timing probe and its two cargo-check timing steps) all
            # capture `$?` explicitly, restore `set -e` immediately, and
            # re-raise via `exit "$status"`, so a blanket ban would need
            # per-site exemptions that rot. Revisit only if a `set +e`
            # appears WITHOUT its paired restore-plus-explicit-status
            # handling.

    def test_loongarch64_switch_preserves_default_routes_and_disables_only_that_leg(self):
        expression = _extract_job_level_if(
            _extract_job_block(self.pr_workflow, "rust-loongarch64-check")
        )[3:-2]
        for event in ("push", "pull_request", "merge_group"):
            for field in ("rust_full", "cli_rust", "knowledge_rust"):
                for enabled in ("", "true", "false"):
                    with self.subTest(event=event, field=field, enabled=enabled):
                        self.assertEqual(_evaluate_github_condition(
                            expression, event_name=event, draft=False, mode="ordinary",
                            loongarch64_check_enabled=enabled, **{field: True},
                        ), enabled != "false")
        self.assertFalse(_evaluate_github_condition(
            expression, event_name="pull_request", draft=True, mode="ordinary", rust_full=True))
        self.assertFalse(_evaluate_github_condition(
            expression, event_name="pull_request", draft=False, mode="ordinary"))
        for job in GATE_JOB_LEVEL_IFS:
            if job != "rust-loongarch64-check":
                self.assertNotIn("PINVOU_LOONGARCH64_CHECK_ENABLED", _extract_job_level_if(
                    _extract_job_block(self.pr_workflow, job)))

    def test_gate_job_level_ifs_are_pinned_verbatim(self):
        # Round-52 review: a job-level `if: >-` folded block can get
        # `&& false` appended inside its `${{ }}` block (or after `}}` on
        # the single-line form). The leg then skips while required-gate's
        # success|skipped loop accepts it, and every routing pin above —
        # which asserts SUBSTRINGS of the condition — stays green. Pin the
        # exact full text of every gate job's job-level `if:` (extracted
        # line-normalized, so pure re-indentation still matches but any
        # content change does not): an appended `&& false`, a deleted
        # routing branch, or a flipped draft gate fails here.
        for job, expected in GATE_JOB_LEVEL_IFS.items():
            with self.subTest(job=job):
                actual = _extract_job_level_if(
                    _extract_job_block(self.pr_workflow, job)
                )
                self.assertEqual(
                    actual,
                    expected,
                    f"{job}'s job-level `if:` drifted from the pinned text; "
                    "an appended `&& false` (or a deleted routing branch) "
                    "silently skips the leg while required-gate accepts "
                    "'skipped'",
                )

    def test_pinned_gate_step_names_are_unique_within_their_job(self):
        # Round-52 review: the name+`if` step pins are assertIn — a decoy
        # duplicate step carrying the pinned name (and the pinned `if`)
        # satisfies them while the REAL step's `if` is flipped or its run
        # body disabled; the job reports success without executing the
        # pinned command (mutation-verified bypass on cli-test). Every
        # step name this suite pins inside a gate job must occur EXACTLY
        # ONCE in that job's block.
        for job, names in PINNED_GATE_STEP_NAMES.items():
            block = _without_yaml_comments(_extract_job_block(self.pr_workflow, job))
            for name in names:
                with self.subTest(job=job, step=name):
                    occurrences = [
                        line
                        for line in block.splitlines()
                        if line.strip() == f"- name: {name}"
                    ]
                    self.assertEqual(
                        len(occurrences),
                        1,
                        f"{job}: pinned step name '{name}' must occur exactly "
                        "once; a decoy duplicate satisfies the name+`if` "
                        "assertIn pins while the real step is disabled",
                    )

    def test_required_gate_membership_triples_pin_the_unpinned_legs(self):
        # Round-52 review: rust-test, frontend-test, relay-test, rust-lint
        # and macos-codex-runtime-test had NO membership pins — unlike
        # cli-test/cli-lint/knowledge-rust above. Unwiring one of them from
        # required-gate (dropping the needs entry, the env backfill or the
        # summary-loop entry) kept the whole suite green while the gate
        # silently stopped consuming that leg. Pin the same triple for
        # each: needs entry, `<JOB>_RESULT:` backfill, loop entry.
        for job, result_env in (
            ("rust-test", "RUST_RESULT"),
            ("frontend-test", "FRONTEND_RESULT"),
            ("relay-test", "RELAY_RESULT"),
            ("rust-lint", "RUST_LINT_RESULT"),
            ("macos-codex-runtime-test", "MACOS_CODEX_RESULT"),
            ("rust-loongarch64-check", "RUST_LOONGARCH64_CHECK_RESULT"),
        ):
            with self.subTest(job=job):
                self.assert_required_gate_wiring(job, result_env)

    def test_relay_test_keeps_the_ten_minute_cap_that_justifies_draft_runs(self):
        # relay-test runs drafts as well (GATE_JOB_LEVEL_IFS pins the
        # draft-inclusive if); that tradeoff only holds while the suite
        # stays short. A silent timeout bump would invalidate it with no red.
        job = self.pr_workflow.split("\n  relay-test:", maxsplit=1)[1].split(
            "\n  windows-relay-dacl:", maxsplit=1
        )[0]
        self.assertIn("timeout-minutes: 10", job)

    def test_fast_gate_checkout_has_no_ref_indirection(self):
        # Round-52 review: fast-gate RUNS this suite (the runner line is
        # pinned above), so a `ref:` under its actions/checkout makes the
        # whole gate — and every pin in this file — validate another
        # commit (e.g. refs/heads/main) instead of the PR head, while
        # required-gate stays green. Pin the checkout step's exact text
        # (end-anchored by full equality, so an inserted `ref:` line or
        # any other `with:` drift fails) plus an explicit `ref:` ban.
        fast_gate = _extract_job_block(self.pr_workflow, "fast-gate")
        self.assertEqual(
            fast_gate.count("uses: actions/checkout"),
            1,
            "fast-gate must keep exactly one checkout step; a second one "
            "could shadow the pinned head checkout",
        )
        checkout = fast_gate.split(
            "- name: Checkout (含 submodule,fork 为 public 直接拉)",
            maxsplit=1,
        )[1].split("\n\n", maxsplit=1)[0].strip()
        self.assertEqual(
            checkout,
            "uses: actions/checkout@v7\n"
            "        with:\n"
            "          submodules: false\n"
            "          fetch-depth: 0",
            "fast-gate's checkout arguments drifted from the pinned block: "
            "a `ref:` here runs this whole gate suite against another "
            "commit instead of the PR head",
        )
        self.assertNotIn(
            "ref:",
            checkout,
            "fast-gate's checkout must not pin a `ref:` — the suite must "
            "validate the commit under test, not a mutable branch",
        )

    def test_run_heavy_rust_checks_value_is_pinned(self):
        # Round-52 review: the heavy-check switch was pinned by NAME
        # presence only (assertIn in
        # test_rust_modes_run_combined_full_regression_only_for_high_risk),
        # so rewriting the value to a draft-gated (or constant-false)
        # expression silently disabled the clippy/doc hard gates for
        # draft PRs while every consumer pin (`env.RUN_HEAVY_RUST_CHECKS
        # == 'true'`) stayed green. Pin the full value line, end-anchored
        # by its trailing newline so an appended tail fails too, and keep
        # the definition unique.
        self.assertEqual(
            len(
                re.findall(
                    r"^\s*RUN_HEAVY_RUST_CHECKS:",
                    self.pr_workflow,
                    flags=re.MULTILINE,
                )
            ),
            1,
            "RUN_HEAVY_RUST_CHECKS must be defined exactly once (rust-lint's "
            "job env); a second definition could shadow the pinned value",
        )
        rust_lint = _extract_job_block(self.pr_workflow, "rust-lint")
        self.assertIn(
            "      RUN_HEAVY_RUST_CHECKS: "
            "${{ github.event_name != 'pull_request' || "
            "github.event.pull_request.draft == false }}\n",
            rust_lint,
            "RUN_HEAVY_RUST_CHECKS's value drifted: a draft-gated or "
            "constant-false rewrite silently disables the clippy/doc hard "
            "gates while the step pins still name the switch",
        )

    def test_benchmark_jobs_stay_out_of_product_pr_workflow(self):
        self.assertNotIn("\n  benchmark-contract:", self.pr_workflow)
        self.assertNotIn("\n  benchmark-test:", self.pr_workflow)
        changes = _changes_block(self.pr_workflow)
        self.assertNotIn("benchmark:", changes)
        self.assertNotIn("benchmark_cli:", changes)
        self.assertNotIn("benchmark_headless:", changes)
        self.assertNotIn("benchmark_codewhale:", changes)

    def test_full_rust_filter_fails_closed_with_stable_module_boundaries(self):
        changes = _without_yaml_comments(_changes_block(self.pr_workflow))
        rust_full = changes.split("            rust_full:", maxsplit=1)[1].split(
            "            knowledge_rust:", maxsplit=1
        )[0]
        rust_full_paths = _extract_quoted_paths(rust_full)
        self.assertIn(
            "predicate-quantifier: some-with-excludes",
            changes,
        )
        self.assertIn("pinvou3-app/src-tauri/**/*.rs", rust_full_paths)

        low_risk_boundaries = (
            "!pinvou3-app/src-tauri/src/features/feedback/**",
            "!pinvou3-app/src-tauri/src/features/personas/**",
            "!pinvou3-app/src-tauri/src/features/pet/**",
        )
        for boundary in low_risk_boundaries:
            self.assertIn(boundary, rust_full_paths)

        high_risk_examples = (
            "pinvou3-app/src-tauri/src/app/commands/chat.rs",
            "pinvou3-app/src-tauri/src/app/commands/interaction.rs",
            "pinvou3-app/src-tauri/src/app/commands/settings.rs",
            "pinvou3-app/src-tauri/src/features/knowledge/mod.rs",
            "pinvou3-app/src-tauri/src/features/review/mod.rs",
            "pinvou3-app/src-tauri/src/features/runtime_bundle/platform/mod.rs",
            "pinvou3-app/src-tauri/src/features/voice/voice_asr.rs",
            "pinvou3-app/src-tauri/src/features/updater/mod.rs",
            "pinvou3-app/src-tauri/src/features/future_feature/mod.rs",
            "pinvou3-app/src-tauri/src/features/assistant/product_runtime/headless_bridge_contract_tests.rs",
        )
        for path in high_risk_examples:
            self.assertTrue(
                _matches_paths_filter(path, rust_full_paths),
                f"unclassified/high-risk Rust path must run full tests: {path}",
            )

        low_risk_examples = (
            "pinvou3-app/src-tauri/src/features/feedback/mod.rs",
            "pinvou3-app/src-tauri/src/features/personas/mod.rs",
            "pinvou3-app/src-tauri/src/features/pet/platform/detach.rs",
        )
        for path in low_risk_examples:
            self.assertFalse(
                _matches_paths_filter(path, rust_full_paths),
                f"documented low-risk leaf should use the fast route: {path}",
            )

        self.assertNotIn(
            ".github/workflows/pr-check.yml",
            rust_full_paths,
            "workflow policy changes must not link the full application tests",
        )

        # PR #1518: the test-registry guard script is executed by the
        # rust-test leg, so a guard-script-only change must route to that leg
        # (round-47 class: a script the leg runs must select the leg itself).
        # Round-22 review: the two entries are pinned BEHAVIORALLY, not by
        # literal presence — dorny/paths-filter's some-with-excludes lets an
        # appended `!path` entry silence a positive match while every
        # assertIn stays green, so replay the filter for the exact paths and
        # fail on both deletion and any exclude entry that targets them.
        for route_path in (
            "scripts/test-registry-guard.py",
            "scripts/tests/test_registry_guard.py",
        ):
            self.assertTrue(
                (ROOT / route_path).is_file(),
                f"routed path must exist in the checkout: {route_path}",
            )
            self.assertTrue(
                _matches_paths_filter(route_path, rust_full_paths),
                f"{route_path}: a rust_full exclude entry (or the entry's "
                "deletion) silently unroutes the test-registry guard from "
                "the rust-test leg that executes it — a guard-script-only "
                "PR would run no rust leg",
            )
        # Same class, review #1518 round-7: the guard's behavior regression
        # suite pins the module-aware ignore attribution; a change confined
        # to the suite must still run the leg that executes the guard.
        self.assertIn(
            "scripts/tests/test_registry_guard.py",
            rust_full_paths,
            "registry guard behavior tests must trigger the rust-test leg that executes the guard",
        )

        literal_feature_files = [
            path
            for path in rust_full_paths
            if "/src/features/" in path
            and path.endswith(".rs")
            and "*" not in path
        ]
        self.assertEqual(
            literal_feature_files,
            [],
            "rust_full must not enumerate internal feature files",
        )

    def test_rust_modes_run_combined_full_regression_only_for_high_risk(self):
        self.assertIn("merge_group:", self.pr_workflow)
        self.assertIn("ci:full-rust", self.pr_workflow)
        rust_lint = _extract_job_block(self.pr_workflow, "rust-lint")
        self.assertIn("timeout-minutes: 60", rust_lint)
        self.assertIn("RUN_HEAVY_RUST_CHECKS", rust_lint)
        self.assertIn("github.event.pull_request.draft == false", rust_lint)
        self.assertIn("needs.changes.outputs.rust_dependencies == 'true'", rust_lint)

        rust_test = _extract_job_block(self.pr_workflow, "rust-test")
        # rust-test's job-level routing (push, merge_group+rust_full, ready
        # non-draft PRs, rust_full/rust_code+ci:full-rust) is pinned verbatim
        # by GATE_JOB_LEVEL_IFS["rust-test"]; the asserts below keep the
        # step-level pins that `if:` cannot cover.
        self.assertIn(
            "cargo --config 'profile.dev.package.codewhale-tui.opt-level=0' test --manifest-path pinvou3-app/src-tauri/Cargo.toml --no-default-features --lib --features benchmark-hooks --locked -- --test-threads=1",
            rust_test,
        )
        # 开源侧同口径：Linux lib 腿显式开启 benchmark-hooks，headless-bridge
        # 合同回归随全量 lib 测试编译并执行（可编译性另由 rust-lint 的
        # all-targets check 钉住）；--no-default-features 的 local-embed 关闭
        # 理由不变（避免 fastembed/ORT 重复链接进巨型测试二进制）。
        self.assertIn("--no-default-features --lib --features benchmark-hooks --locked --no-run", rust_test)
        # PR #1518 closed the last unlocked repo-workspace cargo-test leg:
        # all four src-tauri cargo-test run lines in this job carry --locked
        # (lib --no-run on PR and push, --lib execution, behavior_telemetry
        # harness). Pin the count so silently dropping any single --locked
        # (e.g. while editing the surrounding command) goes red here.
        # Round-11 review: count EXECUTABLE lines only — a bare `--locked`
        # inside a comment otherwise offsets a real removal and the swap
        # keeps this at 4 while a run line silently unlocks. Round-12
        # review: an inline trailing comment (`cmd  # --locked`) defeats a
        # full-line-only filter too, so each line is stripped at its first
        # `#` before matching — a real relocation into a comment drops the
        # count and goes red.
        def _code_part(line):
            return line.split("#", 1)[0]

        locked_run_lines = [
            line
            for line in rust_test.splitlines()
            if "--locked" in _code_part(line)
            and not line.lstrip().startswith("#")
        ]
        self.assertEqual(
            len(locked_run_lines),
            4,
            "every src-tauri cargo test invocation in rust-test must stay "
            "--locked (counted on comment-stripped code only); when a fifth "
            "cargo test lands here, raise this pin in the same commit",
        )
        # push(main) 只编译暖 cache,不执行测试:MQ 已对同一组合树跑过全量测试;
        # push 恢复暖 cache 后跑全量测试曾连续触发 hosted runner 失联(见 workflow 注释)。
        # Round-53 review: these name+`if` pairs are END-ANCHORED like the
        # round-51 cli-test pins — each pinned string ends with the `if:`
        # line's trailing "\n", so a suffix mutation (`&& false`, a trailing
        # comment) fails the pin instead of silently skipping the leg's
        # tests while required-gate accepts 'skipped'.
        self.assertIn(
            "- name: cargo test --lib（含 strict_mode 回归；真 bge-m3/vLLM 测试已 #[ignore]）\n"
            "        if: ${{ github.event_name != 'push' }}\n",
            rust_test,
        )
        self.assertIn(
            "- name: cargo test --lib --no-run (push main 仅编译暖 cache)\n"
            "        if: ${{ github.event_name == 'push' }}\n",
            rust_test,
        )
        # hosted runner 失联防护(私有仓规格实测为 2-core/7.8GiB RAM,2026-09
        # 探针):编译与执行拆成独立 step(失联后日志全丢,按 step 状态定位阶段),
        # CI 关 DWARF 缩小测试二进制降低链接内存峰值。有效内存由 job 开头的
        # zram 扩容 step(scripts/ci-memory-setup.sh,zram→盘上 swap→zswap→
        # 镜像自带 swap 逐层降级)提供;盘上 swapfile 8G 必建(zram 池 70%
        # 封顶后唯一无界溢出层,/mnt 为 tmpfs 或空间不足时护栏跳过,见
        # ci-memory-setup.sh 头注释);看门狗已删除。
        self.assertIn(
            "- name: cargo test --lib --no-run（编译链接测试二进制）\n"
            "        if: ${{ github.event_name != 'push' }}\n",
            rust_test,
        )
        self.assertIn(
            "cargo test --test behavior_telemetry (standalone memory-bounded harness)",
            rust_test,
        )
        self.assertIn("--test behavior_telemetry", rust_test)
        self.assertIn("--features behavior-telemetry-test", rust_test)
        self.assertEqual(rust_test.count("--no-default-features"), 4)
        self.assertNotIn(
            "cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib",
            rust_test,
        )
        self.assertIn(
            'sudo --preserve-env=GITHUB_ACTIONS,'
            'PINVOU3_CI_DISABLE_ZRAM,RUNNER_ENVIRONMENT bash "${{ github.workspace }}'
            '/scripts/ci-memory-setup.sh"',
            rust_test,
        )
        self.assertNotIn("ci-memguard", self.pr_workflow)
        self.assertNotIn("fallocate -l 3G", rust_test)
        self.assertIn('CARGO_PROFILE_DEV_CODEGEN_UNITS: "16"', rust_test)
        self.assertIn('CARGO_PROFILE_DEV_DEBUG: "0"', rust_test)
        self.assertIn("timeout-minutes: 120", rust_test)
        self.assertIn(
            'RUSTFLAGS: "-C link-arg=-fuse-ld=lld '
            '-C link-arg=-Wl,--thinlto-jobs=1 '
            '-C link-arg=-Wl,--threads=1"',
            rust_test,
        )

    def test_all_linux_jobs_enlarge_runner_memory(self):
        # Every ubuntu-* job must run the zram/swap memory setup right after
        # checkout; Windows/macOS jobs are out of scope (hosted images there
        # have different memory characteristics).
        # Only split at 2-space-indented `key:` lines (job/trigger boundaries),
        # not at deeper indentation.
        setup_step = "- name: Set up zram and swap"
        # rust-lint 曾豁免(Swatinem/rust-cache 恢复与扩容组合曾稳定杀死
        # runner,2026-09-12 前后 5/5 复现;后定性为时间窗环境事故)。自
        # rust-lint 承接 benchmark-hooks all-targets 编译门禁起,其内存压力
        # 与 rust-test 同级,2026-10 起一并强制,不再豁免。
        blocks = re.split(
            r"\n  (?=[A-Za-z0-9_-]+:\s*$)", self.pr_workflow, flags=re.MULTILINE
        )
        linux_jobs = [
            block
            for block in blocks
            if re.search(r"^    runs-on: ubuntu", block, flags=re.MULTILINE)
        ]
        self.assertGreaterEqual(len(linux_jobs), 10)
        for job in linux_jobs:
            job_name = job.strip().split(":", maxsplit=1)[0]
            # Round-11 review: judge the contract on EXECUTABLE lines only —
            # a commented-out step otherwise satisfies every assertIn below
            # while the job actually runs on stock memory.
            job_exec = "\n".join(
                line
                for line in job.splitlines()
                if not line.lstrip().startswith("#")
            )
            self.assertIn(
                setup_step,
                job_exec,
                f"ubuntu job '{job_name}' must run scripts/ci-memory-setup.sh",
            )
            self.assertIn(
                '"${{ github.workspace }}/scripts/ci-memory-setup.sh"',
                job_exec,
                f"ubuntu job '{job_name}' must invoke scripts/ci-memory-setup.sh"
                " via an absolute path (some jobs set a run working-directory)",
            )
            # 内核态挂起无法被脚本内部的用户态 timeout 中断,workflow 侧的
            # timeout 240 + 非致命包装是唯一防线(脚本头注释所声称的最后
            # 一道防线)。漏写包装的 job 挂死时会烧满整个 job 时限,守卫
            # 在此强制每个调用点结构一致。
            self.assertIn(
                'run: timeout --kill-after=15 240 sudo --preserve-env='
                'GITHUB_ACTIONS,PINVOU3_CI_DISABLE_ZRAM,RUNNER_ENVIRONMENT'
                ' bash "${{ github.workspace }}/scripts/ci-memory-setup.sh"',
                job_exec,
                f"ubuntu job '{job_name}' must hard-cap ci-memory-setup with"
                " 'timeout --kill-after=15 240' (userspace hang backstop) and"
                " pass GITHUB_ACTIONS and the PINVOU3_CI_DISABLE_ZRAM emergency"
                " switch through sudo (sudo env_reset strips them, silently"
                " disabling the script's ::warning annotations and the standing"
                " zram opt-out)",
            )
            self.assertIn(
                '|| echo "::warning::ci-memory-setup',
                job_exec,
                f"ubuntu job '{job_name}' must keep ci-memory-setup non-fatal"
                " (degrade to stock runner memory with a ::warning)",
            )
        # 透传与非致命包装契约必须覆盖所有 workflow 的调用点,不只
        # pr-check:其余文件任何一处缩水,都会让注解/应急开关在该 workflow
        # 静默失效(2026-09-18 复审:缩水 release-packages 调用点时,仅扫
        # pr-check 的守卫 27 项全绿)。
        workflows_dir = ROOT / ".github/workflows"
        workflow_paths = sorted(workflows_dir.glob("*.yml")) + sorted(
            workflows_dir.glob("*.yaml")
        )
        call_line = (
            "run: timeout --kill-after=15 240 sudo --preserve-env="
            "GITHUB_ACTIONS,PINVOU3_CI_DISABLE_ZRAM,RUNNER_ENVIRONMENT"
            ' bash "${{ github.workspace }}/scripts/ci-memory-setup.sh"'
        )
        total_call_sites = 0
        for workflow_path in workflow_paths:
            text = workflow_path.read_text(encoding="utf-8")
            call_sites = text.count(
                'bash "${{ github.workspace }}/scripts/ci-memory-setup.sh"'
            )
            if call_sites == 0:
                continue
            total_call_sites += call_sites
            self.assertEqual(
                text.count(call_line),
                call_sites,
                f"{workflow_path.name}: every ci-memory-setup call site must"
                " hard-cap with 'timeout --kill-after=15 240' and pass"
                " GITHUB_ACTIONS and the PINVOU3_CI_* emergency switches"
                " through sudo (sudo env_reset strips them, silently"
                " disabling ::warning annotations and the standing opt-out)",
            )
            self.assertEqual(
                text.count('|| echo "::warning::ci-memory-setup'),
                call_sites,
                f"{workflow_path.name}: every ci-memory-setup call site must"
                " stay non-fatal (degrade to stock runner memory with a"
                " ::warning)",
            )
        self.assertGreaterEqual(
            total_call_sites,
            19,
            "ci-memory-setup call sites across workflows must not shrink"
            " without updating this contract",
        )

    def test_memory_setup_script_pins_annotations_and_honest_degradation(self):
        # #1149 复审跟进:此前该脚本只有 workflow 调用结构被钉住,脚本内容
        # 零覆盖——「声称保留实已删除」这类误导文案的回归现有测试抓不住。
        # 这里钉住三类锚点:zswap 关闭(压缩在内存中的层只允许存在一份)、
        # ::warning:: 注解及其单行约束、诚实失败文案。
        source = (ROOT / "scripts" / "ci-memory-setup.sh").read_text(encoding="utf-8")
        # zram 激活时必须显式关闭 zswap,否则存量 Ubuntu 内核默认开启的
        # zswap 会在 zram 前对每个换出页双重压缩。
        self.assertIn("echo 0 >/sys/module/zswap/params/enabled", source)
        # Actions 下降级告警必须是步骤注解,且注解载荷必须折叠换行与 CR
        # (workflow command 是单行协议,换行处会被截断;runner 的 .NET 行
        # 读取器同样把孤立 CR 当行终止,可伪造第二条 workflow command)。
        self.assertIn('echo "::warning::[memory-setup] ${*//[', source)
        self.assertIn("${*//[$'\\r\\n']/ }", source)
        # 预期可恢复的慢路径(镜像 swap as-is 失败后的重试、全层失败后的
        # 镜像 swapfile 兜底重试)只进日志,不得注解化,否则每个 job 都会
        # 常驻黄标稀释真告警。函数体全文钉死:改回 ::warning:: 注解即红。
        # modprobe 失败不属于可恢复慢路径:2026-10 起脚本不再安装内核包,
        # 该失败是终局降级,必须走 warn 注解让 zram 缺席可见。
        self.assertIn(
            'warn_recoverable() { echo "[memory-setup] WARNING: $*" >&2; }',
            source,
        )
        self.assertIn(
            'warn "modprobe zram failed${modprobe_err:+: ${modprobe_err}};',
            source,
        )
        # 模块加载失败后不得出现任何包管理器安装路径:超时杀掉的 apt 事务
        # 会留下 dpkg 中断状态,毒化同 job 后续必需的依赖安装(静态取代被
        # 删除的行为级 PACKAGE_MANAGER_CALLED 断言)。钉法对齐主线 #2095
        # 后的 no-install 合同:零 apt 安装、零等锁选项。repair_dpkg_state
        # 的注释与 warn 文案合法提及 "later apt-get steps"(脚本零 apt
        # 调用的合同不变),裸 "apt-get " 全文禁令会与该合同相撞
        # (merge queue run 41619867285 即此红)。
        self.assertNotIn("apt-get install", source)
        self.assertNotIn("-o DPkg::Lock::Timeout", source)
        self.assertIn(
            "using fallback swap without installing kernel packages",
            source,
        )
        self.assertIn(
            'warn_recoverable "no active swap after all layers; trying the'
            ' image-provided swapfile"',
            source,
        )
        self.assertIn(
            'warn_recoverable "swapon ${cand} failed as is; trying chmod 600'
            " + mkswap + swapon once\"",
            source,
        )
        self.assertIn(
            'warn_recoverable "no active swap after all layers; trying the'
            ' image-provided swapfile"',
            source,
        )
        # 磁盘 swap 重建失败必须携带诊断,且 removed_note 只允许出现在 rm
        # 实际成功的分支内(裸 `|| removed_note=""` 会被后续无条件赋值覆盖
        # 成死代码);不得再出现「声称保留既有 swap 配置」的旧误导文案。
        self.assertIn(
            "if rm -f /mnt/swapfile; then\n"
            '        removed_note=" (the previous /mnt/swapfile was removed)"\n'
            "      fi",
            source,
        )
        self.assertNotIn("keeping the existing swap configuration", source)

    def test_windows_rust_test_parallel_phases_preserve_routing_and_coverage(self):
        # The all-target check and the linked regressions run as two matrix
        # legs of one required job. Splitting the compile graphs must not
        # turn either leg into an optional check, add a cache writer, or drop
        # a step from its leg.
        windows_rust_test = _without_yaml_comments(
            _extract_job_block(self.pr_workflow, "windows-rust-test")
        )
        self.assertIn("name: windows-rust-test (${{ matrix.phase }})", windows_rust_test)
        self.assertIn("fail-fast: false", windows_rust_test)
        self.assertIn("max-parallel: 2", windows_rust_test)
        self.assertIn("phase: [all-targets-check, regression]", windows_rust_test)
        # Routing is job level, so it applies to both legs unchanged.
        job_if = windows_rust_test.split("\n    if: >-", 1)[1].split("\n    strategy:", 1)[0]
        self.assertNotIn("matrix.phase", job_if)

        steps = re.split(r"\n      - name: ", windows_rust_test)[1:]
        phase_of = {}
        for step in steps:
            name = step.split("\n", 1)[0].strip()
            # Round-46 review: end-anchored, so the exact silent-skip
            # mutation this map advertises against — appending `&& false`
            # AFTER the closing braces — no longer still extracts the phase
            # name and passes. (`}} && false` suffix = the step skips while
            # the map reports it routed.)
            match = re.search(
                r"\n        if: \$\{\{ matrix\.phase == '([a-z-]+)' \}\}[ \t]*(?:\n|$)",
                step,
            )
            phase_of[name] = match.group(1) if match else None
        expected = {
            "Windows Rust 全目标检查": "all-targets-check",
            "pinvou-cli Windows compile check": "all-targets-check",
            "pinvou-cli Windows compile check (adapter-gaia test-support)": "all-targets-check",
            "Windows Rust 单元测试链接检查": "regression",
            "Windows 测试 exe 嵌入 Common-Controls v6 清单": "regression",
            "Windows 测试二进制导入诊断": "regression",
            "CodeWhale Windows PowerShell regressions": "regression",
            "Windows 原子替换状态机回归": "regression",
            # Round-44 review: the step carries all the CLI workspace's
            # Windows-gated pins, so its routing condition is load-bearing —
            # an `if: false` (or an unmatchable condition) would silently
            # skip it while every text pin below stays green.
            "pinvou-cli Windows-gated unit tests": "regression",
            # Shared setup runs on both legs. (The diagnostics step's name
            # follows the official workflow's Chinese step title.)
            "初始化公共底座 submodule": None,
            "Cargo cache": None,
            "Windows Rust 缓存基线诊断": None,
        }
        for name, phase in expected.items():
            with self.subTest(step=name):
                self.assertIn(name, phase_of)
                self.assertEqual(phase_of[name], phase)
        # Round-45 review: `phase_of` is a dict keyed by step name, so a
        # second step with the same name would shadow the pinned one and
        # keep every assertion above green while the real step's routing
        # `if` is flipped (the silent-skip bypass). Each pinned name must
        # appear exactly once.
        step_names = [
            step.split("\n", 1)[0].strip()
            for step in re.split(r"\n      - name: ", windows_rust_test)[1:]
        ]
        duplicated = {
            name for name in step_names if step_names.count(name) > 1
        }
        self.assertEqual(
            duplicated,
            set(),
            "duplicate step names shadow the phase map; the routing pins "
            f"cannot hold: {sorted(duplicated)}",
        )
        self.assertIn("--all-targets --features dev-tools", windows_rust_test)
        self.assertIn("--lib --no-run --locked --features benchmark-hooks --message-format=json",
                      windows_rust_test,
                      "round-45 review: the regression link check must compile the "
                      "benchmark-hooks contract module, or the round-44 filter for "
                      "windows_attachment_runtime_stays_security_gated matches 0 tests")

        # Both legs restore one established namespace; only regression on
        # main may save, so there is no second writer or new key.
        self.assertEqual(windows_rust_test.count("shared-key:"), 1)
        self.assertIn("shared-key: windows-rust-test", windows_rust_test)
        self.assertIn(
            "save-if: ${{ matrix.phase == 'regression' && "
            "github.ref == 'refs/heads/main' }}",
            windows_rust_test,
        )
        self.assertIn("WINDOWS_RUST_CACHE", windows_rust_test)
        self.assertIn("WINDOWS_RUST_TIMING", windows_rust_test)
        self.assertNotIn("actions/setup-node", windows_rust_test)
        # The workspaces list must be EXACTLY the three compiled entries:
        # the app workspace, the CodeWhale regression target, and pinvou-cli
        # (check leg + filtered regression tests). Dropping an entry
        # cold-compiles that workspace on the slowest leg again; adding one
        # silently rotates the exact key and grows the quota on that leg.
        # The block
        # is extracted from the Cargo cache STEP and bounded by indentation
        # (entry lines are indented deeper than the sibling `with:` keys):
        # a first-occurrence "workspaces: |" split over the whole job could
        # be captured by a decoy echo, and an exact-indent `shared-key:`
        # end anchor breaks silently when the `with:` keys are reordered.
        cargo_cache_step = windows_rust_test.split(
            "- name: Cargo cache", maxsplit=1
        )[1].split("\n      - name:", maxsplit=1)[0]
        self.assertIn(
            "workspaces: |", cargo_cache_step,
            "workspaces marker missing from the Cargo cache step — the "
            "extraction below must fail, not widen",
        )
        workspaces_lines = cargo_cache_step.split(
            "workspaces: |", maxsplit=1
        )[1].splitlines()[1:]
        workspaces_block = []
        for line in workspaces_lines:
            if line.strip() and not line.startswith("            "):
                break
            workspaces_block.append(line)
        workspaces_text = "\n".join(workspaces_block)
        workspaces_entries = sorted(
            line.strip() for line in workspaces_block if line.strip()
        )
        self.assertEqual(
            workspaces_entries,
            ["CodeWhale", "pinvou-cli", "pinvou3-app/src-tauri"],
            "the windows-rust-test cache must cache EXACTLY the three "
            "compiled workspaces: a dropped entry cold-compiles its "
            "workspace on every run, an extra entry silently rotates the "
            "exact key and grows the quota on the slowest leg",
        )

    def test_windows_regression_loop_pins_the_round43_and_round44_app_filters(self):
        # Round-44 review: six app-side cfg(windows) tests ran on no leg —
        # three in the dependencies platform module, two in codex_acp's
        # platform module, and the headless-bridge attachment security gate
        # (whose cfg(not(windows)) sibling DOES run on Linux rust-test). The
        # round-43 additions to this loop were themselves never pinned by
        # the policy suite, so a filter deletion would have gone unpunished.
        # Pin the loop's round-43/44 additions and its zero-match guard;
        # deleting one must fail the suite instead of silently orphaning the
        # pins again.
        windows_rust_test = _without_yaml_comments(
            _extract_job_block(self.pr_workflow, "windows-rust-test")
        )
        for needle in [
            # Keep the transport and actual A-port connect-deadline
            # regressions in the executable Windows loop.
            "'features::remote_control::controller::relay_transport::tests::' \\",
            "'features::remote_control::controller::controlling_service::tests::connect_attempt_' \\",
            "'failed_probe_never_deletes_an_existing_store_windows' \\",
            "'features::voice_shortcut::platform::tests::' \\",
            "'app::commands::dependencies::platform::windows::tests::' \\",
            "'features::codex_acp::platform::windows::tests::' \\",
            # Round-47 review: the 22 cfg(windows)-only tests under
            # platform::os::windows (windows_path 18 + windows_system 4)
            # executed on NO leg — the orphan class rounds 42-44 claimed
            # eliminated. The module filter runs them here; deleting it
            # re-orphans them, so it is pinned like its siblings.
            "'platform::os::windows::' \\",
            "'windows_attachment_runtime_stays_security_gated'; do",
            # The loop's zero-match guard is load-bearing (a renamed test
            # must fail the step, not pass silently).
            "回归过滤器 '$filter' 匹配 0 个测试",
        ]:
            self.assertIn(needle, windows_rust_test)

    def test_windows_regression_executes_managed_process_and_install_tests(self):
        # Linux excludes the Windows Job Object regressions; the other Windows
        # phase only type-checks them. Pin both modules in the executable loop,
        # including late cancellation and armed/disarmed install guard coverage.
        job = _without_yaml_comments(
            _extract_job_block(self.pr_workflow, "windows-rust-test")
        )
        step = job.split(
            "\n      - name: Windows 原子替换状态机回归\n", 1
        )[1].split("\n      - name:", 1)[0]
        loop = step.split("for filter in \\\n", 1)[1].split("; do", 1)[0]
        filters = re.findall(r"(?m)^\s*'([^']+)'(?:\s*\\)?\s*$", loop)
        for module in (
            "platform::process::tests::",
            "features::codex_acp::install::tests::",
        ):
            with self.subTest(module=module):
                self.assertEqual(filters.count(module), 1)
        self.assertIn('"$test_exe" "$filter" --test-threads=1', step)
        self.assertIn(
            "grep -qE 'running [1-9][0-9]* tests?' /tmp/windows_regression.log || {",
            step,
        )
        self.assertIn("exit 1", step.split("grep -qE", 1)[1])

    def test_memory_setup_disk_swap_is_mandatory(self):
        # 2026-09-19 拍板:磁盘 swap 必建,不再是 PINVOU3_CI_ENABLE_DISK_SWAP
        # opt-in(zram 池 70% 封顶后,8G /mnt swapfile 是唯一无界溢出层)。
        # 开关残留即红;主流程必须无条件调用(顶层零缩进),8G 定值一并钉死。
        source = (ROOT / "scripts" / "ci-memory-setup.sh").read_text(encoding="utf-8")
        self.assertNotIn("PINVOU3_CI_ENABLE_DISK_SWAP", source)
        self.assertIn("DISK_SWAP_SIZE_KIB=$((8 * 1024 * 1024))", source)
        self.assertIn("DISK_SWAP_SIZE_KIB=$((16 * 1024 * 1024))", source)
        self.assertIn(
            "swapon --show=NAME --noheadings 2>/dev/null | grep -q '/dev/zram'",
            source,
        )
        # 2026-10-28 修订,2026-11-02 随主线 #2095 再修订:zram 层不再安装
        # 内核包(modprobe 失败直接降级磁盘 swap/zswap,脚本自身零 apt 调用,
        # 永不变更包管理器状态),同 job 的 dpkg 中断态改由本 PR 的
        # repair_dpkg_state 在退出路径兜底(unattended-upgrades 等外部来源
        # 仍可留下中断态)。合同钉住:脚本零 apt 安装/等锁选项(禁装政策)、
        # 修复的按钟等锁重试(40s 预算、每次尝试仍 60s 上限、真实失败早停)
        # 与 stderr 透传、updates 残留 journal 门、TERM 兜底,以及退出路径
        # 上的修复兜底。
        # 这些文本钉只是廉价漂移警报;生产行为由
        # CiMemorySetupRepairBehaviorTests 端到端兜底(函数体清空/删调用
        # 在那里必红)。
        self.assertNotIn("apt-get install", source)
        self.assertNotIn("-o DPkg::Lock::Timeout", source)
        # 定义 + EXIT trap + 头注 journal 门说明,共 3 处;减少即说明
        # 兜底被拆,行为测试会给出具体断层数据。
        self.assertEqual(source.count("repair_dpkg_state"), 3)
        self.assertIn("run_to 60 dpkg --configure -a", source)
        self.assertIn("wait_deadline=$(( $(date +%s) + 40 ))", source)
        self.assertIn(
            "    if (( attempt_secs >= 2 )) || (( $(date +%s) >= wait_deadline )); then",
            source,
        )
        self.assertIn("    sleep 5\n  done", source)
        self.assertIn(
            'warn "dpkg --configure -a (attempt ${attempt}) failed after ${attempt_secs}s, retrying${configure_err:+: ${configure_err}}"',
            source,
        )
        self.assertIn('log "dpkg admin state repaired (configure attempt ${attempt})"', source)
        self.assertIn(
            'compgen -G "${PINVOU3_CI_DPKG_UPDATES_DIR:-/var/lib/dpkg/updates}/[0-9]*" >/dev/null',
            source,
        )
        self.assertIn(
            'warn "dpkg --configure -a did not succeed; later apt-get steps in this job may still fail until it is run manually${audit_out:+ (dpkg --audit still reports: ${audit_out})}"',
            source,
        )
        self.assertIn("trap 'exit 143' TERM", source)
        self.assertIn("trap repair_dpkg_state EXIT", source)
        self.assertIn(
            'log "provisioning the mandatory /mnt disk swap"\nsetup_disk_swap',
            source,
        )

    def test_fetch_connectors_verifies_checksums_with_loud_diagnostics(self):
        # #1155 复审跟进:fetch-connectors.sh 的 verify_file 在 set -e 下被
        # 三处调用点裸调(check_only 门禁、下载复核、二进制落位前),失配若
        # 零输出退出,门禁只剩 "exit code 1"。诊断内建在 verify_file,任何
        # 调用点都无法静默化;下载前的探测调用自行 >/dev/null 2>&1 抑制。
        source = (ROOT / "scripts" / "fetch-connectors.sh").read_text(
            encoding="utf-8"
        )
        # 失配必须携带期望/实际值(文件缺失/不可读单列,不可读属 I/O 故障
        # 而非哈希差异),并显式 return 1。
        self.assertIn(
            'echo "sha256 校验失败: $1（期望 $2，实际 $(compute_sha256 "$1")）"',
            source,
        )
        self.assertIn('echo "sha256 校验失败: $1（文件缺失，期望 $2）"', source)
        self.assertIn('echo "sha256 校验失败: $1（文件不可读，期望 $2）"', source)
        self.assertIn('[[ "$(compute_sha256 "$1")" == "$2" ]]', source)
        self.assertIn("return 1", source)
        # 三处裸调 + 下载前探测调用,校验点不得悄悄减少。
        self.assertEqual(
            source.count("verify_file "),
            4,
            "fetch-connectors.sh must keep verifying: check_only gate,"
            " pre-download probe, post-download recheck, and pre-install"
            " binary check",
        )

    def test_release_linux_build_jobs_prepare_disk_and_prune_apt(self):
        # 发布构建 job(单盘 hosted runner,x64 开机仅 ~13-14G 可用)必须在
        # 工具链/缓存/依赖落盘之前清理未使用预装 SDK,并在装系统依赖后
        # autoremove + clean;否则冷编译曾把盘写满(ENOSPC,2026-09-13 起)。
        blocks = re.split(
            r"\n  (?=[A-Za-z0-9_-]+:\s*$)", self.release_workflow, flags=re.MULTILINE
        )
        for job_id in ("build-linux-x64", "build-linux-arm64"):
            job = next(
                (b for b in blocks if b.strip().startswith(f"{job_id}:")), None
            )
            self.assertIsNotNone(job, f"release job '{job_id}' not found")
            self.assertIn(
                "python3 scripts/ci-rust-disk.py",
                job,
                f"'{job_id}' must run the disk preparation script",
            )
            self.assertIn(
                "--min-free-gib 24",
                job,
                f"'{job_id}' must fail fast below 24 GiB free",
            )
            # 磁盘准备必须发生在 setup-node 之前(aggressive 档会删
            # /opt/hostedtoolcache,删后 setup-node 会重新下载 Node)。
            self.assertLess(
                job.index("python3 scripts/ci-rust-disk.py"),
                job.index("uses: actions/setup-node"),
                f"'{job_id}' must prepare disk before actions/setup-node",
            )
            self.assertIn("sudo apt-get autoremove -y --purge", job)
            self.assertIn("sudo apt-get clean", job)
        # x64 镜像预装最重(开机可用最少),用 aggressive 档;arm64 镜像轻量
        # 且要保住 setup-node 的工具缓存,只用 base 档。
        x64 = next(b for b in blocks if b.strip().startswith("build-linux-x64:"))
        arm64 = next(b for b in blocks if b.strip().startswith("build-linux-arm64:"))
        self.assertIn("--aggressive", x64)
        self.assertNotIn("--aggressive", arm64)

    def test_audited_redundant_apt_packages_stay_pruned(self):
        # 2026-09-15 探针逐包审计结论(单包模拟安装,安装集合不变即冗余):
        # libgtk-3-dev/libsoup-3.0-dev/libx11-dev/libxi-dev/libxtst-dev 均被
        # libwebkit2gtk-4.1-dev 硬依赖链传递安装,librsvg2-dev 构建不需要
        # (Cargo 无 rsvg crate)。任何 workflow 的安装清单回填它们都会拖慢
        # 依赖安装并占用单盘 runner 的构建磁盘。守卫剥注释后全文扫描:审计
        # 注释允许提及这些名字,但非注释文本出现即拒绝(含多行续行)。
        redundant = (
            "libgtk-3-dev",
            "libsoup-3.0-dev",
            "librsvg2-dev",
            "libx11-dev",
            "libxi-dev",
            "libxtst-dev",
        )
        workflows = sorted(
            list((ROOT / ".github/workflows").glob("*.yml"))
            + list((ROOT / ".github/workflows").glob("*.yaml"))
        )
        self.assertTrue(workflows, "no workflow files found under .github/workflows")
        for workflow in workflows:
            text = _without_yaml_comments(workflow.read_text(encoding="utf-8"))
            for package in redundant:
                self.assertNotIn(
                    package,
                    text,
                    f"{workflow.name}: 审计判定的冗余 apt 包 '{package}' 不得回填"
                    "(由 libwebkit2gtk-4.1-dev 传递安装或构建不需要)",
                )

    def test_all_linux_jobs_pin_the_release_runner_image(self):
        # 镜像版本绝不漂移(单一镜像公约):全部 workflow 的 Linux runner 必须
        # 与 release 构建同基线(ubuntu-22.04 / ubuntu-22.04-arm)。发布二进制
        # 链接构建机的 glibc,测试/校验必须跑在发布同款系统上;镜像升级必须
        # 全仓一次性协调进行(连同 glibc 下限守卫),禁止 ubuntu-latest 等
        # 滚动镜像或个别 job 单独换版。剥掉 YAML 注释再扫:注释里的版本
        # 记述(如迁移说明)不应触发守卫,守卫只管真实 runs-on。
        allowed = {"ubuntu-22.04", "ubuntu-22.04-arm"}
        workflows = sorted(
            list((ROOT / ".github/workflows").glob("*.yml"))
            + list((ROOT / ".github/workflows").glob("*.yaml"))
        )
        self.assertTrue(workflows, "no workflow files found under .github/workflows")
        for workflow in workflows:
            text = _without_yaml_comments(workflow.read_text(encoding="utf-8"))
            self.assertNotIn(
                "ubuntu-latest",
                text,
                f"{workflow.name}: ubuntu-latest 是滚动镜像,违反单一镜像公约,"
                "必须钉到与 release 一致的 ubuntu-22.04",
            )
            for image in sorted(set(re.findall(r"ubuntu-\d+\.\d+(?:-arm)?", text))):
                self.assertIn(
                    image,
                    allowed,
                    f"{workflow.name}: Linux 镜像 '{image}' 偏离发布基线,"
                    "镜像升级必须全仓一次性进行",
                )

    def test_windows_rust_test_cumulative_main_push_is_path_independent(self):
        # Main's Windows regression must remain independent of adjacent diff paths.
        windows_rust_test = _extract_job_block(
            self.pr_workflow, "windows-rust-test"
        )
        # The job-level routing (unconditional push, rust_full/cli_rust PR
        # legs, draft gate, label fallback, and the absence of merge_group)
        # is pinned verbatim by GATE_JOB_LEVEL_IFS["windows-rust-test"]; the
        # asserts below keep the non-if pins.
        # Cold Windows compile plus the lib link check recently died at the
        # 90-minute cap while passing runs already took 85-87 minutes; the
        # 2026-10-01 stable rollover to 1.99.0 invalidated the rust-cache key
        # so PR legs recompiled both workspaces cold and the link step alone
        # consumed 180 minutes (run 36919362322).
        # Job-level line anchor, same discipline as the macos fold pin:
        # a bare substring is satisfied by timeout-minutes: 2400 or by a
        # step-level 240 while the job cap reverts.
        self.assertIn("\n    timeout-minutes: 240\n", windows_rust_test)

        windows_rust_test = _without_yaml_comments(
            _extract_job_block(self.pr_workflow, "windows-rust-test")
        )
        self.assertIn(
            "- name: Windows Rust 全目标检查\n"
            "        if: ${{ matrix.phase == 'all-targets-check' }}",
            windows_rust_test,
        )
        self.assertIn("--all-targets --features dev-tools", windows_rust_test)
        self.assertIn(
            "- name: Windows Rust 单元测试链接检查\n"
            "        if: ${{ matrix.phase == 'regression' }}",
            windows_rust_test,
        )
        # Round-45/53: the link check must compile the benchmark-hooks
        # contract module, or the round-44 filter for
        # windows_attachment_runtime_stays_security_gated matches 0 tests.
        self.assertIn(
            "--lib --no-run --locked --features benchmark-hooks --message-format=json",
            windows_rust_test,
        )

        # Both isolated legs restore one established namespace, but only the
        # regression/main leg may save. This avoids a second cache writer/key.
        self.assertIn("- name: Cargo cache", windows_rust_test)
        self.assertIn("shared-key: windows-rust-test", windows_rust_test)
        self.assertIn(
            "save-if: ${{ matrix.phase == 'regression' && "
            "github.ref == 'refs/heads/main' }}",
            windows_rust_test,
        )
        self.assertEqual(windows_rust_test.count("shared-key:"), 1)
        self.assertNotIn(
            "if: ${{ matrix.phase == 'regression' }}\n"
            "        uses: Swatinem/rust-cache",
            windows_rust_test,
        )
        self.assertIn("WINDOWS_RUST_CACHE", windows_rust_test)
        self.assertIn("WINDOWS_RUST_TIMING", windows_rust_test)

        # The static updater contract is dependency-free and uses the runner's
        # preinstalled Node. Rust checks still must not install frontend dependencies.
        self.assertNotIn("actions/setup-node", windows_rust_test)
        self.assertNotIn("npm --prefix pinvou3-app ci", windows_rust_test)
        self.assertIn(
            "npm --prefix pinvou3-app run test:windows-static-updater",
            windows_rust_test,
        )

        self.assertIn(
            "defaults:\n      run:\n        shell: bash",
            windows_rust_test,
        )
        self.assertIn("Windows 测试 exe 嵌入 Common-Controls v6 清单", windows_rust_test)
        self.assertIn('"-outputresource:$($testExe.FullName);#1"', windows_rust_test)
        self.assertIn('"PINVOU3_TEST_EXE=$testExe" | Out-File', windows_rust_test)
        self.assertIn('test_exe="$(cygpath -u "$PINVOU3_TEST_EXE")"', windows_rust_test)
        regression = windows_rust_test.split(
            "- name: Windows 原子替换状态机回归", maxsplit=1
        )[1]
        self.assertIn('"$test_exe" "$filter" --test-threads=1', regression)
        self.assertNotIn("cargo test", regression)
        self.assertIn(
            "'connector_introspection_guard_matches_complete_names_only'",
            regression,
            "Windows must execute the PowerShell connector-introspection hook regression",
        )
        for filter_name in (
            "platform::filesystem::tests::windows_",
            "platform::paths::tests::managed_python_",
            "artifact_read_recovers_an_occupied_1177_layout_after_release",
            "artifact_writes_never_expose_partial_utf8_to_concurrent_readers",
            "artifact_public_read_",
            "features::memory::tests::topic_migration_",
            "features::updater::platform::windows::tests::",
        ):
            self.assertEqual(regression.count(filter_name), 1)
        self.assertIn("running [1-9][0-9]* tests?", regression)

        self.assert_required_gate_wiring("windows-rust-test", "WINDOWS_RUST_RESULT")

    def test_windows_reader_resume_runs_in_the_nonzero_regression_loop(self):
        windows_job = _without_yaml_comments(
            self.pr_workflow.split("\n  windows-rust-test:", maxsplit=1)[1].split(
                "\n  windows-rustup-repair-test:", maxsplit=1
            )[0]
        )
        regression = windows_job.split(
            "- name: Windows 原子替换状态机回归", maxsplit=1
        )[1]

        def assert_reader_execution(block):
            loop = block.split("for filter in ", maxsplit=1)[1].split(
                "done", maxsplit=1
            )[0]
            filters, commands = loop.split("; do", maxsplit=1)
            self.assertEqual(
                filters.count("'app::commands::remote_control::reader_resume_tests::'"),
                1,
            )
            self.assertIn('"$test_exe" "$filter" --test-threads=1', commands)
            self.assertIn("running [1-9][0-9]* tests?", commands)
            self.assertRegex(commands, r"grep -qE[^\n]+\|\|\s*\{[^}]*exit 1")
            self.assertIn("set -euo pipefail", block)
            self.assertNotIn("continue-on-error", block)

        assert_reader_execution(regression)
        # Static guard negatives: a compile-only or broad/zero-match replacement
        # cannot satisfy this wiring contract. These are not Wry runtime evidence.
        for old, new in (
            ("'app::commands::remote_control::reader_resume_tests::'", "'features::remote_control::'"),
            ('"$test_exe" "$filter" --test-threads=1', "echo compile-only"),
            ("running [1-9][0-9]* tests?", "running [0-9]+ tests?"),
            ("exit 1", "exit 0"),
        ):
            with self.subTest(mutation=old):
                with self.assertRaises(AssertionError):
                    assert_reader_execution(regression.replace(old, new))

    def test_windows_a_resume_projection_executes_with_nonzero_product_filters(self):
        windows = _without_yaml_comments(
            _extract_job_block(self.pr_workflow, "windows-rust-test")
        )
        regression = windows.split(
            "- name: Windows 原子替换状态机回归", maxsplit=1
        )[1].split("\n      - name:", maxsplit=1)[0]
        required = (
            "'features::remote_control::controller::controlling_service::resume_projection_tests::'",
            "'features::remote_control::controller::controlling::tests::'",
            "'features::remote_control::controller::controlling_service::tests::resume_sequence_reopens_business_only_after_controller_resumed'",
        )

        def assert_execution(block):
            loop = block.split("for filter in ", maxsplit=1)[1].split("done", maxsplit=1)[0]
            filters, commands = loop.split("; do", maxsplit=1)
            for prefix in required:
                self.assertEqual(filters.count(prefix), 1)
            self.assertIn('"$test_exe" "$filter" --test-threads=1', commands)
            self.assertIn("running [1-9][0-9]* tests?", commands)
            self.assertRegex(commands, r"grep -qE[^\n]+\|\|\s*\{[^}]*exit 1")
            self.assertIn("set -euo pipefail", block)
            self.assertNotIn("continue-on-error", block)

        assert_execution(regression)
        # Deletion/rename, compile-only execution and a vacuous zero-test guard
        # must each fail this policy; unrelated B filters cannot satisfy it.
        mutations = [(prefix, change) for prefix in required
                     for change in ("", "'renamed_no_matching_tests'")]
        mutations += [
            ('"$test_exe" "$filter" --test-threads=1', "echo compiled-only"),
            ("running [1-9][0-9]* tests?", "running [0-9]+ tests?"),
            ("exit 1", "exit 0"),
        ]
        for old, new in mutations:
            with self.subTest(mutation=(old, new)):
                with self.assertRaises(AssertionError):
                    assert_execution(regression.replace(old, new))

    def test_windows_controller_inbound_runs_in_the_nonzero_regression_loop(self):
        windows_job = _without_yaml_comments(
            self.pr_workflow.split("\n  windows-rust-test:", maxsplit=1)[1].split(
                "\n  windows-rustup-repair-test:", maxsplit=1
            )[0]
        )
        regression = windows_job.split(
            "- name: Windows 原子替换状态机回归", maxsplit=1
        )[1]
        required_filters = (
            "'features::remote_control::controller::controlling_inbound::tests::'",
            "'features::remote_control::controller::controlling_service::inbound_tests::'",
        )

        def assert_inbound_execution(block):
            loop = block.split("for filter in ", maxsplit=1)[1].split(
                "done", maxsplit=1
            )[0]
            filters, commands = loop.split("; do", maxsplit=1)
            for required in required_filters:
                self.assertEqual(filters.count(required), 1)
            self.assertIn('"$test_exe" "$filter" --test-threads=1', commands)
            self.assertIn("running [1-9][0-9]* tests?", commands)
            self.assertRegex(commands, r"grep -qE[^\n]+\|\|\s*\{[^}]*exit 1")
            self.assertIn("set -euo pipefail", block)
            self.assertNotIn("continue-on-error", block)

        assert_inbound_execution(regression)
        # These mutations protect actual behavior selection, not just linking
        # or the presence of an unrelated filter elsewhere in the job.
        mutations = [
            (required, replacement)
            for required in required_filters
            for replacement in ("", "'features::remote_control::'")
        ] + [
            ('"$test_exe" "$filter" --test-threads=1', "echo compile-only"),
            ("running [1-9][0-9]* tests?", "running [0-9]+ tests?"),
            ("exit 1", "exit 0"),
        ]
        for old, new in mutations:
            with self.subTest(mutation=(old, new)):
                with self.assertRaises(AssertionError):
                    assert_inbound_execution(regression.replace(old, new))

    def test_windows_rust_split_matches_the_declared_feature_graph(self):
        with TAURI_MANIFEST.open("rb") as manifest_file:
            manifest = tomllib.load(manifest_file)
        features = manifest["features"]
        self.assertEqual(features["default"], ["local-embed"])
        self.assertEqual(features["dev-tools"], [])
        dump_prompt = next(
            binary for binary in manifest["bin"]
            if binary["name"] == "dump_system_prompt"
        )
        self.assertEqual(dump_prompt["required-features"], ["dev-tools"])

        windows_rust_test = self.pr_workflow.split(
            "\n  windows-rust-test:", maxsplit=1
        )[1].split("\n  windows-rustup-repair-test:", maxsplit=1)[0]
        self.assertIn("--all-targets --features dev-tools", windows_rust_test)
        # Round-45/53: the link check compiles the benchmark-hooks contract
        # module so the round-44 attachment-security filter matches a real
        # test binary instead of silently running 0 tests.
        self.assertIn(
            "--lib --no-run --locked --features benchmark-hooks --message-format=json",
            windows_rust_test,
        )
        self.assertNotIn("--lib --no-run --features dev-tools", windows_rust_test)

    def test_windows_release_powershell_scripts_route_to_native_static_contract(self):
        changes = self.pr_workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  backflow-portability:", maxsplit=1
        )[0]
        route = changes.split("            windows_static_updater:", maxsplit=1)[1].split(
            "            backflow_infra:", maxsplit=1
        )[0]
        paths = _extract_quoted_paths(route)
        for script in (
            "scripts/prepare-windows-release.ps1",
            "scripts/verify-windows-release-online.ps1",
        ):
            self.assertTrue(_matches_paths_filter(script, paths), script)

        job = self.pr_workflow.split(
            "\n  windows-static-updater-contract:", maxsplit=1
        )[1].split("\n  windows-rust-test:", maxsplit=1)[0]
        self.assertIn("runs-on: windows-latest", job)
        self.assertIn("needs.changes.outputs.windows_static_updater == 'true'", job)
        self.assertIn("npm --prefix pinvou3-app run test:windows-static-updater", job)
        required_gate = self.pr_workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("- windows-static-updater-contract", required_gate)
        self.assertIn(
            '"windows-static-updater-contract:$WINDOWS_STATIC_UPDATER_RESULT"',
            required_gate,
        )

    def test_windows_policy_condition_truth_table_and_required_result(self):
        windows_job = self.pr_workflow.split(
            "\n  windows-rust-test:", maxsplit=1
        )[1].split("\n  windows-rustup-repair-test:", maxsplit=1)[0]
        job_expression = re.search(
            r"\n    if: >-\n      \$\{\{(.*?)\n      \}\}",
            windows_job,
            flags=re.DOTALL,
        ).group(1)

        required_gate = self.pr_workflow.split("\n  required-gate:", maxsplit=1)[1]
        required_expression = re.search(
            r"WINDOWS_RUST_REQUIRED_BY_POLICY: \$\{\{ (.*?) \}\}",
            required_gate,
        ).group(1)

        cases = (
            # policy/API/diff failure is full even for a paths-filter-low-risk diff
            ("policy failure ready", "pull_request", False, "full", False, True, False, True, True),
            ("policy failure draft", "pull_request", True, "full", False, True, False, False, False),
            ("incomplete queue entry policy failure", "merge_group", False, "full", False, False, False, True, True),
            ("truncated queue labels policy failure", "merge_group", False, "full", True, True, False, True, True),
            # A provable mixed/non-trusted group keeps ordinary MQ behavior; each PR
            # already took its own ready-stage Windows route.
            ("mixed cumulative group", "merge_group", False, "ordinary", True, True, False, False, False),
            ("non-main low-risk PR", "pull_request", False, "ordinary", False, True, False, False, False),
            ("ordinary high-risk PR", "pull_request", False, "ordinary", True, True, False, True, False),
            ("ci full label", "pull_request", False, "ordinary", False, True, True, True, False),
            ("Windows-native non-Rust", "pull_request", False, "windows_full", False, False, False, True, True),
            ("portable Rust ready", "pull_request", False, "portable_rust", True, True, False, False, False),
            ("pure release ready", "pull_request", False, "release", True, True, False, False, False),
            ("main cumulative", "push", False, "full", False, False, False, True, False),
        )
        for (
            name,
            event_name,
            draft,
            mode,
            rust_full,
            rust_code,
            full_label,
            expected_job,
            expected_policy_required,
        ) in cases:
            with self.subTest(name=name):
                values = dict(
                    event_name=event_name,
                    draft=draft,
                    mode=mode,
                    rust_full=rust_full,
                    rust_code=rust_code,
                    full_label=full_label,
                )
                self.assertEqual(
                    _evaluate_github_condition(job_expression, **values),
                    expected_job,
                )
                policy_required = _evaluate_github_condition(
                    required_expression, **values
                )
                self.assertEqual(policy_required, expected_policy_required)
                # Replay the required-gate shell contract: a policy-required
                # Windows leg accepts only success; without the requirement the
                # per-item loop accepts success|skipped and rejects the rest.
                accepted = [
                    result
                    for result in ("success", "skipped", "failure", "cancelled")
                    if result == "success"
                    or (result == "skipped" and not policy_required)
                ]
                self.assertEqual(
                    accepted,
                    ["success"] if policy_required else ["success", "skipped"],
                )

    def test_pure_release_backflow_runs_contract_only_and_skipped_jobs_are_accepted(self):
        for start, end in (
            ("frontend-test", "relay-test"),
            ("knowledge-rust", "rust-lint"),
            ("rust-lint", "rust-test"),
            ("windows-rustup-repair-test", "windows-codex-runtime-test"),
            ("windows-codex-runtime-test", "windows-runtime-test"),
            ("windows-runtime-test", "macos-codex-runtime-test"),
            ("macos-codex-runtime-test", "required-gate"),
        ):
            job = self.pr_workflow.split(f"\n  {start}:", maxsplit=1)[1].split(
                f"\n  {end}:", maxsplit=1
            )[0]
            self.assertIn("needs.changes.outputs.pure_release_backflow != 'true'", job)

        release_contract = self.pr_workflow.split(
            "\n  release-contract-test:", maxsplit=1
        )[1].split("\n  knowledge-rust:", maxsplit=1)[0]
        self.assertNotIn("pure_release_backflow", release_contract)

        required_gate = self.pr_workflow.split("\n  required-gate:", maxsplit=1)[1]
        self.assertIn("success|skipped", required_gate)
        self.assertIn("WINDOWS_RUST_REQUIRED_BY_POLICY", required_gate)
        self.assertIn('"$WINDOWS_RUST_RESULT" != success', required_gate)
        self.assertIn("回流策略 $BACKFLOW_MODE 要求完整 Windows Rust", required_gate)
    def test_macos_rust_check_is_wired_into_required_gate(self):
        # Review finding: the new native macOS leg must satisfy the same
        # three-wiring rule as windows-rust-test (needs entry, env backfill,
        # summary-loop entry) -- otherwise removing the job keeps CI green
        # while the gate silently degrades.
        job_body = self.pr_workflow.split("\n  macos-rust-check:", maxsplit=1)[1]
        job = re.split(r"\n  [a-zA-Z]", job_body, maxsplit=1)[0]
        self.assertIn("needs: changes", job)
        self.assertIn("MACOS_RUST_CHECK_RESULT", self.pr_workflow)
        required_gate = self._required_gate()
        self.assertIn("- macos-rust-check", required_gate)
        self.assertIn("MACOS_RUST_CHECK_RESULT", required_gate)
        self.assertIn('"macos-rust-check:$MACOS_RUST_CHECK_RESULT"', required_gate)

    def test_windows_browser_wrapper_lifecycle_runs_in_required_native_job(self):
        changes = _changes_block(self.pr_workflow)
        windows_codex_filter = changes.split("windows_codex:", maxsplit=1)[1]
        self.assertIn(
            "resources/common/bundle/mcp-servers/browser-*",
            windows_codex_filter,
        )
        self.assertIn(
            "browser_wrapper_windows_lifecycle.test.mjs",
            windows_codex_filter,
        )
        self.assertIn("browser_wrapper_lazy.test.mjs", windows_codex_filter)

        windows_job = self.pr_workflow.split(
            "\n  windows-codex-runtime-test:", maxsplit=1
        )[1].split("\n  macos-codex-runtime-test:", maxsplit=1)[0]
        self.assertIn("needs.changes.outputs.windows_codex == 'true'", windows_job)
        self.assertIn("runs-on: windows-latest", windows_job)
        self.assertIn("Windows browser wrapper lifecycle regression", windows_job)
        self.assertIn(
            "node --test pinvou3-app/tests/browser_wrapper_windows_lifecycle.test.mjs",
            windows_job,
        )
        self.assertIn(
            "node pinvou3-app/tests/browser_wrapper_lazy.test.mjs",
            windows_job,
        )
        self.assertIn('PINVOU3_TEST_BROWSER_NO_HOST: "1"', windows_job)

        required_gate = self._required_gate()
        self.assertIn("- windows-codex-runtime-test", required_gate)
        self.assertIn("WINDOWS_CODEX_RESULT", required_gate)

    def test_windows_python_dependency_contract_runs_in_required_native_job(self):
        changes = _changes_block(self.pr_workflow)
        # Anchor on the 12-space-indented dorny filter key (not the 8-space outputs
        # mapping) and capture only the entry lines of that one filter group, so
        # moving the ps1 route into another filter fails this assertion.
        windows_codex_filter = _dorny_filter_group(changes, "windows_codex")
        self.assertIn(
            "windows_python_dependency_contract.ps1",
            windows_codex_filter,
        )
        # The VC++ temp-preflight pins inside windows_runtime_packaging_contract.test.js
        # target src-tauri/packaging/windows/nsis/vcredist-temp-preflight.ps1, so edits
        # to that file must trigger the only job that runs the pins.
        self.assertIn(
            "pinvou3-app/src-tauri/packaging/windows/nsis/**",
            windows_codex_filter,
        )

        windows_job = self.pr_workflow.split(
            "\n  windows-codex-runtime-test:", maxsplit=1
        )[1].split("\n  macos-codex-runtime-test:", maxsplit=1)[0]
        self.assertIn(
            "npm --prefix pinvou3-app run test:windows-runtime",
            windows_job,
        )

        required_gate = self._required_gate()
        self.assertIn("- windows-codex-runtime-test", required_gate)

    def test_windows_rustup_repair_runs_in_required_native_job(self):
        changes = _changes_block(self.pr_workflow)
        self.assertIn(
            "windows_rustup_repair: ${{ steps.filter.outputs.windows_rustup_repair }}",
            changes,
        )
        repair_filter = _dorny_filter_group(changes, "windows_rustup_repair")
        for trigger in (
            ".github/workflows/pr-check.yml",
            "pinvou3-app/scripts/ci/**",
            "pinvou3-app/scripts/tauri/build.js",
            "pinvou3-app/tests/windows_rustup_repair_smoke.ps1",
            "pinvou3-app/tests/windows_rust_toolchain_contract.test.js",
            "pinvou3-app/src-tauri/rust-toolchain.toml",
            "pinvou3-app/package.json",
        ):
            self.assertIn(trigger, repair_filter)

        job_body = self.pr_workflow.split(
            "\n  windows-rustup-repair-test:", maxsplit=1
        )[1]
        job = re.split(r"\n  [a-zA-Z]", job_body, maxsplit=1)[0]
        self.assertIn("needs: changes", job)
        self.assertIn(
            "needs.changes.outputs.windows_rustup_repair == 'true'", job
        )
        self.assertIn("runs-on: windows-latest", job)
        self.assertIn("npm --prefix pinvou3-app run test:windows-rustup-repair", job)
        # scripts/ci/** is in no node-test filter, so this job is the only
        # gate that pins ensure-rust-toolchain.ps1 through the node contract
        # test. Dropping the step would silently unpin the repair engine.
        self.assertIn(
            "node --test pinvou3-app/tests/windows_rust_toolchain_contract.test.js",
            job,
        )
        # The contract test must run before the smoke: it fails in seconds on
        # engine drift, while the smoke pays a real toolchain download first.
        self.assertLess(
            job.index(
                "node --test pinvou3-app/tests/windows_rust_toolchain_contract.test.js"
            ),
            job.index("npm --prefix pinvou3-app run test:windows-rustup-repair"),
        )

        # Same three-wiring rule as the other native legs: needs entry, env
        # backfill, and summary-loop entry.
        self.assert_required_gate_wiring(
            "windows-rustup-repair-test", "WINDOWS_RUSTUP_REPAIR_RESULT"
        )

    def test_release_contract_runs_for_ready_pr_queue_and_main(self):
        changes = _without_yaml_comments(_changes_block(self.pr_workflow))
        release_contract_paths = changes.split(
            "            release_contract:", maxsplit=1
        )[1].split("            l1:", maxsplit=1)[0]
        self.assertIn("- 'pinvou3-app/src-tauri/resources/**'", release_contract_paths)
        self.assertIn(
            "- 'pinvou3-app/tests/knowledge_host_packaging.test.mjs'",
            release_contract_paths,
        )
        # tauri_effective_config.test.js parses this script's --target dispatch to
        # pin the cross-build Node architecture mapping, and it runs inside
        # release-contract-test; without this entry a PR touching only the bridge
        # script would silently skip that contract gate.
        self.assertIn(
            "- 'pinvou3-app/scripts/prepare-codex-bridge-runtime.sh'",
            release_contract_paths,
        )
        # Keep the l1 section boundary load-bearing: if the split anchor stops
        # matching, this slice would silently grow and make the assertions above
        # vacuous instead of exposing the workflow drift.
        self.assertNotIn(
            "- 'pinvou3-app/src-tauri/tests/l1_dialog_harness.rs'",
            release_contract_paths,
        )

        release_contract = _without_yaml_comments(
            _extract_job_block(self.pr_workflow, "release-contract-test")
        )
        self.assertIn(
            "needs.changes.outputs.release_contract == 'true'",
            release_contract,
        )
        self.assertNotIn("github.event_name != 'merge_group'", release_contract)
        self.assertIn("github.event.pull_request.draft == false", release_contract)
        self.assertIn(
            "npm --prefix pinvou3-app run test:knowledge-host-packaging",
            release_contract,
        )

        for start, end in (
            ("windows-codex-runtime-test", "windows-runtime-test"),
            ("windows-runtime-test", "macos-codex-runtime-test"),
            ("macos-codex-runtime-test", "required-gate"),
        ):
            block = self.workflow.split(f"\n  {start}:", maxsplit=1)[1].split(
                f"\n  {end}:", maxsplit=1
            )[0]
            self.assertIn(
                "github.event.pull_request.draft == false",
                block,
                f"{start} 必须只在非 draft 的 Ready PR 上运行",
            )

    def _assert_macos_deployment_regression(self, workflow):
        # This coverage belongs to the existing native job; it must not become
        # an echo, syntax-only check, filtered zero-match run or npm install.
        block = workflow.split("\n  macos-rust-check:", maxsplit=1)[1].split(
            "\n  windows-codex-runtime-test:", maxsplit=1
        )[0]
        body = _without_yaml_comments(block)
        self.assertIn("runs-on: macos-15", body)
        self.assertIn("uses: actions/setup-node@v7", body)
        self.assertIn("node-version: '24'", body)
        command = 'node --test --test-reporter=tap remote-control-relay/test/deployment-incidents.test.js | tee "$hardening_log"'
        step = body.split("      - name: macOS temporary-root hardening and deployment regression\n", maxsplit=1)[1].split(
            "\n      - name:", maxsplit=1
        )[0]
        self.assertRegex(step, r"(?m)^\s*" + re.escape(command) + r"$")
        self.assertIn("set -euo pipefail", step)
        for assertion in (
            "grep -qE '^# tests [1-9][0-9]*$'",
            "grep -qx '# fail 0'",
            "grep -qx '# skipped 0'",
            "grep -qx '# todo 0'",
            "grep -qE '^ok [1-9][0-9]* - actual Darwin system temporary roots accept hardening input and reject operator links$'",
        ):
            self.assertIn(assertion + ' "$hardening_log"', step)
        for bypass in ("|| true", "continue-on-error:", "if:", "--test-name-pattern"):
            self.assertNotIn(bypass, step)
        self.assertLess(body.index("uses: actions/setup-node@v7"), body.index(command))
        self.assertLess(body.index(command), body.index("name: macOS Rust all-targets check"))

    def test_macos_deployment_regression_uses_explicit_node_and_actual_test_file(self):
        self._assert_macos_deployment_regression(self.workflow)

    def test_macos_deployment_regression_rejects_execution_and_summary_bypasses(self):
        for old, new in (
            ("node --test --test-reporter=tap remote-control-relay/test/deployment-incidents.test.js", "node --check remote-control-relay/test/deployment-incidents.test.js"),
            ("node --test --test-reporter=tap remote-control-relay/test/deployment-incidents.test.js", "node --test --test-name-pattern=absent --test-reporter=tap remote-control-relay/test/deployment-incidents.test.js"),
            ('| tee "$hardening_log"', '| tee "$hardening_log" || true'),
            ("grep -qE '^# tests [1-9][0-9]*$'", "grep -qE '^# tests [0-9]*$'"),
            ("grep -qx '# skipped 0'", "echo '# skipped 0'"),
            ("grep -qx '# fail 0'", "echo '# fail 0'"),
            ("grep -qx '# todo 0'", "echo '# todo 0'"),
            ("uses: actions/setup-node@v7", "# removed setup"),
        ):
            with self.subTest(mutation=new):
                with self.assertRaises((AssertionError, IndexError)):
                    self._assert_macos_deployment_regression(self.workflow.replace(old, new))

    def test_mac_bundle_smoke_is_gated_on_bundle_chain_paths(self):
        # mac-build.yml's bundle_chain filter (before its 2026-10 fold into
        # pr-check's macos-rust-check) decided when to append the universal
        # bundle smoke. After the fold the filter moved into pr-check's
        # changes job, and the smoke step runs only on push with a
        # bundle_chain hit; this test pins: the filter entries are complete
        # (one missing = packaging-chain changes silently skip the smoke),
        # the smoke step consumes that output, and the PR/Queue side does not
        # run it (the PR side gets the lightweight contract from
        # release-contract-test, and a VERSION bump gets full dmg builds from
        # release-packages). VERSION is deliberately absent from the filter:
        # a real version-sync commit always touches
        # tauri.conf.json/package.json, entering bundle_chain through them.
        changes = _without_yaml_comments(_changes_block(self.pr_workflow))
        self.assertIn("bundle_chain:", changes)
        bundle_chain_paths = _extract_quoted_paths(
            changes.split("            bundle_chain:", maxsplit=1)[1]
        )
        self.assertTrue(bundle_chain_paths, "bundle_chain parsed to an empty path list")
        for entry in (
            "pinvou3-app/scripts/tauri/**",
            "pinvou3-app/src-tauri/tauri.conf.json",
            "pinvou3-app/src-tauri/config/**",
            "pinvou3-app/src-tauri/packaging/**",
            "pinvou3-app/src-tauri/resources/**",
            "pinvou3-app/package.json",
            "pinvou3-app/package-lock.json",
            "pinvou3-app/vite.config.mjs",
        ):
            self.assertIn(
                entry,
                bundle_chain_paths,
                f"bundle_chain is missing the packaging-chain entry; this path "
                f"changing would silently skip the bundle smoke: {entry}",
            )

        macos_job = _extract_job_block(self.pr_workflow, "macos-rust-check")
        smoke_step = _without_yaml_comments(
            macos_job.split(
                "- name: Tauri bundle smoke", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn(
            "github.event_name == 'push' && needs.changes.outputs.bundle_chain == 'true'",
            smoke_step,
            "the bundle smoke must run only on push and only for packaging-chain changes",
        )
        self.assertIn(
            "node scripts/tauri/build.js build --target universal-apple-darwin",
            smoke_step,
        )
        self.assertNotIn(
            "continue-on-error",
            smoke_step,
            "the universal bundle smoke must be able to fail main "
            "(a broken macos packaging chain must be red)",
        )

    def test_macos_rust_check_routes_by_rust_filters_not_frontend_paths(self):
        # PR/MQ-side routing is pinned verbatim by
        # GATE_JOB_LEVEL_IFS["macos-rust-check"]: the job's `if:` must not
        # consume the frontend/pet outputs (it is a rust gate; frontend
        # changes are validated by frontend-test), main push stays
        # unconditional (the cumulative main-push contract, frontend-only
        # pushes included), and the PR branch keeps the rust_full/cli_rust
        # routing. This test keeps what the job-level `if:` cannot cover:
        # the bundle_chain filter must not treat pure-frontend src/** as a
        # packaging-chain change — but package.json/package-lock.json must
        # stay bundle_chain triggers (the lockfile affects the build).
        # Formerly pinned the standalone mac-build.yml trigger; the workflow
        # was folded into pr-check's macos-rust-check in 2026-10.
        changes = _without_yaml_comments(_changes_block(self.pr_workflow))
        bundle_tail = changes.split("            bundle_chain:", maxsplit=1)[1]
        # bundle_chain is the last filter group and the extraction below has
        # no lower bound: a group appended after it would leak into the
        # extracted paths (the exact-entry pins would keep passing until a
        # negation false-fails). Pin the boundary instead.
        leaked_groups = [
            line
            for line in bundle_tail.splitlines()[1:]
            if re.fullmatch(r"            [A-Za-z0-9_-]+:", line)
        ]
        self.assertEqual(
            [], leaked_groups,
            "a new filter group after bundle_chain must re-bound this extraction",
        )
        bundle_chain_paths = _extract_quoted_paths(bundle_tail)
        self.assertNotIn(
            "pinvou3-app/src/**",
            bundle_chain_paths,
            "pure-frontend src/** must not enter bundle_chain (avoids needless native builds)",
        )
        self.assertIn("pinvou3-app/package.json", bundle_chain_paths)
        self.assertIn("pinvou3-app/package-lock.json", bundle_chain_paths)

    def test_macos_native_regression_legs_survived_the_mac_build_fold(self):
        # mac-build.yml was deleted in 2026-10 and its push-only coverage was
        # folded into macos-rust-check. Its old failure mode was silent: the
        # workflow kept "running" (cancel-failing) for weeks with zero
        # coverage. Pin each folded leg so removing one cannot go unnoticed.
        macos_job = _extract_job_block(self.pr_workflow, "macos-rust-check")
        # mac-build died inside its own 50-minute cap; the fold replaced it
        # with 180 and the 2026-10-15 re-scoping (release-fast smoke gated to
        # bundle_chain/rust_full/cli_rust pushes + release artifacts pruned
        # from the cache save) raised it to 240: the push legs had become the
        # merge-throughput bottleneck (p90 at 99% of the 180 cap, three
        # timeout kills in 13 days). Reverting to the default (360) or
        # deleting the cap burns a hung runner for hours — the exact waste
        # this PR removes. Anchored on the 4-space job-level line: a bare
        # substring is satisfied by 2400 or by a step-level 240 while the
        # job cap reverts.
        self.assertIn(
            "\n    timeout-minutes: 240\n", macos_job,
            "the macos-rust-check job cap must stay 240 at job level",
        )

        # Full native lib regression: push-only, serial threads, locked.
        # Comment-stripped: an if-line deleted but kept "alive" in a YAML
        # comment must not satisfy the gate.
        test_step = _without_yaml_comments(
            macos_job.split(
                "- name: macOS full lib tests", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn(
            "if: ${{ github.event_name == 'push' }}", test_step
        )
        self.assertIn(
            "cargo test --lib --features benchmark-hooks --locked -- --test-threads=1",
            test_step,
        )
        # Same silent-no-op guard as the PR leg's computer_use run: a
        # filterless suite that somehow runs zero tests must fail the push.
        self.assertIn("running [1-9][0-9]* tests?", test_step)

        # The universal build needs both darwin targets installed; dropping
        # this step resurfaces as MODULE/target errors on the first
        # bundle_chain push, weeks after the fold (same class as the npm
        # provisioning miss the review caught). The raw slice is computed
        # once here; the bundle_chain gate pin further down reuses it.
        dual_target_step = macos_job.split(
            "- name: Install both targets (universal bundle smoke, push only)",
            maxsplit=1,
        )[1].split("\n      - name:", maxsplit=1)[0]
        targets_step = _without_yaml_comments(dual_target_step)
        self.assertIn("github.event_name == 'push'", targets_step)
        self.assertIn(
            "rustup target add aarch64-apple-darwin x86_64-apple-darwin",
            targets_step,
        )

        # The release-fast compile smoke and the verify script stay push-only.
        # Since 2026-10-15 the smoke additionally requires one of the
        # bundle_chain/rust_full/cli_rust/knowledge_rust buckets:
        # bundle_chain is load-bearing (the bundle smoke below reuses this
        # step's aarch64 release-fast artifacts), rust_full/cli_rust mirror
        # the job's own PR-leg Rust routing, and knowledge_rust closes the
        # path-dep hole (src-tauri path-depends on pinvou-knowledge, whose
        # sources never match rust_full) — without them the smoke either
        # runs on every push (the merge-throughput bottleneck this
        # re-scoping removed) or stops running on merges that can break the
        # release-path compile. Extractions anchor on the EXACT pinned step
        # name line via _step_named: a decoy step whose name merely contains
        # a pinned name cannot capture the slice, and gutting or renaming
        # the real step fails closed (see PINNED_GATE_STEP_NAMES for the
        # paired line-exact uniqueness pin).
        release_fast = _without_yaml_comments(
            _step_named(macos_job, "Cargo build (release-fast smoke, push only)")
        )
        self.assertIn("github.event_name == 'push'", release_fast)
        # Pin the WHOLE gate expression, not its parts: bare-substring
        # bucket pins stay green when a never-matching conjunct (or a
        # typo'd ref pin) is appended to the folded if — the smoke would
        # silently stop running while every bucket substring is still
        # present. Normalize the value's whitespace and require the exact
        # token stream, accepting both the folded and the inline spelling
        # so a pure YAML reformat does not false-fail the pin.
        smoke_if = _step_if_tokens(release_fast)
        # Parens are padded before comparison: the folded scalar and an
        # inline one-liner tokenize parens differently while meaning the
        # same thing, and the exactness that matters is the conjunction
        # structure, not paren spacing.
        self.assertEqual(
            " ".join(
                smoke_if.replace("(", " ( ").replace(")", " ) ").split()
            ),
            "${{ github.event_name == 'push' && ( "
            "needs.changes.outputs.bundle_chain == 'true' || "
            "needs.changes.outputs.rust_full == 'true' || "
            "needs.changes.outputs.cli_rust == 'true' || "
            "needs.changes.outputs.knowledge_rust == 'true' ) }}",
            "the release-fast smoke gate must be exactly push && the four "
            "buckets — anything appended here silently skips the smoke",
        )
        self.assertIn(
            "cargo build --profile release-fast --target aarch64-apple-darwin --lib",
            release_fast,
        )
        # The release-profile artifacts must be pruned from the cache save:
        # they were the bulk of the namespace's measured 3.1GB standing
        # footprint (30% of the whole 10GB repo quota) and dead weight for
        # every PR leg. The prune must run under always() — cache-on-failure
        # saves failed runs too, so a failed run must not re-save the stale
        # release dirs. target/release-fast is the host-side dir cargo pairs
        # with every --target build; rust-cache keeps external-package units
        # there, so it needs pruning with the per-target dirs.
        prune_step = _without_yaml_comments(
            _step_named(
                macos_job,
                "Prune release artifacts before the cache save (push only)",
            )
        )
        # Exact-line if pin: a bare assertIn stays green when a
        # never-matching conjunct is appended (the prune silently stops
        # running and the release dirs re-land in the saved cache).
        self.assertIn(
            "if: ${{ always() && github.event_name == 'push' }}",
            [line.strip() for line in prune_step.splitlines()],
        )
        # Pin the deletion itself, not just the directory names: replacing
        # rm -rf with a no-op while keeping the four target strings would
        # keep every name pin green.
        prune_run = " ".join(
            prune_step.split("run: >-", 1)[1].split()
        )
        self.assertEqual(
            prune_run,
            "rm -rf target/release-fast target/aarch64-apple-darwin/release-fast "
            "target/x86_64-apple-darwin/release-fast target/universal-apple-darwin",
        )
        # The rm runs relative to the src-tauri workspace: repointing
        # working-directory (e.g. to ".") makes every target a no-op match
        # from the repo root while the command text stays byte-identical,
        # so the directory the command runs in is pinned too.
        self.assertIn(
            "working-directory: pinvou3-app/src-tauri",
            [line.strip() for line in prune_step.splitlines()],
        )
        # The prune only lands in the saved cache when the key generation
        # rotates: rust-cache skips the post-job save on an exact
        # primary-key hit, so a content-only prune can never replace the
        # pre-rotation fat entry. The -v2 suffix is that one-time rotation.
        # Comment-stripped: the rotation is load-bearing for the prune, so
        # a commented-out copy of the line must not satisfy the pin.
        stripped_macos = _without_yaml_comments(macos_job)
        self.assertEqual(stripped_macos.count("shared-key:"), 1)
        # Line-exact membership, not a substring: assertIn would also be
        # satisfied by a v22-style stretch (macos-rust-check-v22), which
        # rust-cache would mint under the v0-rust-macos-rust-check-v22-
        # series — a prefix janitor rule A3 never matches, silently
        # disarming the fat-save insurance for the prune invariant.
        self.assertIn(
            "shared-key: macos-rust-check-v2",
            [line.strip() for line in stripped_macos.splitlines()],
            "the rotation key must be the exact live line "
            "shared-key: macos-rust-check-v2",
        )
        verify_step = _without_yaml_comments(
            _step_named(
                macos_job,
                "Verify script (plist/usage-key/bundle targets checks, push only)",
            )
        )
        self.assertIn(
            "if: ${{ always() && github.event_name == 'push' }}",
            [line.strip() for line in verify_step.splitlines()],
        )
        self.assertIn("./scripts/run-mac-verify.sh --skip-test", verify_step)
        # The prune must run AFTER the verify step: run-mac-verify §7's
        # universal-bundle checks hard-fail on plist/ATS/locale/lipo defects
        # only when the bundle smoke's artifact exists (warn-only otherwise),
        # so a prune before verify silently disables them on every
        # bundle_chain push. The order reads the exact step-name lines: a
        # decoy step whose name merely contains a pinned name neither
        # satisfies nor disturbs the lookup, and a missing pinned name
        # fails closed.
        step_names = [name for name, _ in _steps_of(macos_job)]
        self.assertLess(
            step_names.index(
                "Verify script (plist/usage-key/bundle targets checks, push only)"
            ),
            step_names.index(
                "Prune release artifacts before the cache save (push only)"
            ),
        )

        # The computer_use filtered run yields to the full leg on push (the
        # full suite executes the same tests) and keeps guarding PR legs.
        computer_use = _without_yaml_comments(
            macos_job.split(
                "- name: macOS computer_use unit tests", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn("github.event_name != 'push'", computer_use)

        # The bundle smoke consumes node_modules (build.js resolves
        # @tauri-apps/cli from it and the beforeBuildCommand runs vite), so the
        # push-gated Node.js setup + npm ci steps must survive alongside it.
        # The 2026-10 review round caught the fold initially dropping them:
        # the first bundle_chain push would have died MODULE_NOT_FOUND while
        # every lock test stayed green.
        node_step = _without_yaml_comments(
            macos_job.split(
                "- name: Node.js (universal bundle smoke, push only)", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn(
            "github.event_name == 'push' && needs.changes.outputs.bundle_chain == 'true'",
            node_step,
            "node setup is only consumed by the bundle_chain-gated smoke; "
            "gating it the same way stops paying npm ci on unrelated pushes",
        )
        self.assertIn("node-version: '24'", node_step)
        self.assertIn(
            "cache-dependency-path: pinvou3-app/package-lock.json", node_step
        )
        npm_step = _without_yaml_comments(
            macos_job.split(
                "- name: Install frontend deps (universal bundle smoke, push only)",
                maxsplit=1,
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn(
            "github.event_name == 'push' && needs.changes.outputs.bundle_chain == 'true'",
            npm_step,
        )
        self.assertIn("npm ci", npm_step)
        # The dual rustup targets feed the same universal build (lipo needs
        # both arches); they are smoke-only provisioning too, so they carry
        # the identical gate — otherwise unrelated pushes pay rustup while
        # node/npm skip. `dual_target_step` is the same step's raw slice,
        # already extracted above for the darwin-targets pin.
        self.assertIn(
            "github.event_name == 'push' && needs.changes.outputs.bundle_chain == 'true'",
            dual_target_step,
        )
        self.assertIn(
            "rustup target add aarch64-apple-darwin x86_64-apple-darwin",
            dual_target_step,
        )
        # Provisioning must precede the smoke step in the job body.
        self.assertLess(
            macos_job.index("- name: Node.js (universal bundle smoke, push only)"),
            macos_job.index("- name: Tauri bundle smoke"),
        )

        # Deployment-target parity with the release job.
        self.assertIn('MACOSX_DEPLOYMENT_TARGET: "11.0"', macos_job)

    def test_cargo_deny_installs_stay_version_pinned(self):
        # cargo-deny is a hard dependency gate on two jobs (knowledge-rust
        # and rust-lint). install-action defaults to the tool's latest
        # release when the version is unversioned, so an upstream CLI or
        # lint-semantics change would flip a hard gate nondeterministically
        # (same rationale as the cargo-shear pin). The two call sites keep
        # in sync by comment only, so pin the exact set of live cargo-deny
        # install lines across ALL workflow files; the value catcher is a
        # regex, so quoted or oddly-spelled values are still collected and
        # then fail the exact-set comparison: every spelling other than the
        # pinned version — unversioned, a drifted patch (0.20.21 passes a
        # count+prefix pin), a quoted value, an extra site in another
        # workflow — fails.
        deny_lines = []
        for path in sorted((ROOT / ".github/workflows").glob("*.yml")):
            workflow = _without_yaml_comments(path.read_text(encoding="utf-8"))
            for line in workflow.splitlines():
                match = re.match(r"^\s*tool:\s*(.*cargo-deny.*)$", line)
                if match:
                    deny_lines.append((path.name, match.group(1).strip()))
        self.assertEqual(
            deny_lines,
            [("pr-check.yml", "cargo-deny@0.20.2")] * 2,
            "exactly the knowledge-rust and rust-lint call sites must "
            "install cargo-deny, both at exactly the pinned version, "
            "and no other workflow may install it at all",
        )

    def test_macos_lld_linker_wiring_is_pinned(self):
        # The mac leg's lld linker wiring is easy to lose silently: a dropped
        # env line, a job-level leak into the release legs, or a replaced
        # RUSTFLAGS write all fail no build. The probe cannot self-validate
        # these static pieces, so pin them:
        # - the linker env applied per step to exactly the three
        #   dev-profile test legs (the linux leg pins its own
        #   RUSTFLAGS/DEV_DEBUG exactly; mirror that here),
        # - the job-level env staying free of the linker: the folded
        #   push-only release-fast/bundle legs ship artifacts and are
        #   outside the validated scope, so they must keep default Apple
        #   ld linking,
        # - the probe emitting a step output instead of GITHUB_ENV, so
        #   nothing downstream inherits the probed linker by default,
        # - no global RUSTFLAGS write: the obsolete strip workaround is
        #   removed with the pinned toolchain fix, and a global write could
        #   re-propagate the probe's flags onto the release legs,
        # - the probe staying before the cache step, so a toolchain without
        #   a usable lld fails in seconds ahead of any cache restore or
        #   build work (cargo fingerprints inside target/, not the cache
        #   key, keep the lld-built artifacts consistent).
        macos_job = _extract_job_block(self.pr_workflow, "macos-rust-check")

        # No linker env at job level (comments excluded: they document
        # the scoping and legitimately name the variables).
        job_env = _without_yaml_comments(
            macos_job.split("\n    env:", maxsplit=1)[1].split(
                "\n    steps:", maxsplit=1
            )[0]
        )
        self.assertNotIn("CARGO_PROFILE_DEV_LTO", job_env)
        self.assertNotIn("RUSTFLAGS", job_env)

        # Per-step application to the three dev-profile test legs, identical
        # across them so target/debug artifacts stay incrementally reusable.
        test_leg_names = (
            "- name: macOS Rust all-targets check",
            "- name: macOS computer_use unit tests",
            "- name: macOS full lib tests (native regression, push only)",
        )
        for step_name in test_leg_names:
            body = macos_job.split(step_name, maxsplit=1)[1].split(
                "\n      - name:", maxsplit=1
            )[0]
            self.assertIn('CARGO_PROFILE_DEV_LTO: "thin"', body, step_name)
            self.assertIn(
                "RUSTFLAGS: ${{ steps.mac_lld_probe.outputs.flags }}",
                body,
                step_name,
            )

        # The probe emits a step output, not GITHUB_ENV: nothing between the
        # probe and the consuming test steps — nor the release legs below —
        # may inherit the probed linker silently.
        probe_step = macos_job.split(
            "- name: Probe and export the macOS lld link flags", maxsplit=1
        )[1].split("\n      - name:", maxsplit=1)[0]
        self.assertIn("id: mac_lld_probe", probe_step)
        self.assertIn('echo "flags=$flags" >> "$GITHUB_OUTPUT"', probe_step)
        self.assertIn('echo "probed RUSTFLAGS=$flags"', probe_step)
        self.assertNotIn("$GITHUB_ENV", probe_step)
        self.assertLess(
            macos_job.index("- name: Probe and export the macOS lld link flags"),
            macos_job.index("uses: Swatinem/rust-cache@v2"),
        )

        # Release builds use the profile's strip policy now that the pinned
        # toolchain includes the Mach-O fix. No step may export RUSTFLAGS
        # globally and accidentally carry the dev linker into those builds.
        self.assertNotIn("- name: macOS 27+ strip workaround", macos_job)
        for line in _without_yaml_comments(macos_job).splitlines():
            if "GITHUB_ENV" in line:
                self.assertNotIn("RUSTFLAGS", line)

        # The push-only release legs carry no linker env of their own.
        for step_name in (
            "- name: Cargo build (release-fast",
            "- name: Tauri bundle smoke",
        ):
            body = macos_job.split(step_name, maxsplit=1)[1].split(
                "\n      - name:", maxsplit=1
            )[0]
            self.assertNotIn("RUSTFLAGS", body, step_name)
            self.assertNotIn("CARGO_PROFILE_DEV_LTO", body, step_name)

    def test_main_rust_caches_save_on_failure_and_rust_test_keeps_targets(self):
        # cache-on-failure keeps one failed main run from stranding a
        # namespace cold — mac-build's exact death loop (evicted once, then
        # save-on-success-only kept it cold forever). Saves stay main-only
        # (rust-cache's post step also requires save-if), so PR runs never
        # write caches. rust-test's target cache stays re-enabled: the
        # 2026-08-29/30 restored-target runner deaths were root-caused to
        # runner RAM and absorbed by the zram+swap layers (maintainer
        # ruling, recorded at the cache step) — dropping targets again is a
        # policy change, not cleanup, and must revisit this pin.
        for job_name, end_marker in (
            ("rust-test", "\n  cli-test:"),
            ("windows-rust-test", "\n  macos-rust-check:"),
            ("macos-rust-check", "\n  windows-codex-runtime-test:"),
            # Round-49 review: macos-cli-check joined the flag in round-48
            # (same main-only namespace death-loop exposure) but not this
            # pin — the flag could be dropped with the suite green.
            ("macos-cli-check", "\n  required-gate:"),
            # 2026-10 audit followups (PR #745): knowledge-rust/rust-lint/
            # cli-test joined the flag when their main-push unconditional
            # runs landed — same "one failed main push drops the namespace
            # fully cold" exposure; pin them so they cannot rot the same way.
            ("knowledge-rust", "\n  rust-lint:"),
            ("rust-lint", "\n  rust-test:"),
            ("cli-test", "\n  cli-lint:"),
        ):
            job = self.pr_workflow.split(
                f"\n  {job_name}:", maxsplit=1
            )[1].split(end_marker, maxsplit=1)[0]
            # Official's rust-test job restores the lint warm cache
            # (rust-lint-v2, save-if: false) before its own Cargo test cache
            # step; anchor on the step name so the lint restore is not
            # mistaken for the save-on-failure cache.
            job_scope = (
                job.split("- name: Cargo test cache", maxsplit=1)[1]
                if job_name == "rust-test"
                else job
            )
            cache_step = _without_yaml_comments(
                job_scope.split("uses: Swatinem/rust-cache@v2", maxsplit=1)[1].split(
                    "\n      - name:", maxsplit=1
                )[0]
            )
            self.assertIn("cache-on-failure: true", cache_step, job_name)
            self.assertIn("refs/heads/main", cache_step, job_name)
        rust_test_job = self.pr_workflow.split(
            "\n  rust-test:", maxsplit=1
        )[1].split("\n  cli-test:", maxsplit=1)[0]
        rust_test_cache = _without_yaml_comments(
            rust_test_job.split("- name: Cargo test cache", maxsplit=1)[1].split(
                "uses: Swatinem/rust-cache@v2", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn("shared-key: rust-test-v2", rust_test_cache)
        # The re-enable is the absence of the old cache-targets: false
        # (rust-cache's default keeps target artifacts). Comments stripped:
        # the decision record above mentions the old value historically.
        self.assertNotIn("cache-targets:", rust_test_cache)

    def test_console_container_script_only_diff_selects_its_actual_job(self):
        changes = _without_yaml_comments(
            self.pr_workflow.split("            update_release_console:", 1)[1].split(
                "            acp_runtime:", 1
            )[0]
        )
        paths = _extract_quoted_paths(changes)
        self.assertTrue(_matches_paths_filter("scripts/ci-update-release-console-container.sh", paths))
        self.assertNotIn("scripts/**", paths)
        console = _without_yaml_comments(_extract_job_block(self.pr_workflow, "update-release-console-test"))
        self.assertEqual(
            _extract_job_level_if(console),
            "${{ !cancelled() && github.event.action != 'closed' && needs.changes.outputs.update_release_console == 'true' && (github.event_name == 'pull_request' || github.event_name == 'merge_group') }}",
        )
        self.assertRegex(console, r"(?m)^        run: bash scripts/ci-update-release-console-container\.sh$")

    def test_dev_profile_o0_guard_stays_on_all_three_heavy_legs(self):
        # Linux may reuse a runner with a persistent Cargo home. Keep O0 on
        # each actual invocation, without writing administrator configuration.
        # Windows/macOS retain their existing hosted-runner payloads.
        for job_name, end_marker in (
            ("rust-test", "\n  cli-test:"),
            ("windows-rust-test", "\n  macos-rust-check:"),
            ("macos-rust-check", "\n  windows-codex-runtime-test:"),
        ):
            job = _without_yaml_comments(
                self.pr_workflow.split(f"\n  {job_name}:", maxsplit=1)[1].split(
                    end_marker, maxsplit=1
                )[0]
            )
            if job_name == "rust-test":
                executable = re.findall(
                    r"(?m)^ +(?:run: )?(?:/usr/bin/time -v )?cargo (.+)$", job
                )
                test_commands = [line for line in executable if re.search(r"(?:^| )test(?: |$)", line)]
                self.assertEqual(len(test_commands), 4)
                for command in test_commands:
                    self.assertTrue(command.startswith("--config 'profile.dev.package.codewhale-tui.opt-level=0' test"), command)
                self.assertNotIn('config="${CARGO_HOME:-$HOME/.cargo}/config.toml"', job)
                self.assertNotIn('cat >> "$config"', job)
            else:
                self.assertIn("codewhale-tui O0", job, job_name)
                self.assertIn("[profile.dev.package.codewhale-tui]", job, job_name)
                self.assertIn("opt-level = 0", job, job_name)
                if job_name == "macos-rust-check":
                    self.assertIn('config="${CARGO_HOME:-$HOME/.cargo}/config.toml"', job)

    def test_frontend_test_webui_smoke_reuses_the_prebuilt_dist(self):
        # frontend-test builds the web dist unconditionally (the web index
        # contract test only covers it when it exists), so the diff-selected
        # test:webui smoke must reuse that dist via PINVOU_WEBUI_SKIP_BUILD=1
        # instead of paying a second identical vite build. Both smoke steps —
        # the PR-selected and the merge-group one — must keep the env: a
        # dropped step silently reintroduces the duplicate build.
        # Comment-stripped: commenting one of the two live lines must not
        # keep the count at 2 (round-3 review of PR #745).
        frontend = _without_yaml_comments(
            self.pr_workflow.split("\n  frontend-test:", maxsplit=1)[1].split(
                "\n  relay-test:", maxsplit=1
            )[0]
        )
        self.assertEqual(
            frontend.count('PINVOU_WEBUI_SKIP_BUILD: "1"'),
            2,
            "both webui smoke steps must set PINVOU_WEBUI_SKIP_BUILD=1",
        )

    def test_release_checkouts_never_persist_credentials(self):
        # release-packages 的 header 承诺 checkout 后不保留凭据。构建 job 的
        # 子模块在 checkout 内部用 token 初始化,之后的运行时仓库按锁定
        # gitlink 匿名拉取(Windows 先行验证);PR #745 补齐其余四腿,这里把
        # 每个 checkout(含 gate 的纯文件级比对)钉住,凭据策略不得回退。
        workflow = (ROOT / ".github/workflows/release-packages.yml").read_text(
            encoding="utf-8"
        )
        for job_name, end_marker in (
            ("check-version-bump", "\n  build-linux-x64:"),
            ("build-linux-x64", "\n  build-linux-arm64:"),
            ("build-linux-arm64", "\n  build-linux-loongarch64:"),
            ("build-linux-loongarch64", "\n  build-windows-x64:"),
            ("build-windows-x64", "\n  build-macos-universal:"),
            ("build-macos-universal", None),
        ):
            job = workflow.split(f"\n  {job_name}:", maxsplit=1)[1]
            if end_marker is not None:
                job = job.split(end_marker, maxsplit=1)[0]
            checkout = job.split("uses: actions/checkout@v7", maxsplit=1)[1].split(
                "\n      - name:", maxsplit=1
            )[0]
            # Comment-stripped: a commented-out live line must not satisfy
            # the pin (same live-config rule as _without_yaml_comments users
            # elsewhere in this file).
            self.assertIn(
                "persist-credentials: false",
                _without_yaml_comments(checkout),
                job_name,
            )
        # The gate compares VERSION files only: no submodules, depth 1 — a
        # future git-history-reading step must not silently need depth 0.
        gate = workflow.split("\n  check-version-bump:", maxsplit=1)[1].split(
            "\n  build-linux-x64:", maxsplit=1
        )[0]
        gate_checkout = gate.split("uses: actions/checkout@v7", maxsplit=1)[1].split(
            "\n      - name:", maxsplit=1
        )[0]
        live_gate_checkout = _without_yaml_comments(gate_checkout)
        self.assertIn("\n          fetch-depth: 1\n", live_gate_checkout)
        self.assertNotIn("submodules:", live_gate_checkout)

    def test_quota_and_toolchain_hardening_stays_pinned(self):
        # 2026-10 audit hardening. Each item is a silent policy change (quota
        # weight or toolchain drift) if removed, with no runtime error to
        # expose it — so each is pinned:
        # - CARGO_INCREMENTAL=0 repo-wide: incremental artifacts are pure
        #   cache-bloat on ephemeral runners under the 10GB quota.
        # - knowledge-rust drops its target cache (workspace compiles cold
        #   inside its 30-minute cap); only ~/.cargo is cached.
        # - The CodeWhale Windows regression resolves its toolchain via the
        #   same rust-toolchain.toml-derived resolution as every other leg
        #   (RUSTUP_TOOLCHAIN beats the directory override file).
        # - Both Cargo.lock drift guards fail loud when a cargo invocation
        #   regenerated a lockfile instead of honoring it (--locked parity).
        self.assertIn('CARGO_INCREMENTAL: "0"', self.pr_workflow)

        knowledge_job = _extract_job_block(self.pr_workflow, "knowledge-rust")
        knowledge_cache = _without_yaml_comments(
            knowledge_job.split(
                "uses: Swatinem/rust-cache@v2", maxsplit=1
            )[1].split("\n      - name:", maxsplit=1)[0]
        )
        self.assertIn("cache-targets: false", knowledge_cache)

        windows_rust_job = _extract_job_block(
            self.pr_workflow, "windows-rust-test"
        )
        self.assertIn(
            "RUSTUP_TOOLCHAIN: ${{ steps.pinned_toolchain.outputs.version }}",
            _without_yaml_comments(windows_rust_job),
        )

        for job_name, end_marker in (
            ("knowledge-rust", "\n  rust-lint:"),
            ("rust-lint", "\n  rust-test:"),
        ):
            job = self.pr_workflow.split(
                f"\n  {job_name}:", maxsplit=1
            )[1].split(end_marker, maxsplit=1)[0]
            self.assertIn(
                "Cargo.lock drift guard", job, job_name,
            )
            self.assertIn(
                "git diff --exit-code -- '**/Cargo.lock'", job, job_name,
            )

    def test_connector_darwin_x64_executes_on_intel(self):
        # darwin-x64 verifies the pinned x86_64 connector CLIs by EXECUTING
        # them. On the arm64 image that step was skipped (can_run: false),
        # leaving sha256/file checks only — a hash-matching but broken binary
        # shipped green. macos-15-intel is the Intel image (sunset ~2027 with
        # the macos-15 generation; the re-homing note lives in the matrix).
        workflow = default_ci_routing((
            ROOT / ".github/workflows/connector-verify.yml"
        ).read_text(encoding="utf-8"))
        darwin_x64 = workflow.split(
            "- platform: darwin-x64", maxsplit=1
        )[1].split("- platform:", maxsplit=1)[0]
        self.assertIn("runs-on: macos-15-intel", darwin_x64)
        self.assertIn("can_run: true", darwin_x64)
        # The arm64 leg keeps executing on the arm64 image (no coverage lost).
        darwin_arm64 = workflow.split(
            "- platform: darwin-arm64", maxsplit=1
        )[1].split("- platform:", maxsplit=1)[0]
        self.assertIn("runs-on: macos-15", darwin_arm64)
        self.assertIn("can_run: true", darwin_arm64)

    def test_wrapper_smoke_routes_merge_groups_before_platform_matrix(self):
        # rustc-wrapper-smoke must first pass the paths-filter gate before
        # entering the three-platform matrix, so wrapper-unrelated PRs do not
        # run the full three-platform smoke.
        # Folded in from scripts/tests/test_ci_trigger_routing_policy.py.
        workflow = default_ci_routing((
            ROOT / ".github/workflows/rustc-wrapper-smoke.yml"
        ).read_text(encoding="utf-8"))
        trigger = workflow.split("\non:", maxsplit=1)[1].split(
            "\npermissions:", maxsplit=1
        )[0]
        pull_request = trigger.split("\n  pull_request:", maxsplit=1)[1].split(
            "\n  merge_group:", maxsplit=1
        )[0]
        push = trigger.split("\n  push:", maxsplit=1)[1]
        changes = workflow.split("\n  changes:", maxsplit=1)[1].split(
            "\n  smoke:", maxsplit=1
        )[0]
        smoke = workflow.split("\n  smoke:", maxsplit=1)[1]

        self.assertIn("merge_group:", trigger)
        self.assertIn("push:", trigger)
        self.assertIn("paths:", trigger)
        workflow_path = "'.github/workflows/rustc-wrapper-smoke.yml'"
        self.assertIn(workflow_path, pull_request)
        self.assertIn(workflow_path, push)
        self.assertIn(workflow_path, changes)
        self.assertIn("uses: dorny/paths-filter@v4", changes)
        self.assertIn("wrapper: ${{ steps.filter.outputs.wrapper }}", changes)
        self.assertIn("needs: changes", smoke)
        self.assertIn("if: ${{ needs.changes.outputs.wrapper == 'true' }}", smoke)
        self.assertIn("os: [macos-15, ubuntu-22.04, windows-latest]", smoke)
        # The 2026-10 concurrency fix: only PR runs cancel each other; queue
        # entries and main pushes must never cancel (queue entries carry the
        # required-check contexts the merge queue waits on). Both jobs get
        # explicit caps instead of the 360-minute workflow default.
        self.assertIn(
            "cancel-in-progress: ${{ github.event_name == 'pull_request' }}",
            workflow,
        )
        self.assertIn("timeout-minutes: 10", changes)
        self.assertIn("timeout-minutes: 20", smoke)

    def test_wrapper_smoke_chain_probe_step_is_pinned(self):
        # Round-13 review: the chain-contract probe (step 5) had no
        # anti-deletion pin — deleting the whole step left the remaining
        # smoke steps green and every gate-policy test here untouched,
        # exactly the gap this PR fixed for the windows runtime-manifest
        # step in round 11. Pin the step name and the two load-bearing
        # probe assertions on executable lines, so the smoke can silently
        # lose its delegation contract no more.
        workflow = default_ci_routing((
            ROOT / ".github/workflows/rustc-wrapper-smoke.yml"
        ).read_text(encoding="utf-8"))
        self.assertIn(
            "- name: 链式 wrapper —— 完整 argv 委托链上命令且仍注入 16 MiB",
            workflow,
        )
        step = workflow.split(
            "- name: 链式 wrapper —— 完整 argv 委托链上命令且仍注入 16 MiB",
            maxsplit=1,
        )[1].split("\n      - name:", maxsplit=1)[0]
        step_code = "\n".join(
            line
            for line in step.splitlines()
            if not line.lstrip().startswith("#")
        )
        # Round-20 review: two of the probe's five assertions were pinned —
        # deleting the three per-argv content greps kept every gate-policy
        # test green, so a "same arg count, different args" edit (the
        # delegation contract's other half) regressed silently. Pin all
        # five on executable lines.
        for probe in (
            "grep -Fqx 'chain_probe_stack=[16777216]'",
            "grep -Fqx 'chain_probe_argc=3'",
            "grep -Fqx 'chain_probe_arg=[rustc]'",
            "grep -Fqx 'chain_probe_arg=[--version]'",
            "grep -Fqx 'chain_probe_arg=[--verbose]'",
        ):
            self.assertIn(
                probe,
                step_code,
                f"the chain probe must keep its {probe.split('[')[0]}… "
                "assertion as an executable line — deleting it reopens the "
                "merge-args/drop-injection escape the round-12 probe rewrite "
                "closed",
            )
        self.assertIn("rustc-stack-wrapper", step_code)
        # Round-22 review: substring pins are echo-neutralizable — a step
        # that merely echoes the pinned substrings (probe logic deleted)
        # passed every assertion above, and the smoke workflow has no
        # step-name-uniqueness backstop. Pin the full invocation and the
        # five full assertion LINES (the disk-step exact-recipe pattern),
        # and ban the step-level decoys (if:/continue-on-error).
        self.assertIn(
            'out="$(RUSTC_WRAPPER_CHAIN="$PWD/chain-probe.exe" '
            '"$wrapper" rustc --version --verbose)"',
            step_code,
            "the probe must actually run THROUGH the wrapper under "
            "RUSTC_WRAPPER_CHAIN — running it directly (or echoing the "
            "assertions) breaks the delegation contract this step exists "
            "to verify",
        )
        for assertion_line in (
            "printf '%s\\n' \"$out\" | grep -Fqx 'chain_probe_stack=[16777216]'",
            "printf '%s\\n' \"$out\" | grep -Fqx 'chain_probe_argc=3'",
            "printf '%s\\n' \"$out\" | grep -Fqx 'chain_probe_arg=[rustc]'",
            "printf '%s\\n' \"$out\" | grep -Fqx 'chain_probe_arg=[--version]'",
            "printf '%s\\n' \"$out\" | grep -Fqx 'chain_probe_arg=[--verbose]'",
            # round-23: exit-code half of the delegation contract
            'CHAIN_PROBE_EXIT=42 RUSTC_WRAPPER_CHAIN="$PWD/chain-probe.exe" '
            '"$wrapper" rustc --version --verbose >/dev/null || chain_status=$?',
            'test "$chain_status" -eq 42',
        ):
            self.assertIn(
                assertion_line,
                step_code,
                "the chain probe must keep its full per-field assertion "
                "line — a weakened or echoed form no longer proves the "
                "delegation contract",
            )
        self.assertNotIn(
            "continue-on-error",
            step_code,
            "the chain probe step must not be allowed to fail silently",
        )
        self.assertFalse(
            any(re.match(r"^ {8}if:", line) for line in step.splitlines()),
            "the chain probe step must run unconditionally on its leg "
            "(an `if:` on the step would let a self-decoy keep every pin "
            "green while the probe never runs)",
        )




class DependencyReviewWarnOnlyGateTests(unittest.TestCase):
    """dependency-review.yml 的能力缺失降级必须由仓库 variable 门控，不得无条件吞错。

    Dependency graph 未开启时动作必然失败（能力缺失），降级为告警是合理的；
    但降级必须由仓库 variable DEPENDENCY_REVIEW_WARN_ONLY 门控——无条件
    continue-on-error 会在依赖图开启后把真实 high 级漏洞/许可证违规静默吞成
    告警，且没有任何机制或测试保障恢复强制门禁。
    """

    def test_dependency_review_downgrade_is_variable_gated(self):
        workflow = (ROOT / ".github/workflows/dependency-review.yml").read_text(
            encoding="utf-8"
        )
        self.assertIsNone(
            re.search(r"continue-on-error:\s*['\"]?true['\"]?", workflow),
            "dependency-review 不得无条件 continue-on-error（含引号形式 "
            "`continue-on-error: 'true'`）：降级必须由 vars.DEPENDENCY_REVIEW_WARN_ONLY "
            "门控，依赖图开启后把该 variable 设为 'false' 即恢复强制门禁",
        )
        self.assertIn(
            "vars.DEPENDENCY_REVIEW_WARN_ONLY",
            workflow,
            "dependency-review 的降级必须由仓库 variable DEPENDENCY_REVIEW_WARN_ONLY 门控",
        )
        self.assertIn(
            "fail-on-severity: high",
            workflow,
            "dependency-review 必须保留 fail-on-severity: high 强度阈值",
        )


class SecurityGateEnforcementPinsTests(unittest.TestCase):
    """secret-scan / dco / .gitleaks.toml 的承重形态钉（round-2 评审收口）。

    变异实证：把 dco 的 core.setFailed 降级为 core.warning、删除 secret-scan
    的「commits scanned」防空扫守卫、甚至整文件删除 secret-scan.yml，原有
    套件全部保持绿色——门禁的强制力此前没有任何测试保护。本类以存在性
    断言钉住各承重点；改动这些形态时必须显式评估并同步更新对应断言。
    """

    def test_secret_scan_workflow_keeps_fail_closed_guard(self):
        workflow = (ROOT / ".github/workflows/secret-scan.yml").read_text(
            encoding="utf-8"
        )
        self.assertIn(
            'grep -Eq "[1-9][0-9]* commits scanned"',
            workflow,
            "secret-scan 的防空扫守卫不得删除：gitleaks 的 exit code 在坏 git "
            "区间下不可信（可 exit 0 且打印 0 commits）",
        )
        self.assertIn(
            "fetch-depth: 0",
            workflow,
            "全量历史扫描需要完整 checkout（fetch-depth: 0）",
        )
        self.assertIn(
            "sha256sum -c -",
            workflow,
            "gitleaks 安装必须保留校验和验证",
        )
        self.assertIn(
            "--diff-merges=first-parent",
            workflow,
            "git log -p 默认不输出 merge 补丁；扫描必须显式纳入 merge diff，"
            "否则 evil merge / 绕队列手工 merge 引入的泄漏对本门禁不可见",
        )

    def test_secret_scan_cutoff_matches_commit_message_gate(self):
        validator = (ROOT / "scripts/validate-commit-msg.py").read_text(
            encoding="utf-8"
        )
        match = re.search(
            r"LEGACY_HISTORY_CUTOFF\s*=\s*[\"']([0-9a-f]{40})[\"']", validator
        )
        self.assertIsNotNone(
            match, "validate-commit-msg.py 必须定义 40 位 hex 的 LEGACY_HISTORY_CUTOFF"
        )
        workflow = (ROOT / ".github/workflows/secret-scan.yml").read_text(
            encoding="utf-8"
        )
        self.assertIn(
            match.group(1),
            workflow,
            "secret-scan 的 --log-opts 扫描起点必须与 LEGACY_HISTORY_CUTOFF 一致，"
            "两道门禁的信任边界不得漂移",
        )

    def test_dco_workflow_stays_enforcing(self):
        workflow = (ROOT / ".github/workflows/dco.yml").read_text(encoding="utf-8")
        self.assertIn(
            "core.setFailed(",
            workflow,
            "dco 必须以 core.setFailed 强制失败；降级为 core.warning 会让门禁静默失效",
        )
        self.assertIn(
            "/^Signed-off-by:",
            workflow,
            "dco 的签署存在性正则不得移除或弱化",
        )
        self.assertIn('"dependabot[bot]"', workflow)
        self.assertIn('"github-actions[bot]"', workflow)

    def test_dco_workflow_fails_closed_on_commit_list_truncation(self):
        """DCO 检查必须对 PR 提交列表截断保持 fail-closed（2026-11-03 评审收口）。

        `pulls.listCommits` 即使 paginate 也在 250 条静默封顶：更大 PR 的尾部
        提交会在这道门禁里未经核验就放行。必须以 `pulls.get().commits` 的
        权威计数比对并在截断时 core.setFailed，不得降级为 warning——与
        secret-scan 空证据即失败的公约同因。
        """
        workflow = (ROOT / ".github/workflows/dco.yml").read_text(encoding="utf-8")
        self.assertIn(
            "github.rest.pulls.get(",
            workflow,
            "dco 必须取 pulls.get 的权威提交计数用于截断比对",
        )
        self.assertIn(
            "pr.data.commits > commits.length",
            workflow,
            "dco 必须比对权威计数与 listCommits 返回数，缺失比对会让截断静默放行",
        )
        self.assertIn(
            "squash or split the PR",
            workflow,
            "截断分支必须 core.setFailed 并给出 squash/split 指引，不得静默通过",
        )

    def test_gitleaks_config_keeps_default_rules_and_allowlist_surface(self):
        config_path = ROOT / ".gitleaks.toml"
        self.assertTrue(config_path.exists(), ".gitleaks.toml 不得删除")
        config = tomllib.loads(config_path.read_text(encoding="utf-8"))
        self.assertEqual(
            config.get("extend", {}).get("useDefault"),
            True,
            "必须扩展 gitleaks 默认规则集（useDefault = true），不得整体替换",
        )
        allowlists = config.get("allowlists", [])
        self.assertEqual(
            len(allowlists),
            18,
            "allowlist 组数被钉死（cargo-deny 精确集合同款公约）：新增或删除豁免组"
            "属于扫描面安全变化，必须显式更新本断言并逐组说明理由",
        )

    def test_gitleaks_allowlists_anchor_extracted_secret_full_value(self):
        """豁免条件必须绑定提取 secret 的完整受审值（2026-11-02 评审收口）。

        旧形态 regexTarget="line" 下，登记字面量只命中整行的已知前缀：登记
        docKey 样例后追加不同字符，generic-api-key 提取的是完整未登记新值，
        但允许正则仍在整行命中已知前缀，使路径 AND 正则条件一起成立，把
        未登记的新值（含同行真实凭据样式的其它命中）一并吞掉。新形态要求
        每组 regexTarget="secret" 且正则全值锚定（^…$），唯一例外是
        private-key 标记组——其 secret 是从 BEGIN 标记开始的多行 blob，
        只做起点锚定，且仅作用于 secret 按定义以 BEGIN 开头的 private-key
        规则。gitleaks 二进制不进本套件，这里以合成 ASCII 谓词复现提取值
        的匹配判定（提取=赋值右侧完整 token）。
        """
        import re as _re

        config = tomllib.loads((ROOT / ".gitleaks.toml").read_text(encoding="utf-8"))
        allowlists = config.get("allowlists", [])
        self.assertTrue(allowlists, ".gitleaks.toml 必须定义 allowlist 组")

        for group in allowlists:
            label = group.get("description", "<无描述>")
            self.assertEqual(
                group.get("regexTarget"),
                "secret",
                f"豁免组「{label}」必须 regexTarget=\"secret\"；line 目标会把"
                "登记前缀扩展出的未登记新值一并豁免",
            )
            self.assertEqual(
                group.get("condition"),
                "AND",
                f"豁免组「{label}」必须保持路径 AND 正则的 AND 条件",
            )
            start_anchored_only = group.get("targetRules") == ["private-key"]
            for pattern in group.get("regexes", []):
                self.assertTrue(
                    pattern.startswith("^"),
                    f"豁免组「{label}」的正则必须从 secret 起点锚定: {pattern}",
                )
                if not start_anchored_only:
                    self.assertTrue(
                        pattern.endswith("$"),
                        f"豁免组「{label}」的正则必须锚定到 secret 完整值结束"
                        f"（private-key 标记组除外）: {pattern}",
                    )

        # 组 1（文档登记值）的合成谓词正负控：登记值豁免、前缀扩展/其它值/
        # 48 字符长扩展不豁免。
        doc_group = next(
            (g for g in allowlists if "Pinned upstream connector" in g["description"]),
            None,
        )
        self.assertIsNotNone(doc_group, "组 1（文档登记值）不得删除或改名")
        compiled = [_re.compile(p) for p in doc_group["regexes"]]

        def any_match(secret: str) -> bool:
            return any(c.match(secret) is not None for c in compiled)

        registered = [
            "wo1g3x54FzVEJ5yE",
            "rBGBr2r1HmwanAGW",
            "Pd6l2Z7V8ZWydl7M",
            "YhljsPiGfUgnVAg9urvRFd-BvSqRLxxxx",
            "A2f5boKjfazMzesI9zKbmugTc4T",
        ]
        for value in registered:
            self.assertTrue(
                any_match(value),
                f"登记值 {value!r} 必须仍被组 1 豁免（豁免面不得回退）",
            )
        for attack in (
            "Pd6l2Z7V8ZWydl7MAPPENDED",
            "A2f5boKjfazMzesI9zKbmugTc4T" + "x" * 24,
            "Qq7Xk2Vb9NwRt5Yz",
            "sk-live-realkeyshape0",
        ):
            self.assertFalse(
                any_match(attack),
                f"非登记值 {attack!r} 不得被组 1 豁免（前缀扩展/其它值必须照常报出）",
            )

    def test_gitleaks_protocol_fixture_group_enumerates_registered_vectors(self):
        """协议夹具豁免组必须逐一枚举登记值，不得放行任意 64 位 hex（2026-11-02 评审收口）。

        旧形态 `regexTarget="secret"` + `^[0-9a-f]{64}$` 只锚定了长度形状，
        会把该夹具内任意字段提取到的任意 64 位 hex 一并豁免：向夹具追加一
        个未登记的 api_key 赋值（任意合法 SHA-256 形状即可满足）会静默落
        入本组——完整长度锚定本身不能证明它是已登记的确定性向量。新形态
        要求每条正则都是 `^<64 位 hex 字面量>$` 的精确枚举，且每个字面量
        必须真实存在于夹具文件中（夹具向量变更时登记必须同步复核）。合成
        谓词正负控覆盖：登记值豁免、未登记 64-hex 与前缀扩展不豁免。
        """
        import re as _re

        config = tomllib.loads((ROOT / ".gitleaks.toml").read_text(encoding="utf-8"))
        allowlists = config.get("allowlists", [])
        fixture = (
            ROOT
            / "remote-control-relay"
            / "test"
            / "fixtures"
            / "controller-protocol-v1.json"
        )
        self.assertTrue(fixture.exists(), "协议夹具文件不得删除或改名")

        group = next(
            (
                g
                for g in allowlists
                if "controller-protocol-v1" in (g.get("paths") or [""])[0]
            ),
            None,
        )
        self.assertIsNotNone(group, "协议夹具豁免组不得删除")
        self.assertEqual(
            group.get("targetRules"),
            ["generic-api-key"],
            "协议夹具豁免组的 targetRules 不得漂移",
        )

        hex_literal = _re.compile(r"^\^[0-9a-f]{64}\$$")
        patterns = group.get("regexes", [])
        self.assertTrue(patterns, "协议夹具豁免组必须逐一枚举登记值，不得为空")
        registered = []
        fixture_text = fixture.read_text(encoding="utf-8")
        for pattern in patterns:
            self.assertRegex(
                pattern,
                hex_literal,
                "协议夹具豁免组的每条正则都必须是 ^<64 位 hex 字面量>$ 的"
                f"精确枚举，不得放行任意长度形状: {pattern}",
            )
            value = pattern[1:-1]
            self.assertIn(
                value,
                fixture_text,
                f"豁免组登记值 {value} 必须真实存在于协议夹具文件中",
            )
            registered.append(value)
        self.assertEqual(
            len(registered),
            len(set(registered)),
            "协议夹具豁免组不得出现重复登记",
        )

        compiled = [_re.compile(p) for p in patterns]

        def group_match(secret: str) -> bool:
            return any(c.match(secret) is not None for c in compiled)

        for value in registered:
            self.assertTrue(
                group_match(value),
                f"登记值 {value} 必须仍被本组豁免（豁免面不得回退）",
            )
        # 同文件未登记 64-hex 凭据负例：任何不在这批枚举里的 64 位 hex
        # （包括合法 SHA-256 形状的合成值）都不得落入本组豁免。
        unregistered = [
            "b3a9e2f14c8d7a0655e93f21c4b7d80669a2f5e18c7d4b309a6e1f25d8c4b7a9",
            "0" * 64,
            ("a" * 63) + "2",
        ]
        for attack in unregistered:
            self.assertNotIn(attack, registered)
            self.assertFalse(
                group_match(attack),
                f"未登记 64-hex 值 {attack} 不得被本组豁免（须照常报出）",
            )


class ReleaseDiskAndImagePolicyTests(unittest.TestCase):
    """Guard for release-build disk preparation and the single-image convention (backported from private-repo #1112 on 2026-09-16)."""

    def setUp(self):
        self.release_workflow = (ROOT / ".github/workflows/release-packages.yml").read_text(
            encoding="utf-8"
        )

    def test_release_linux_build_jobs_prepare_disk_and_prune_apt(self):
        # Release build jobs (single-disk hosted runner; with the old 16G
        # /mnt swapfile in place x64 builds had only ~13-14G free) must clean
        # up unused preinstalled SDKs before toolchains/caches/dependencies
        # hit the disk, and run autoremove + clean after installing system
        # deps; otherwise a cold compile has filled the disk (ENOSPC, since
        # 2026-09-13).
        blocks = re.split(
            r"\n  (?=[A-Za-z0-9_-]+:\s*$)", self.release_workflow, flags=re.MULTILINE
        )
        for job_id in ("build-linux-x64", "build-linux-arm64"):
            job = next(
                (b for b in blocks if b.strip().startswith(f"{job_id}:")), None
            )
            self.assertIsNotNone(job, f"release job '{job_id}' not found")
            self.assertIn("python3 scripts/ci-rust-disk.py", job)
            self.assertIn("--min-free-gib 24", job)
            # Disk preparation must run before setup-node (the aggressive tier
            # deletes /opt/hostedtoolcache, and setup-node would re-download
            # Node afterwards) and before the toolchain lands, so the
            # free-space gate measures the disk the cold build actually
            # gets. (The release jobs no longer restore a rust-cache —
            # removed 2026-10-15, see test_release_cache_policy — so there is
            # no cache-restore ordering left to pin.)
            self.assertLess(
                job.index("python3 scripts/ci-rust-disk.py"),
                job.index("uses: actions/setup-node"),
            )
            self.assertLess(
                job.index("python3 scripts/ci-rust-disk.py"),
                job.index("uses: dtolnay/rust-toolchain"),
            )
            self.assertIn("sudo apt-get autoremove -y --purge", job)
            self.assertIn("sudo apt-get clean", job)
        x64 = next(b for b in blocks if b.strip().startswith("build-linux-x64:"))
        arm64 = next(b for b in blocks if b.strip().startswith("build-linux-arm64:"))
        self.assertIn("--aggressive", x64)
        self.assertNotIn("--aggressive", arm64)

    def test_all_linux_jobs_pin_the_release_runner_image(self):
        # Image versions never drift (single-image convention): every
        # workflow's Linux runner must match the release build baseline
        # (ubuntu-22.04 / ubuntu-22.04-arm). Release binaries link against the
        # build host's glibc, so tests and checks must run on the same system
        # as the release build; image upgrades must be coordinated across the
        # whole repo at once — rolling images like ubuntu-latest and per-job
        # version bumps are forbidden.
        # Strip full-line YAML comments before scanning: version mentions
        # inside full-line comments (e.g. migration notes) must not trip the
        # image rules. Note that the ubuntu-latest ban still scans the
        # remaining text, including inline trailing comments — keep such
        # notes on their own comment lines.
        allowed = {"ubuntu-22.04", "ubuntu-22.04-arm"}
        workflows = sorted(
            list((ROOT / ".github/workflows").glob("*.yml"))
            + list((ROOT / ".github/workflows").glob("*.yaml"))
        )
        self.assertTrue(workflows, "no workflow files found under .github/workflows")
        for workflow in workflows:
            text = _without_yaml_comments(workflow.read_text(encoding="utf-8"))
            self.assertNotIn(
                "ubuntu-latest",
                text,
                f"{workflow.name}: ubuntu-latest is a rolling image and violates"
                " the single-image convention; pin it to ubuntu-22.04 like the"
                " release build",
            )
            for image in sorted(set(re.findall(r"ubuntu-\d+\.\d+(?:-arm)?", text))):
                self.assertIn(
                    image,
                    allowed,
                    f"{workflow.name}: Linux image '{image}' diverges from the"
                    " release baseline; image upgrades must happen repo-wide"
                    " at once",
                )

    def test_audited_redundant_apt_packages_stay_pruned(self):
        # 2026-09-15 per-package probe audit verdict (simulate installing each
        # package alone; if the installed set is unchanged the package is
        # redundant): libgtk-3-dev/libsoup-3.0-dev/libx11-dev/libxi-dev/
        # libxtst-dev are all pulled in transitively by the hard dependency
        # chain of libwebkit2gtk-4.1-dev, and the build does not need
        # librsvg2-dev (Cargo has no rsvg crate). Any workflow adding them
        # back to an install list would slow dependency installation and eat
        # the single-disk runner's build disk. The guard scans the full text
        # with comments stripped: audit comments may mention these names, but
        # their appearance in non-comment text is rejected (including
        # multi-line continuations).
        redundant = (
            "libgtk-3-dev",
            "libsoup-3.0-dev",
            "librsvg2-dev",
            "libx11-dev",
            "libxi-dev",
            "libxtst-dev",
        )
        workflows = sorted(
            list((ROOT / ".github/workflows").glob("*.yml"))
            + list((ROOT / ".github/workflows").glob("*.yaml"))
        )
        self.assertTrue(workflows, "no workflow files found under .github/workflows")
        for workflow in workflows:
            text = _without_yaml_comments(workflow.read_text(encoding="utf-8"))
            for package in redundant:
                self.assertNotIn(
                    package,
                    text,
                    f"{workflow.name}: audited redundant apt package '{package}'"
                    " must not be added back (pulled in transitively by"
                    " libwebkit2gtk-4.1-dev or not needed by the build)",
                )


@unittest.skipUnless(shutil.which("bash"), "bash is required to run the script")
@unittest.skipIf(hasattr(os, "geteuid") and os.geteuid() == 0,
                 "the host-touching script must not be exercised as root")
class CiMemorySetupRepairBehaviorTests(unittest.TestCase):
    """repair_dpkg_state 的行为级回归(2026-10-28 评审轮 3)。

    合同断言只钉文本:把修复函数清成空实现、保留注释,静态钉仍可能全绿。
    这里以桩替身端到端运行真实脚本(仅中和 root guard),用假时钟 + 假
    sleep 把 40s 等锁预算压缩到毫秒级,覆盖:忙锁快败按钟重试、预算内
    (>20s)释放原地恢复、预算外由 EXIT 再入兜住、真实失败早停不烧预算、
    干净状态零 configure、journal-only 与 audit 不可用也进修复。生产修复
    行为失效(函数体被清空等)时,本类必红。
    """

    # 假时钟基准:脚本启动到 repair 之间没有真实 sleep,桩时钟仍在此值。
    BASE = 1000000000

    @classmethod
    def setUpClass(cls):
        cls.src = (ROOT / "scripts" / "ci-memory-setup.sh").read_text(encoding="utf-8")
        guard = "if [[ ${EUID} -ne 0 ]]; then"
        if guard not in cls.src:
            raise AssertionError("root guard drifted; the behavior harness must be updated")

    def setUp(self):
        tmp = Path(tempfile.mkdtemp(prefix="memory-setup-repair-"))
        self.addCleanup(shutil.rmtree, tmp, ignore_errors=True)
        self.state = tmp / "state"
        self.state.mkdir()
        self.bin = tmp / "bin"
        self.bin.mkdir()
        self.updates = tmp / "updates"
        self.updates.mkdir()
        self.clock = self.state / "clock"
        self.clock.write_text(str(self.BASE))
        for name, body in self.STUBS.items():
            stub = self.bin / name
            stub.write_text(body)
            stub.chmod(0o755)

    # dpkg 桩的行为:--audit 按 state/audit-dirty、state/audit-fail 给出
    # 脏/不可用;--configure -a 在 release-at 之前模拟忙锁毫秒级快败
    # (rc 2),slow-fail 模拟真实失败(推进假时钟 30s 后 rc 1);成功即
    # 清理脏标记与残留 journal,对应真实 dpkg 修复后的收敛状态。
    STUBS = {
        "date": """#!/usr/bin/env bash
# Fake clock: the script only ever asks for +%s (epoch seconds).
if [[ ${1:-} == "+%s" ]]; then
  cat "$REPAIR_TEST_STATE/clock"
  exit 0
fi
exit 0
""",
        "sleep": """#!/usr/bin/env bash
# Fake sleep: advance the fake clock instead of really sleeping.
case ${1:-} in
  (*[!0-9]*|"") exit 0 ;;
esac
echo $(( $(cat "$REPAIR_TEST_STATE/clock") + $1 )) > "$REPAIR_TEST_STATE/clock"
""",
        "timeout": """#!/usr/bin/env bash
# run_to invokes `timeout -k 15 SECS CMD...`; pass-through keeps the call
# shape while the fake clock governs all timing.
shift 3
exec "$@"
""",
        "sysctl": """#!/usr/bin/env bash
exit 0
""",
        "modprobe": """#!/usr/bin/env bash
echo "modprobe: FATAL: Module zram not found in directory /lib/modules/$(uname -r)" >&2
exit 1
""",
        "apt-get": """#!/usr/bin/env bash
echo "E: could not get lock /var/lib/dpkg/lock-frontend - open" >&2
exit 100
""",
        "swapon": """#!/usr/bin/env bash
case ${1:-} in
  (--show*) exit 0 ;;
esac
echo "swapon: ${1:-}: Invalid argument" >&2
exit 1
""",
        "swapoff": """#!/usr/bin/env bash
exit 1
""",
        "findmnt": """#!/usr/bin/env bash
exit 1
""",
        "df": """#!/usr/bin/env bash
exit 1
""",
        "fallocate": """#!/usr/bin/env bash
exit 1
""",
        "mkswap": """#!/usr/bin/env bash
exit 1
""",
        "dpkg": """#!/usr/bin/env bash
state="${REPAIR_TEST_STATE:?}"
echo "dpkg $* @$(cat "$state/clock")" >> "$state/dpkg-calls.log"
if [[ ${1:-} == --audit ]]; then
  if [[ -f "$state/audit-fail" ]]; then
    exit 42
  fi
  if [[ -f "$state/audit-dirty" ]]; then
    echo "The following packages have been kept back:"
    echo "  linux-image-6.8.0-999-generic"
  fi
  exit 0
fi
if [[ ${1:-} == --configure ]]; then
  if [[ -f "$state/release-at" ]]; then
    now="$(cat "$state/clock")"
    if (( now < $(cat "$state/release-at") )); then
      echo "dpkg frontend lock was locked by another process with pid 2374" >&2
      exit 2
    fi
  fi
  if [[ -f "$state/slow-fail" ]]; then
    now="$(cat "$state/clock")"
    echo $(( now + 30 )) > "$state/clock"
    echo "dpkg: error processing archive" >&2
    exit 1
  fi
  rm -f "$state/audit-dirty" "$state/audit-fail"
  if [[ -n ${PINVOU3_CI_DPKG_UPDATES_DIR:-} ]]; then
    rm -f "${PINVOU3_CI_DPKG_UPDATES_DIR}"/[0-9]* 2>/dev/null
  fi
  exit 0
fi
exit 0
""",
    }

    def _configure_calls(self):
        log = self.state / "dpkg-calls.log"
        if not log.exists():
            return []
        return [line for line in log.read_text().splitlines() if "--configure" in line]

    def _run_script(self):
        env = dict(os.environ)
        env["PATH"] = f"{self.bin}{os.pathsep}{env.get('PATH', '')}"
        env["REPAIR_TEST_STATE"] = str(self.state)
        env["PINVOU3_CI_DPKG_UPDATES_DIR"] = str(self.updates)
        env.pop("GITHUB_ACTIONS", None)
        # 行为夹具必须走 hosted 主路径:生产脚本在 RUNNER_ENVIRONMENT=self-hosted
        # 时于 repair/trap 安装前安全早退(保留管理员预置内存),该早退本身
        # 不在本类回归范围。若继承外层 self-hosted 环境(fast-gate 的
        # self-hosted runner 正是如此),五个行为测试将拿不到任何 configure
        # 调用、断言空转。显式钉 hosted,保持生产早退分支不受影响。
        env["RUNNER_ENVIRONMENT"] = "github-hosted"
        script = self.state / "ci-memory-setup.sh"
        script.write_text(self.src.replace(
            "if [[ ${EUID} -ne 0 ]]; then", "if [[ 0 -ne 0 ]]; then"
        ))
        proc = subprocess.run(
            ["bash", str(script)], capture_output=True, text=True, env=env, timeout=60,
        )
        self.assertEqual(proc.returncode, 0, f"script failed; stderr:\n{proc.stderr}")
        return proc

    def test_busy_lock_released_after_25s_is_repaired_in_exit_round(self):
        # 评审轮 3 回归:忙锁在 ~25s 后释放,按钟重试必须等到释放点恢复,
        # 而不是像 3 次×5s 固定重试那样 ~10s 就耗尽等锁窗口。主线 #2095
        # 后脚本自身不再安装内核包,修复只经 EXIT trap 单轮执行。
        (self.state / "audit-dirty").write_text("1")
        (self.state / "release-at").write_text(str(self.BASE + 25))
        proc = self._run_script()
        calls = self._configure_calls()
        # 尝试落在假时钟 0,5,10,15,20,25 —— 25s 那次锁已释放并成功。
        self.assertEqual(len(calls), 6, calls)
        self.assertIn("dpkg admin state repaired (configure attempt 6)", proc.stdout)
        self.assertNotIn("did not succeed", proc.stderr)

    def test_lock_held_past_budget_gives_up_loudly(self):
        (self.state / "audit-dirty").write_text("1")
        (self.state / "release-at").write_text(str(self.BASE + 45))
        proc = self._run_script()
        calls = self._configure_calls()
        # 唯一一轮(EXIT 兜底)的尝试落在 0..40 共 9 次:40s 预算耗尽后
        # 响亮放弃并上报仍脏的 audit;45s 才释放的锁不再有第二轮等待,
        # 残留状态交给后续步骤与告警。
        self.assertEqual(len(calls), 9, calls)
        self.assertTrue(
            all(int(c.rsplit("@", 1)[1]) <= self.BASE + 40 for c in calls), calls,
        )
        self.assertIn("dpkg --configure -a did not succeed", proc.stderr)
        self.assertIn("dpkg --audit still reports", proc.stderr)
        self.assertNotIn("dpkg admin state repaired", proc.stdout)

    def test_real_configure_failure_stops_early_without_burning_the_budget(self):
        (self.state / "audit-dirty").write_text("1")
        (self.state / "slow-fail").write_text("1")
        proc = self._run_script()
        calls = self._configure_calls()
        # 尝试本身跑了 30s 仍失败 = 真实错误,等待无益:单轮(EXIT 兜底)
        # 1 次即早停,而不是把 40s 等锁预算烧满。
        self.assertEqual(len(calls), 1, calls)
        self.assertIn("failed after 30s", proc.stderr)
        self.assertIn("did not succeed", proc.stderr)

    def test_clean_state_never_invokes_configure(self):
        proc = self._run_script()
        self.assertEqual(self._configure_calls(), [])
        self.assertNotIn("interrupted state", proc.stderr)
        self.assertNotIn("repaired", proc.stdout)

    def test_leftover_updates_journal_alone_triggers_repair(self):
        # audit 静默但 updates/ 残留全数字 journal —— apt 判 interrupted 的
        # 同款依据,必须进修复;成功后 journal 被真实收敛(桩同步清理)。
        (self.updates / "12345").write_text("journal\n")
        proc = self._run_script()
        calls = self._configure_calls()
        self.assertEqual(len(calls), 1, calls)
        self.assertIn("dpkg admin state repaired (configure attempt 1)", proc.stdout)

    def test_unrunnable_audit_falls_through_to_repair(self):
        (self.state / "audit-fail").write_text("1")
        proc = self._run_script()
        calls = self._configure_calls()
        self.assertEqual(len(calls), 1, calls)
        self.assertIn("dpkg admin state repaired (configure attempt 1)", proc.stdout)


if __name__ == "__main__":
    unittest.main()
