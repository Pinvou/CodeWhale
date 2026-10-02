#!/usr/bin/env python3
"""Enforce pinvou3 module and platform boundaries without third-party packages.

The checked-in baseline records existing architecture debt. A check passes when
the debt stays unchanged or decreases; new violation kinds, new files/edges, and
increased counts fail. This makes the guard useful before the migration is fully
complete without normalising new debt.
"""

from __future__ import annotations

import argparse
import io
import json
import re
import subprocess
import sys
import tarfile
import tempfile
from collections import Counter, defaultdict
from pathlib import Path
from typing import Iterable


SCHEMA_VERSION = 1
DEFAULT_BASELINE = Path("scripts/architecture-baseline.json")
FRONTEND_SUFFIXES = {".html", ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx"}
SESSION_APPLICATION_EXPORTS = (
    "pub(crate) mod admission;",
    "pub(crate) mod dispatch_remote;",
    "pub(crate) mod ports;",
    "pub(crate) mod project_remote;",
    "pub(crate) mod submit_controlled;",
)
# One-time ceiling for introducing the per-callsite legacy rule. After this
# change lands, the target branch baseline is the only ratchet authority.
REMOTE_CONTROL_HOST_API_INITIAL_CEILING = {
    "pinvou3-app/src-tauri/src/features/remote_control/controller/controlling_service.rs:host_emit": 3,
    "pinvou3-app/src-tauri/src/features/remote_control/controller/controlling_service.rs:tauri_path": 2,
    "pinvou3-app/src-tauri/src/features/remote_control/controller/relay_runtime.rs:host_emit": 1,
    "pinvou3-app/src-tauri/src/features/remote_control/controller/relay_runtime.rs:tauri_path": 2,
    "pinvou3-app/src-tauri/src/features/remote_control/controller/relay_transport.rs:tauri_path": 1,
    "pinvou3-app/src-tauri/src/features/remote_control/manager/mod.rs:host_emit": 4,
    "pinvou3-app/src-tauri/src/features/remote_control/manager/mod.rs:tauri_path": 4,
    "pinvou3-app/src-tauri/src/features/remote_control/manager/rpc.rs:tauri_path": 1,
    "pinvou3-app/src-tauri/src/features/remote_control/mod.rs:tauri_path": 1,
    "pinvou3-app/src-tauri/src/features/remote_control/relay_client.rs:tauri_path": 3,
}
RUST_DEPENDENCY_MIGRATION = "rust_dependency_resolution_v1"
RUST_DEPENDENCY_MIGRATION_ANCHOR = "5bedd1ba3c4eb6534821e293bea408470155f69a"
ROOT_MODULE = "<root>"
def normalize(path: Path, root: Path) -> str:
    return path.resolve().relative_to(root.resolve()).as_posix()


def source_files(root: Path, directory: str, suffixes: set[str]) -> Iterable[Path]:
    base = root / directory
    if not base.exists():
        return []
    return (
        path
        for path in base.rglob("*")
        if path.is_file() and path.suffix.lower() in suffixes
    )


def read_text(path: Path) -> str:
    return path.read_text(encoding="utf-8")


def git_head(root: Path) -> str:
    try:
        return subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=root, text=True, stderr=subprocess.DEVNULL
        ).strip()
    except (OSError, subprocess.CalledProcessError):
        return "unknown"


def git_commit_is_ancestor(root: Path, ancestor: str, descendant: str) -> bool:
    try:
        subprocess.check_call(
            ["git", "merge-base", "--is-ancestor", ancestor, descendant],
            cwd=root,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        return True
    except (OSError, subprocess.CalledProcessError):
        return False


def migration_anchor_is_ancestor(root: Path, anchor: str) -> bool:
    return git_commit_is_ancestor(root, anchor, "HEAD")


def baseline_from_git(root: Path, ref: str, relative_path: Path) -> dict | None:
    git_path = relative_path.as_posix()
    try:
        subprocess.check_call(
            ["git", "rev-parse", "--verify", f"{ref}^{{commit}}"],
            cwd=root,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except (OSError, subprocess.CalledProcessError) as error:
        raise ValueError(f"Git base ref is unavailable: {ref}") from error
    try:
        content = subprocess.check_output(
            ["git", "show", f"{ref}:{git_path}"],
            cwd=root,
            text=True,
            stderr=subprocess.DEVNULL,
        )
    except (OSError, subprocess.CalledProcessError):
        return None
    try:
        return json.loads(content)
    except json.JSONDecodeError as error:
        raise ValueError(f"invalid baseline at {ref}:{git_path}: {error}") from error


def rust_state_from_git(
    root: Path, ref: str
) -> tuple[dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]]:
    """Run the current Rust scanner on a read-only snapshot of target sources."""
    try:
        archive = subprocess.check_output(
            ["git", "archive", "--format=tar", ref, "pinvou3-app/src-tauri/src"],
            cwd=root,
            stderr=subprocess.DEVNULL,
        )
        with tempfile.TemporaryDirectory(prefix="pinvou3-architecture-base-") as temp_dir:
            snapshot = Path(temp_dir)
            with tarfile.open(fileobj=io.BytesIO(archive), mode="r:") as entries:
                for entry in entries:
                    if not entry.isfile() or not entry.name.endswith(".rs"):
                        continue
                    target = (snapshot / entry.name).resolve()
                    if not target.is_relative_to(snapshot.resolve()):
                        raise ValueError("target Rust archive contains an unsafe path")
                    stream = entries.extractfile(entry)
                    if stream is None:
                        raise ValueError(f"unreadable target Rust source: {entry.name}")
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_bytes(stream.read())
            return scan_rust(snapshot, include_feature_edges=True)
    except (OSError, subprocess.CalledProcessError, tarfile.TarError) as error:
        raise ValueError(f"cannot scan target Rust sources at {ref}: {error}") from error


def migration_semantic_states_from_git(
    root: Path, anchor: str, target_ref: str
) -> tuple[
    tuple[dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]],
    tuple[dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]],
    tuple[dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]],
]:
    if not migration_anchor_is_ancestor(root, anchor):
        raise ValueError("Rust dependency migration anchor is not a candidate HEAD ancestor")
    try:
        merge_base = subprocess.check_output(
            ["git", "merge-base", anchor, target_ref],
            cwd=root, text=True, stderr=subprocess.DEVNULL,
        ).strip()
        target = subprocess.check_output(
            ["git", "rev-parse", "--verify", f"{target_ref}^{{commit}}"],
            cwd=root, text=True, stderr=subprocess.DEVNULL,
        ).strip()
        anchor_commit = subprocess.check_output(
            ["git", "rev-parse", "--verify", f"{anchor}^{{commit}}"],
            cwd=root, text=True, stderr=subprocess.DEVNULL,
        ).strip()
    except (OSError, subprocess.CalledProcessError) as error:
        raise ValueError(f"cannot resolve Rust dependency migration history: {error}") from error
    snapshots = {ref: rust_state_from_git(root, ref) for ref in {merge_base, anchor_commit, target}}
    return snapshots[merge_base], snapshots[anchor_commit], snapshots[target]


def rust_module_path(path: Path, rust_root: Path) -> tuple[str, ...]:
    """Map a Rust source file to its filesystem-backed crate module path."""
    parts = path.resolve().relative_to(rust_root.resolve()).parts
    if parts[-1] in {"lib.rs", "main.rs"}:
        return tuple(parts[:-1])
    if parts[-1] == "mod.rs":
        return tuple(parts[:-1])
    return tuple(parts[:-1]) + (Path(parts[-1]).stem,)


def rust_inline_module_paths(
    tokens: tuple[str, ...], module_path: tuple[str, ...]
) -> tuple[tuple[str, ...], ...]:
    """Return the lexical Rust module containing each token."""
    scopes: list[tuple[str, ...]] = []
    stack: list[tuple[str, ...]] = []
    current = module_path
    for index, token in enumerate(tokens):
        scopes.append(current)
        if token == "{":
            stack.append(current)
            if index >= 2 and tokens[index - 2] == "mod" and re.fullmatch(
                r"[A-Za-z_][A-Za-z_0-9]*", tokens[index - 1]
            ):
                current += (tokens[index - 1],)
        elif token == "}" and stack:
            current = stack.pop()
    return tuple(scopes)


def rust_brace_depths(tokens: tuple[str, ...]) -> tuple[int, ...]:
    depths: list[int] = []
    depth = 0
    for token in tokens:
        depths.append(depth)
        depth += (token == "{") - (token == "}")
    return tuple(depths)


def rust_literal_string(text: str, span: tuple[str, int, int]) -> str | None:
    if span[0] != "<literal>":
        return None
    literal = text[span[1] : span[2]]
    if literal.startswith('"'):
        try:
            value = json.loads(literal)
            return value if isinstance(value, str) else None
        except json.JSONDecodeError:
            return None
    raw = re.fullmatch(r'r(#{0,255})"(.*)"\1', literal, re.DOTALL)
    return raw.group(2) if raw else None


def rust_delimiter_end(
    tokens: tuple[str, ...], start: int, opening: str, closing: str
) -> int | None:
    if tokens[start : start + 1] != (opening,):
        return None
    depth = 1
    cursor = start + 1
    while cursor < len(tokens):
        depth += (tokens[cursor] == opening) - (tokens[cursor] == closing)
        if depth == 0:
            return cursor
        cursor += 1
    return None


def rust_module_paths(
    files: list[Path], rust_root: Path
) -> tuple[dict[Path, tuple[str, ...]], set[Path], dict[str, tuple[str, ...]]]:
    """Resolve `#[path] mod` children from the declaring module, not their disk layout.

    A file can be reached through several conditional declarations. If those
    declarations disagree, report the ambiguity rather than guess a module.
    """
    paths = {path.resolve(): rust_module_path(path, rust_root) for path in files}
    declarations: list[tuple[Path, Path, tuple[str, ...], str | None]] = []
    ambiguous: set[Path] = set()
    for path in files:
        text = read_text(path)
        if not ("path" in text or "include" in text):
            continue
        spans = rust_syntax_token_spans(text)
        tokens = tuple(token for token, _, _ in spans)
        scopes = rust_inline_module_paths(tokens, ())
        for index in range(len(tokens)):
            if tokens[index : index + 2] == ("include", "!"):
                end = rust_delimiter_end(tokens, index + 2, "(", ")")
                child: Path | None = None
                body = tokens[index + 3 : end] if end is not None else ()
                if len(body) == 1 and body[0] == "<literal>":
                    literal = rust_literal_string(text, spans[index + 3])
                    if literal is not None:
                        child = (path.parent / literal).resolve()
                elif body[:6] == ("concat", "!", "(", "env", "!", "("):
                    env_end = rust_delimiter_end(tokens, index + 8, "(", ")")
                    concat_end = rust_delimiter_end(tokens, index + 5, "(", ")")
                    if (
                        env_end == index + 10
                        and concat_end is not None
                        and tokens[index + 9] == "<literal>"
                        and tokens[env_end + 1 : env_end + 3] == (",", "<literal>")
                        and tokens[env_end + 3 : concat_end] in {(), (",",)}
                        and rust_literal_string(text, spans[index + 9]) == "CARGO_MANIFEST_DIR"
                    ):
                        suffix = rust_literal_string(text, spans[env_end + 2])
                        if suffix is not None and suffix.startswith("/src/"):
                            child = (rust_root / suffix[len("/src/") :]).resolve()
                if child not in paths:
                    ambiguous.add(path.resolve())
                else:
                    declarations.append((path.resolve(), child, scopes[index], None))
                continue

            if tokens[index : index + 2] != ("#", "["):
                continue
            attr_end = rust_delimiter_end(tokens, index + 1, "[", "]")
            if attr_end is None:
                ambiguous.add(path.resolve())
                continue
            if tokens[index + 2 : index + 3] not in {("path",), ("cfg_attr",)}:
                continue
            path_values: list[str] = []
            for at in range(index + 2, attr_end):
                if tokens[at : at + 2] != ("path", "="):
                    continue
                value = rust_literal_string(text, spans[at + 2]) if at + 2 < len(spans) else None
                if value is None:
                    ambiguous.add(path.resolve())
                else:
                    path_values.append(value)
            if not path_values:
                continue
            cursor = attr_end + 1
            while tokens[cursor : cursor + 2] == ("#", "["):
                other_end = rust_delimiter_end(tokens, cursor + 1, "[", "]")
                if other_end is None:
                    ambiguous.add(path.resolve())
                    break
                cursor = other_end + 1
            if tokens[cursor : cursor + 1] == ("pub",):
                cursor += 1
                if tokens[cursor : cursor + 1] == ("(",):
                    visibility_end = rust_delimiter_end(tokens, cursor, "(", ")")
                    cursor = visibility_end + 1 if visibility_end is not None else len(tokens)
            if (
                tokens[cursor : cursor + 1] != ("mod",)
                or cursor + 1 >= len(tokens)
                or not re.fullmatch(r"[A-Za-z_][A-Za-z_0-9]*", tokens[cursor + 1])
                or tokens[cursor + 2 : cursor + 3] != (";",)
            ):
                ambiguous.add(path.resolve())
                continue
            if scopes[index]:
                ambiguous.add(path.resolve())
                continue
            for child_name in path_values:
                child = (path.parent / child_name).resolve()
                if child not in paths:
                    ambiguous.add(path.resolve())
                else:
                    declarations.append((path.resolve(), child, scopes[index], tokens[cursor + 1]))

    for _ in range(len(paths) + 1):
        explicit: dict[Path, set[tuple[str, ...]]] = defaultdict(set)
        for parent, child, inline_scope, name in declarations:
            if parent not in ambiguous:
                explicit[child].add(paths[parent] + inline_scope + ((name,) if name else ()))
        changed = False
        for child, candidates in explicit.items():
            if len(candidates) != 1:
                ambiguous.add(child)
                continue
            resolved = next(iter(candidates))
            if paths[child] != resolved:
                paths[child] = resolved
                changed = True
        if not changed:
            break
    else:
        ambiguous.update(child for _, child, _, _ in declarations)
    aliases: dict[str, tuple[str, ...]] = {}
    crate_root = (rust_root / "lib.rs").resolve()
    for parent, child, inline_scope, name in declarations:
        if parent != crate_root or parent in ambiguous or inline_scope or name is None:
            continue
        relative = child.relative_to(rust_root.resolve()).parts
        if not relative or relative[0] not in {"app", "features", "platform", "core"}:
            continue
        if len(relative) == 2 and relative[1] == "mod.rs":
            prefix = (relative[0],)
        elif len(relative) >= 3:
            prefix = (relative[0], relative[1])
        else:
            prefix = (relative[0], Path(relative[1]).stem)
        prior = aliases.get(name)
        if prior is not None and prior != prefix:
            ambiguous.add(parent)
        aliases[name] = prefix
    return paths, ambiguous, aliases


def rust_use_token_spans(tokens: tuple[str, ...]) -> list[tuple[int, int]]:
    spans: list[tuple[int, int]] = []
    index = 0
    while index < len(tokens):
        if tokens[index] != "use":
            index += 1
            continue
        end = index + 1
        depth = 0
        while end < len(tokens):
            depth += (tokens[end] == "{") - (tokens[end] == "}")
            if tokens[end] == ";" and depth == 0:
                break
            end += 1
        spans.append((index, end))
        index = end + 1
    return spans


def relative_rust_path(
    parts: tuple[str, ...], module_path: tuple[str, ...]
) -> tuple[str, ...] | None:
    """Resolve `self::`/`super::` from the source file's real module."""
    if not parts or parts[0] not in {"self", "super"}:
        return None
    cursor = 1
    base = module_path
    if parts[0] == "super":
        if not base:
            return None
        base = base[:-1]
        while cursor < len(parts) and parts[cursor] == "super":
            if not base:
                return None
            base = base[:-1]
            cursor += 1
    return base + parts[cursor:]


def canonical_crate_module_path(
    path: tuple[str, ...], aliases: dict[str, tuple[str, ...]]
) -> tuple[str, ...]:
    return aliases[path[0]] + path[1:] if path and path[0] in aliases else path


def crate_references(
    text: str,
    aliases: dict[str, tuple[str, ...]],
    module_path: tuple[str, ...] = (),
) -> list[tuple[str, str]]:
    """Find crate-layer references after lexical normalization of Rust paths."""
    refs: list[tuple[str, str]] = []
    tokens = rust_syntax_tokens(text)
    scopes = rust_inline_module_paths(tokens, module_path)
    import_spans = rust_use_token_spans(tokens)
    root_layers = {"app", "features", "platform", "core"}
    for index, token in enumerate(tokens):
        if token in {"self", "super"} and tokens[index + 1 : index + 3] == (":", ":"):
            if any(start <= index <= end for start, end in import_spans):
                continue
            if tokens[index - 2 : index] == (":", ":"):
                continue
            parts = [token]
            cursor = index + 3
            while cursor < len(tokens) and re.fullmatch(r"[A-Za-z_][A-Za-z_0-9]*", tokens[cursor]):
                parts.append(tokens[cursor])
                cursor += 1
                if tokens[cursor : cursor + 2] != (":", ":"):
                    break
                cursor += 2
            canonical = relative_rust_path(tuple(parts), scopes[index])
            if canonical:
                canonical = canonical_crate_module_path(canonical, aliases)
            if canonical and len(canonical) >= 2 and canonical[0] in root_layers:
                refs.append((canonical[0], canonical[1]))
        if token != "crate" or tokens[index + 1 : index + 3] != (":", ":"):
            continue
        first = tokens[index + 3 : index + 4]
        if not first:
            continue
        target = aliases.get(first[0])
        if first[0] in aliases:
            if not any(start <= index <= end for start, end in import_spans):
                path = list(target)
                cursor = index + 4
                while tokens[cursor : cursor + 2] == (":", ":") and re.fullmatch(
                    r"[A-Za-z_][A-Za-z_0-9]*", tokens[cursor + 2] if cursor + 2 < len(tokens) else ""
                ):
                    path.append(tokens[cursor + 2])
                    cursor += 3
                if len(path) >= 2:
                    refs.append((path[0], path[1]))
                elif len(path) == 1:
                    refs.append((path[0], ROOT_MODULE))
        elif (
            first[0] in root_layers
            and tokens[index + 4 : index + 6] == (":", ":")
            and tokens[index + 6 : index + 7]
            and re.fullmatch(r"[A-Za-z_][A-Za-z_0-9]*", tokens[index + 6])
        ):
            refs.append((first[0], tokens[index + 6]))

    refs.extend(crate_root_alias_references(text, aliases, module_path))
    return refs


def crate_root_alias_references(
    text: str,
    aliases: dict[str, tuple[str, ...]] | None = None,
    module_path: tuple[str, ...] = (),
    exported_aliases: dict[str, tuple[str, ...]] | None = None,
) -> list[tuple[str, str]]:
    """Resolve `use` trees and local aliases of crate roots.

    Direct paths are counted by ``crate_references``. Grouped imports and
    imported root aliases need their own pass because the module name can be
    separated from ``crate::`` or subsequently hidden behind ``self``/``super``.
    """
    aliases = aliases or {}
    tokens = rust_syntax_tokens(text)
    scopes = rust_inline_module_paths(tokens, module_path)
    brace_depths = rust_brace_depths(tokens)
    root_aliases: dict[str, tuple[str, ...]] = {}
    refs: list[tuple[str, str]] = []
    import_spans = rust_use_token_spans(tokens)
    root_layers = {"app", "features", "platform", "core"}

    def names(parts: tuple[str, ...]) -> tuple[str, ...]:
        return tuple(part for part in parts if re.fullmatch(r"[A-Za-z_][A-Za-z_0-9]*", part))

    def use_items(parts: tuple[str, ...], prefix: tuple[str, ...] = ()) -> list[tuple[tuple[str, ...], str]]:
        if "{" in parts:
            opening = parts.index("{")
            head = names(parts[:opening])
            depth = 1
            closing = opening + 1
            while closing < len(parts) and depth:
                depth += (parts[closing] == "{") - (parts[closing] == "}")
                closing += 1
            if depth:
                return []
            nested = parts[opening + 1 : closing - 1]
            result: list[tuple[tuple[str, ...], str]] = []
            start = 0
            depth = 0
            for index, part in enumerate(nested + (",",)):
                depth += (part == "{") - (part == "}")
                if part == "," and depth == 0:
                    result.extend(use_items(nested[start:index], prefix + head))
                    start = index + 1
            return result
        if not parts:
            return []
        if "*" in parts:
            path = prefix + names(parts[: parts.index("*")])
            return [(path, "*")] if path else []
        alias_at = parts.index("as") if "as" in parts else len(parts)
        path = prefix + names(parts[:alias_at])
        if not path:
            return []
        if path[-1] == "self":
            path = path[:-1]
        if not path:
            return []
        local = parts[alias_at + 1] if alias_at < len(parts) and alias_at + 1 < len(parts) else path[-1]
        return [(path, local)]

    def canonical_path(
        path: tuple[str, ...], scope: tuple[str, ...]
    ) -> tuple[str, ...] | None:
        if path[:1] == ("crate",):
            rest = path[1:]
            return aliases[rest[0]] + rest[1:] if rest and rest[0] in aliases else rest
        if path[0] in root_aliases:
            return root_aliases[path[0]] + path[1:]
        if not scope and path[0] in root_layers:
            return path
        if not scope and path[0] in aliases:
            return aliases[path[0]] + path[1:]
        if path[0] == "self" and len(path) > 1 and path[1] in root_aliases:
            return root_aliases[path[1]] + path[2:]
        if path[0] in {"self", "super"}:
            resolved = relative_rust_path(path, scope)
            return canonical_crate_module_path(resolved, aliases) if resolved else None
        return None

    index = 0
    while index < len(tokens):
        if tokens[index : index + 3] == ("extern", "crate", "self"):
            if exported_aliases is not None and brace_depths[index] != 0:
                index += 3
                continue
            if tokens[index + 3 : index + 4] == ("as",):
                local = tokens[index + 4 : index + 5]
                if local and re.fullmatch(r"[A-Za-z_][A-Za-z_0-9]*", local[0]):
                    root_aliases[local[0]] = ()
                    if exported_aliases is not None and brace_depths[index] == 0:
                        exported_aliases[local[0]] = ()
            index += 3
            continue
        if tokens[index] != "use":
            index += 1
            continue
        end = index + 1
        depth = 0
        while end < len(tokens):
            depth += (tokens[end] == "{") - (tokens[end] == "}")
            if tokens[end] == ";" and depth == 0:
                break
            end += 1
        if exported_aliases is not None and brace_depths[index] != 0:
            index = end + 1
            continue
        declaration = tokens[index + 1 : end]
        for path, local in use_items(tokens[index + 1 : end]):
            canonical = canonical_path(path, scopes[index])
            if canonical is None:
                continue
            if (
                exported_aliases is not None
                and brace_depths[index] == 0
                and local not in {"*", "_"}
                and canonical
                and canonical[0] in root_layers
            ):
                exported_aliases[local] = canonical
            direct_import = (
                "crate", ":", ":", canonical[0], ":", ":", canonical[1]
            ) if len(canonical) >= 2 else ()
            already_counted = bool(direct_import) and any(
                declaration[offset : offset + len(direct_import)] == direct_import
                for offset in range(len(declaration))
            )
            if (
                len(canonical) >= 2
                and canonical[0] in root_layers
                and not already_counted
            ):
                refs.append((canonical[0], canonical[1]))
            elif local != "*" and len(canonical) == 1 and canonical[0] in root_layers:
                # Importing the parent module is a dependency even when its
                # local alias is never used in this source file. Children may
                # inherit the import, and its contents are not statically
                # bounded here (notably for the feature registry).
                refs.append((canonical[0], ROOT_MODULE))
            if (
                local == "*"
                and (not canonical or len(canonical) == 1)
                and (
                    path[0] == "crate"
                    or path[0] in root_aliases
                    or canonical in {(layer,) for layer in root_layers}
                )
            ):
                refs.append((canonical[0] if canonical else "crate", "*"))
                continue
            if local != "_" and (not canonical or (len(canonical) == 1 and canonical[0] in root_layers)):
                root_aliases[local] = canonical
        index = end + 1

    for index, token in enumerate(tokens):
        if any(start <= index <= end for start, end in import_spans):
            continue
        if tokens[index - 2 : index] == (":", ":") and tokens[index - 3 : index - 2] == ("super",):
            continue
        prefix = root_aliases.get(token)
        if prefix is None or tokens[index + 1 : index + 3] != (":", ":"):
            continue
        path = list(prefix)
        cursor = index + 3
        while cursor < len(tokens) and re.fullmatch(r"[A-Za-z_][A-Za-z_0-9]*", tokens[cursor]):
            path.append(tokens[cursor])
            cursor += 1
            if tokens[cursor : cursor + 2] != (":", ":"):
                break
            cursor += 2
        if len(path) >= 2 and path[0] in root_layers:
            refs.append((path[0], path[1]))
    return refs


def rust_root_macro_alias_risk(text: str) -> int:
    """Count root macro invocations whose expansion could introduce aliases.

    An uninvoked macro_rules definition is inert; its body must not be parsed
    as real root items. Without expansion, any top-level invocation is unknown.
    """
    tokens = rust_syntax_tokens(text)
    depths = rust_brace_depths(tokens)
    return sum(
        depths[index] == 0
        and token != "macro_rules"
        and re.fullmatch(r"[A-Za-z_][A-Za-z_0-9]*", token) is not None
        and tokens[index + 1 : index + 2] == ("!",)
        and tokens[index + 2 : index + 3] in {("(",), ("[",), ("{",)}
        for index, token in enumerate(tokens)
    )


def frontend_import_target(specifier: str, source: Path, src_root: Path) -> Path | None:
    if specifier.startswith("."):
        return (source.parent / specifier).resolve()
    if specifier.startswith("@/"):
        return (src_root / specifier[2:]).resolve()
    if specifier.startswith("src/"):
        return (src_root.parent / specifier).resolve()
    return None


def scan_frontend(root: Path) -> dict[str, Counter[str]]:
    rules: dict[str, Counter[str]] = {
        "frontend_feature_imports_app": Counter(),
        "frontend_tauri_global_outside_platform": Counter(),
        "frontend_user_agent_platform_detection": Counter(),
    }
    src_root = (root / "pinvou3-app/src").resolve()
    feature_root = (src_root / "features").resolve()
    app_root = (src_root / "app").resolve()
    platform_root = (src_root / "platform").resolve()
    import_patterns = [
        re.compile(
            r"(?:^|\n)\s*(?:import|export)\s+"
            r"(?:[A-Za-z0-9_$*,{}\s]+?\s+from\s+)?[\"']([^\"']+)[\"']"
        ),
        re.compile(r"\bimport\s*\(\s*[\"']([^\"']+)[\"']\s*\)"),
    ]
    tauri_pattern = re.compile(r"\b(?:window|globalThis)\s*\.\s*__TAURI__\b")
    user_agent_pattern = re.compile(
        r"\b(?:window\s*\.\s*)?navigator\s*\.\s*(?:userAgent|userAgentData|platform)\b"
    )

    for path in source_files(root, "pinvou3-app/src", FRONTEND_SUFFIXES):
        text = read_text(path)
        relative = normalize(path, root)
        resolved = path.resolve()
        if resolved.is_relative_to(feature_root):
            for pattern in import_patterns:
                for match in pattern.finditer(text):
                    target = frontend_import_target(match.group(1), resolved, src_root)
                    if target and target.is_relative_to(app_root):
                        feature = resolved.relative_to(feature_root).parts[0]
                        rules["frontend_feature_imports_app"][f"{feature}->{match.group(1)}"] += 1
        if not resolved.is_relative_to(platform_root):
            count = len(tauri_pattern.findall(text))
            if count:
                rules["frontend_tauri_global_outside_platform"][relative] += count
        count = len(user_agent_pattern.findall(text))
        if count:
            rules["frontend_user_agent_platform_detection"][relative] += count
    return rules


def rust_syntax_token_spans(text: str) -> tuple[tuple[str, int, int], ...]:
    """Tokenize Rust declarations while ignoring comments and literal contents.

    This deliberately small lexer supports nested block comments, raw strings,
    raw identifiers, characters and byte characters. Lifetimes remain tokens.
    """
    tokens: list[tuple[str, int, int]] = []
    raw_literal = re.compile(r'(?:b|c)?r(#{0,255})"')
    raw_identifier = re.compile(r"r#([A-Za-z_][A-Za-z_0-9]*)")
    identifier = re.compile(r"[A-Za-z_][A-Za-z_0-9]*")

    def character_end(start: int) -> int | None:
        """Return the end of one valid char/byte-char token, not a lifetime."""
        body = start + (2 if text.startswith("b'", start) else 1)
        if body >= len(text) or text[body] in {"'", "\r", "\n"}:
            return None
        if text[body] == "\\":
            body += 1
            if body >= len(text):
                return None
            if text[body] == "x":
                if not re.fullmatch(r"[0-9a-fA-F]{2}", text[body + 1 : body + 3]):
                    return None
                body += 3
            elif text.startswith("u{", body):
                closing = text.find("}", body + 2)
                digits = text[body + 2 : closing] if closing >= 0 else ""
                if (
                    not re.fullmatch(r"[0-9a-fA-F_]+", digits)
                    or not 1 <= sum(char != "_" for char in digits) <= 6
                ):
                    return None
                body = closing + 1
            elif text[body] in {"0", "n", "r", "t", "\\", "'", '"'}:
                body += 1
            else:
                return None
        else:
            body += 1
        return body + 1 if body < len(text) and text[body] == "'" else None

    index = 0
    while index < len(text):
        if text[index].isspace():
            index += 1
            continue
        if text.startswith("//", index):
            end = text.find("\n", index + 2)
            index = len(text) if end < 0 else end + 1
            continue
        if text.startswith("/*", index):
            depth = 1
            index += 2
            while index < len(text) and depth:
                if text.startswith("/*", index):
                    depth += 1
                    index += 2
                elif text.startswith("*/", index):
                    depth -= 1
                    index += 2
                else:
                    index += 1
            continue
        raw = raw_literal.match(text, index)
        if raw:
            start = index
            terminator = '"' + raw.group(1)
            end = text.find(terminator, raw.end())
            index = len(text) if end < 0 else end + len(terminator)
            tokens.append(("<literal>", start, index))
            continue
        raw_word = raw_identifier.match(text, index)
        if raw_word:
            tokens.append((raw_word.group(1), index, raw_word.end()))
            index = raw_word.end()
            continue
        if text[index] == '"':
            start = index
            index += 1
            while index < len(text):
                if text[index] == "\\":
                    index += 2
                elif text[index] == '"':
                    index += 1
                    break
                else:
                    index += 1
            tokens.append(("<literal>", start, index))
            continue
        if text[index] == "'" or text.startswith("b'", index):
            end = character_end(index)
            if end is not None:
                tokens.append(("<literal>", index, end))
                index = end
                continue
        word = identifier.match(text, index)
        if word:
            tokens.append((word.group(), index, word.end()))
            index = word.end()
            continue
        tokens.append((text[index], index, index + 1))
        index += 1
    return tuple(tokens)


def rust_syntax_tokens(text: str) -> tuple[str, ...]:
    return tuple(token for token, _, _ in rust_syntax_token_spans(text))


def session_application_exports(text: str) -> tuple[tuple[str, ...], ...]:
    """Extract all public `mod` and `use` declarations, including multiline ones."""
    tokens = rust_syntax_tokens(text)
    exports: list[tuple[str, ...]] = []
    index = 0
    while index < len(tokens):
        if tokens[index] != "pub":
            index += 1
            continue
        end = index + 1
        if end < len(tokens) and tokens[end] == "(":
            depth = 1
            end += 1
            while end < len(tokens) and depth:
                depth += (tokens[end] == "(") - (tokens[end] == ")")
                end += 1
        if end >= len(tokens) or tokens[end] not in {"mod", "use"}:
            index += 1
            continue
        kind = tokens[end]
        end += 1
        depth = 0
        while end < len(tokens):
            token = tokens[end]
            if token in {"{", "(", "["}:
                depth += 1
            elif token in {"}", ")", "]"}:
                depth -= 1
            if token == ";" and depth == 0:
                end += 1
                break
            if token == "{" and depth == 1 and kind == "mod":
                # An inline public module differs from the frozen declaration.
                end += 1
                break
            end += 1
        exports.append(tokens[index:end])
        index = end
    return tuple(exports)


def session_application_export_surface_is_frozen(text: str) -> bool:
    """Keep the five declarations unconditional and forbid top-level expansion.

    Inspect only top-level items so the ordinary test module can still contain
    attributes and local macros. A macro at the root could add exports which a
    source-level declaration scan cannot see, so fail closed on that syntax.
    """
    tokens = rust_syntax_tokens(text)
    if session_application_exports(text) != tuple(
        rust_syntax_tokens(item) for item in SESSION_APPLICATION_EXPORTS
    ):
        return False

    brace_depth = 0
    preceding_attribute = False
    index = 0
    while index < len(tokens):
        token = tokens[index]
        if brace_depth == 0:
            if token == "#" and tokens[index + 1 : index + 2] == ("[",):
                brackets = 1
                index += 2
                while index < len(tokens) and brackets:
                    brackets += (tokens[index] == "[") - (tokens[index] == "]")
                    index += 1
                if brackets:
                    return False
                preceding_attribute = True
                continue
            if token == "pub" and preceding_attribute:
                return False
            if (
                re.fullmatch(r"[A-Za-z_][A-Za-z_0-9]*", token)
                and tokens[index + 1 : index + 2] == ("!",)
                and tokens[index + 2 : index + 3] in {("(",), ("[",), ("{",)}
            ):
                return False
            preceding_attribute = False
        if token == "{":
            brace_depth += 1
        elif token == "}":
            brace_depth -= 1
        index += 1
    return True


def remote_control_host_api_counts(text: str) -> Counter[str]:
    """Count direct host references by mode for the legacy remote feature."""
    counts: Counter[str] = Counter()
    if not any(marker in text for marker in ("tauri", "platform", "app_events", "emit")):
        return counts
    tokens = rust_syntax_tokens(text)
    emitter_import = False
    for index, token in enumerate(tokens):
        path = tokens[index + 1 : index + 3] == (":", ":")
        if re.fullmatch(r"tauri(?:_[A-Za-z0-9_]+)?", token) and path:
            counts["tauri_path"] += 1
        if token == "platform" and path:
            tail = tokens[index + 3 :]
            if tail[:1] == ("app_events",):
                counts["app_events"] += 1
            elif tail[:1] == ("{",):
                closing = tail.index("}") if "}" in tail else len(tail)
                if "app_events" in tail[1:closing]:
                    counts["app_events"] += 1
        if token == "use" or tokens[index : index + 2] == ("extern", "crate"):
            start = index + (2 if token == "extern" else 1)
            if tokens[start : start + 2] == (":", ":"):
                start += 2
            if start < len(tokens) and re.fullmatch(
                r"tauri(?:_[A-Za-z0-9_]+)?", tokens[start]
            ):
                end = start + 1
                while end < len(tokens) and tokens[end] != ";":
                    end += 1
                declaration = tokens[start:end]
                if len(declaration) == 1 and end < len(tokens):
                    counts["tauri_alias"] += 1
                elif len(declaration) >= 2 and declaration[1] == "as":
                    counts["tauri_alias"] += 1
                if token == "use" and "Emitter" in declaration and (":", ":") == declaration[1:3]:
                    emitter_import = True
        if token == "use":
            end = index + 1
            while end < len(tokens) and tokens[end] != ";":
                end += 1
            declaration = tokens[index + 1 : end]
            # A root alias or glob can hide every later host API call behind a
            # local name. Count the import itself as a new forbidden callsite.
            for position, part in enumerate(declaration):
                if re.fullmatch(r"tauri(?:_[A-Za-z0-9_]+)?", part):
                    grouped_self = "self" in declaration[position + 1 :]
                    grouped_root = declaration[:1] == ("{",)
                    named_alias = any(
                        declaration[offset] == "as"
                        and declaration[offset + 1 : offset + 2] != ("_",)
                        for offset in range(position + 1, len(declaration))
                    )
                    if (
                        "*" in declaration
                        or declaration[position + 1 : position + 2] == ("as",)
                        or grouped_self
                        or grouped_root
                        or named_alias
                    ):
                        counts["tauri_import_risk"] += 1
                        emitter_import = True
                        break
            for position, part in enumerate(declaration):
                if part == "platform" and (
                    "*" in declaration
                    or declaration[position + 1 : position + 2] == ("as",)
                    or "self" in declaration[position + 1 :]
                ):
                    counts["app_events_import_risk"] += 1
                    break
                if part == "app_events" and (
                    "*" in declaration
                    or declaration[position + 1 : position + 2] == ("as",)
                    or "self" in declaration[position + 1 :]
                ):
                    counts["app_events_import_risk"] += 1
                    break
        if token == "." and tokens[index + 1 : index + 2] in {
            ("emit",), ("emit_to",), ("emit_all",)
        }:
            tail = tokens[index + 2 :]
            direct_call = tail[:1] == ("(",)
            generic_call = tail[:3] == (":", ":", "<")
            if direct_call or generic_call:
                counts["host_emit"] += 1
    if not emitter_import:
        counts.pop("host_emit", None)
    return +counts


def strongly_connected_components(graph: dict[str, set[str]]) -> list[list[str]]:
    index = 0
    indices: dict[str, int] = {}
    lowlinks: dict[str, int] = {}
    stack: list[str] = []
    on_stack: set[str] = set()
    components: list[list[str]] = []

    def visit(node: str) -> None:
        nonlocal index
        indices[node] = index
        lowlinks[node] = index
        index += 1
        stack.append(node)
        on_stack.add(node)
        for target in sorted(graph.get(node, set())):
            if target not in indices:
                visit(target)
                lowlinks[node] = min(lowlinks[node], lowlinks[target])
            elif target in on_stack:
                lowlinks[node] = min(lowlinks[node], indices[target])
        if lowlinks[node] == indices[node]:
            component: list[str] = []
            while stack:
                target = stack.pop()
                on_stack.remove(target)
                component.append(target)
                if target == node:
                    break
            if len(component) > 1 or node in graph.get(node, set()):
                components.append(sorted(component))

    nodes = set(graph)
    for targets in graph.values():
        nodes.update(targets)
    for node in sorted(nodes):
        if node not in indices:
            visit(node)
    return sorted(components)


def target_cfg_allowed(relative: str) -> bool:
    # lib.rs is the composition root; platform selection is legitimate there.
    return "/platform/" in f"/{relative}" or relative.endswith("/src/lib.rs")


def platform_detail_allowed(relative: str) -> bool:
    return "/platform/" in f"/{relative}"


def file_exception_allowed(text: str, exception: str) -> bool:
    """Return whether a reasoned file-level exception appears near the header."""
    header = "\n".join(text.splitlines()[:20])
    pattern = re.compile(
        rf"(?m)^[ \t]*//[ \t]*architecture-guard:[ \t]*"
        rf"allow-{re.escape(exception)}[ \t]+--[ \t]+\S[^\r\n]*$"
    )
    return bool(pattern.search(header))


def count_platform_cfgs(text: str) -> int:
    """Count platform selectors inside cfg/cfg_attr expressions.

    Rust accepts both key/value selectors (`target_os = "windows"`,
    `target_arch = "aarch64"`, ...) and shorthand predicates (`windows`,
    `unix`).  Keep the scanner deliberately syntax-light, but balance nested
    parentheses so `all(...)`, `any(...)` and `not(...)` are covered without
    matching ordinary prose or identifiers outside cfg expressions.
    """
    invocation = re.compile(r"\b(?:cfg|cfg_attr)\s*!?\s*\(")
    target_selector = re.compile(
        r"\b(?:target_os|target_arch|target_family|target_env|target_vendor|"
        r"target_endian|target_pointer_width|target_abi)\s*="
    )
    shorthand_selector = re.compile(r"(?<![A-Za-z0-9_])(?:windows|unix)(?![A-Za-z0-9_])")
    count = 0
    for match in invocation.finditer(text):
        cursor = match.end()
        depth = 1
        while cursor < len(text) and depth:
            if text[cursor] == "(":
                depth += 1
            elif text[cursor] == ")":
                depth -= 1
            cursor += 1
        if depth:
            continue
        expression = text[match.end() : cursor - 1]
        count += len(target_selector.findall(expression))
        expression_without_strings = re.sub(r'"(?:\\.|[^"\\])*"', '""', expression)
        count += len(shorthand_selector.findall(expression_without_strings))
    return count


def scan_rust(
    root: Path, *, include_feature_edges: bool = False
) -> tuple[dict[str, Counter[str]], list[list[str]]] | tuple[
    dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]
]:
    rules: dict[str, Counter[str]] = {
        "rust_feature_depends_on_app": Counter(),
        "rust_crate_root_glob_imports": Counter(),
        "rust_crate_root_alias_ambiguous": Counter(),
        "rust_module_path_ambiguous": Counter(),
        "rust_platform_depends_on_upper_layer": Counter(),
        "rust_cyclic_feature_dependencies": Counter(),
        "rust_target_cfg_outside_adapter": Counter(),
        "rust_platform_details_outside_adapter": Counter(),
        "rust_tauri_commands_outside_app": Counter(),
        "rust_tauri_handler_outside_app": Counter(),
        "rust_tauri_api_in_application": Counter(),
        "rust_remote_control_host_api": Counter(),
        "rust_session_application_export_drift": Counter(),
        "rust_external_group_kill_spawn": Counter(),
        "rust_cli_reaches_past_pinvou3_lib": Counter(),
    }
    rust_root = root / "pinvou3-app/src-tauri/src"
    rust_files = list(source_files(root, "pinvou3-app/src-tauri/src", {".rs"}))
    module_paths, ambiguous_module_paths, aliases = rust_module_paths(rust_files, rust_root)
    lib_rs = (rust_root / "lib.rs").resolve()
    if lib_rs in module_paths:
        exported: dict[str, tuple[str, ...]] = {}
        lib_text = read_text(lib_rs)
        crate_root_alias_references(lib_text, aliases, (), exported)
        macro_risk = rust_root_macro_alias_risk(lib_text)
        if macro_risk:
            rules["rust_crate_root_alias_ambiguous"][normalize(lib_rs, root)] += macro_risk
        for local, canonical in exported.items():
            if local in aliases and aliases[local] != canonical:
                ambiguous_module_paths.add(lib_rs)
            aliases[local] = canonical
    graph: dict[str, set[str]] = defaultdict(set)
    feature_edge_counts: Counter[tuple[str, str]] = Counter()
    tauri_command_pattern = re.compile(r"#\s*\[\s*tauri\s*::\s*command\b")
    tauri_handler_pattern = re.compile(r"generate_handler\s*!\s*\[(.*?)\]", re.DOTALL)
    # Process-group kills must use kill(2) directly. Spawning an external
    # `kill` binary is unsafe because affected procps-ng versions can parse a
    # valid negative process-group id as kill(-1).
    external_group_kill_patterns = [
        re.compile(r'Command\s*::\s*new\s*\(\s*"(?:[^"]*/)?kill"'),
        re.compile(r'::\s*new\s*\(\s*"(?:[^"]*/)?kill"\s*\)'),
        re.compile(r'Path\s*::\s*new\s*\(\s*"(?:[^"]*/)?kill"'),
        re.compile(r'connector_cli_command\s*\(\s*[^,()]*,\s*"kill"'),
    ]
    platform_detail_patterns = [
        re.compile(r"\bpowershell(?:\.exe)?\b", re.IGNORECASE),
        re.compile(r"\bxdg-open\b"),
        re.compile(r"\bDBUS_SESSION_BUS_ADDRESS\b"),
        re.compile(r"\bORT_DYLIB_PATH\b"),
        re.compile(r"\bWEBKIT_(?:DISABLE|DMABUF|FORCE|WEB_RENDER)[A-Z0-9_]*\b"),
        re.compile(r"\b(?:HKEY_[A-Z_]+|Win32::System::Registry)\b"),
        re.compile(
            r'Command\s*::\s*new\s*\(\s*"(?:open|explorer(?:\.exe)?|reg(?:\.exe)?)"'
        ),
    ]

    for path in rust_files:
        text = read_text(path)
        relative = normalize(path, root)
        resolved = path.resolve()
        module_path = module_paths[resolved]
        layer_path = canonical_crate_module_path(module_path, aliases)
        if resolved in ambiguous_module_paths:
            rules["rust_module_path_ambiguous"][relative] += 1
        references = crate_references(text, aliases, module_path)
        for layer, target in references:
            if target == "*":
                rules["rust_crate_root_glob_imports"][relative] += 1
            elif layer == "features" and target == ROOT_MODULE:
                rules["rust_crate_root_glob_imports"][relative] += 1
        if len(layer_path) >= 2 and layer_path[0] == "features":
            source_feature = layer_path[1]
            application_file = len(layer_path) >= 3 and layer_path[2] == "application"
            host_api_counts = (
                remote_control_host_api_counts(text)
                if application_file or source_feature == "remote_control"
                else Counter()
            )
            tauri_api_count = sum(host_api_counts.values())
            if application_file and tauri_api_count:
                rules["rust_tauri_api_in_application"][relative] += tauri_api_count
            if (
                resolved == (rust_root / "features/sessions/application/mod.rs").resolve()
                and not session_application_export_surface_is_frozen(text)
            ):
                rules["rust_session_application_export_drift"][relative] += 1
            if source_feature == "remote_control":
                for mode, count in host_api_counts.items():
                    rules["rust_remote_control_host_api"][f"{relative}:{mode}"] += count
            for layer, target in references:
                if target == "*" or (layer == "features" and target == ROOT_MODULE):
                    continue
                if layer == "app":
                    rules["rust_feature_depends_on_app"][f"{source_feature}->{target}"] += 1
                elif layer == "features" and target != source_feature:
                    graph[source_feature].add(target)
                    feature_edge_counts[(source_feature, target)] += 1
        if layer_path[:1] == ("platform",):
            for layer, target in references:
                if target == "*":
                    continue
                if layer in {"app", "features"}:
                    rules["rust_platform_depends_on_upper_layer"][
                        f"{relative}->{layer}::{target}"
                    ] += 1
        target_count = count_platform_cfgs(text)
        if (
            target_count
            and not target_cfg_allowed(relative)
            and not file_exception_allowed(text, "target-cfg")
        ):
            rules["rust_target_cfg_outside_adapter"][relative] += target_count
        if (
            not platform_detail_allowed(relative)
            and not file_exception_allowed(text, "platform-detail")
        ):
            detail_count = sum(
                len(pattern.findall(text)) for pattern in platform_detail_patterns
            )
            if detail_count:
                rules["rust_platform_details_outside_adapter"][relative] += detail_count
        command_count = len(tauri_command_pattern.findall(text))
        if command_count and "/app/commands/" not in f"/{relative}":
            rules["rust_tauri_commands_outside_app"][relative] += command_count
        external_kill_count = sum(
            len(pattern.findall(text)) for pattern in external_group_kill_patterns
        )
        if external_kill_count:
            rules["rust_external_group_kill_spawn"][relative] += external_kill_count
        for handler in tauri_handler_pattern.findall(text):
            for entry in re.findall(
                r"(?:^|,)\s*([A-Za-z_][A-Za-z0-9_:]*)\s*(?=,|$)", handler
            ):
                if not entry.startswith("commands::") and not entry.startswith(
                    "crate::app::commands::"
                ):
                    rules["rust_tauri_handler_outside_app"][f"{relative}:{entry}"] += 1
    # The CLI crates reach the app through the pinvou3_lib surface only: no
    # direct foundation (deepseek_tui) or Tauri references, so the headless
    # build cannot silently couple to the GUI stack or skip the app's own
    # layering. Comment lines are ignored (docs may name the crates). The
    # `use`-statement patterns catch every import form (plain, `as`-aliased,
    # glob); the qualified-path patterns catch inline references.
    cli_reference_patterns = [
        re.compile(r"\bdeepseek_tui\s*::"),
        re.compile(r"\btauri\s*::\s*[A-Za-z_{]"),
        re.compile(r"\buse\s+(tauri|deepseek_tui)\s*(::|;|as\b)"),
    ]
    for path in source_files(root, "pinvou-cli/crates", {".rs"}):
        text = read_text(path)
        code_lines = "\n".join(
            line for line in text.splitlines() if not line.lstrip().startswith("//")
        )
        reference_count = sum(
            len(pattern.findall(code_lines)) for pattern in cli_reference_patterns
        )
        if reference_count:
            rules["rust_cli_reaches_past_pinvou3_lib"][normalize(path, root)] += (
                reference_count
            )

    cycles = strongly_connected_components(graph)
    for source_target, count in feature_edge_counts.items():
        source, target = source_target
        if any({source, target}.issubset(set(cycle)) for cycle in cycles):
            rules["rust_cyclic_feature_dependencies"][f"{source}->{target}"] = count
    if include_feature_edges:
        return rules, cycles, feature_edge_counts
    return rules, cycles


def scan_resources(root: Path) -> dict[str, Counter[str]]:
    rules = {"common_platform_binaries": Counter()}
    common_root = root / "pinvou3-app/src-tauri/resources/common"
    if not common_root.exists():
        return rules
    for path in common_root.rglob("*"):
        if not path.is_file():
            continue
        # 包管理器按 lockfile 生成的依赖不会提交，由独立打包契约负责校验。
        if "node_modules" in path.parts:
            continue
        try:
            magic = path.read_bytes()[:4]
        except OSError:
            continue
        if magic.startswith(b"MZ") or magic == b"\x7fELF":
            rules["common_platform_binaries"][normalize(path, root)] += 1
    return rules


def current_state(root: Path) -> dict:
    frontend_rules = scan_frontend(root)
    rust_rules, cycles = scan_rust(root)
    resource_rules = scan_resources(root)
    rules = {**frontend_rules, **rust_rules, **resource_rules}
    serializable_rules = {
        name: dict(sorted(counts.items())) for name, counts in sorted(rules.items())
    }
    state = {
        "schema_version": SCHEMA_VERSION,
        "generated_from": git_head(root),
        "rules": serializable_rules,
        "rust_feature_cycles": cycles,
    }
    return state


def compare_counts(rule: str, current: dict[str, int], allowed: dict[str, int]) -> tuple[list[str], list[str]]:
    failures: list[str] = []
    progress: list[str] = []
    for key, count in sorted(current.items()):
        limit = allowed.get(key, 0)
        if count > limit:
            failures.append(f"{rule}: {key}: found {count}, baseline allows {limit}")
    for key, limit in sorted(allowed.items()):
        count = current.get(key, 0)
        if count < limit:
            progress.append(f"{rule}: {key}: reduced {limit} -> {count}")
    return failures, progress


def cycle_allowed(cycle: list[str], baseline_cycles: list[list[str]]) -> bool:
    current = set(cycle)
    if len(current) == 1:
        return any(current == set(allowed) for allowed in baseline_cycles)
    return any(current.issubset(set(allowed)) for allowed in baseline_cycles)


def compare(current: dict, baseline: dict) -> tuple[list[str], list[str]]:
    if baseline.get("schema_version") != SCHEMA_VERSION:
        return [
            f"baseline schema mismatch: expected {SCHEMA_VERSION}, "
            f"found {baseline.get('schema_version')}"
        ], []
    failures: list[str] = []
    progress: list[str] = []
    current_rules = current.get("rules", {})
    baseline_rules = baseline.get("rules", {})
    for rule in sorted(set(current_rules) | set(baseline_rules)):
        rule_failures, rule_progress = compare_counts(
            rule, current_rules.get(rule, {}), baseline_rules.get(rule, {})
        )
        failures.extend(rule_failures)
        progress.extend(rule_progress)
    current_cycles = current.get("rust_feature_cycles", [])
    baseline_cycles = baseline.get("rust_feature_cycles", [])
    current_sets = [set(cycle) for cycle in current_cycles]
    baseline_sets = [set(cycle) for cycle in baseline_cycles]
    for cycle, current_set in zip(current_cycles, current_sets):
        if any(current_set == allowed for allowed in baseline_sets):
            continue
        if len(current_set) > 1 and any(current_set < allowed for allowed in baseline_sets):
            progress.append(f"rust_feature_cycles: cycle shrank to {', '.join(cycle)}")
        else:
            failures.append(f"rust_feature_cycles: new or expanded cycle: {' -> '.join(cycle)}")
    for cycle, baseline_set in zip(baseline_cycles, baseline_sets):
        if any(baseline_set == current_set for current_set in current_sets):
            continue
        if not any(current_set < baseline_set for current_set in current_sets):
            progress.append(f"rust_feature_cycles: removed cycle containing {', '.join(cycle)}")
    return failures, progress


def should_fail(failures: list[str], baseline_updates: list[str]) -> bool:
    return bool(failures or baseline_updates)


def merged_migration_limits(
    merge_state: tuple[dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]],
    anchor_state: tuple[dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]],
    target_state: tuple[dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]],
) -> tuple[dict[str, Counter[str]], list[list[str]]]:
    """Three-way semantic ratchet for the task's one-time legacy migration."""
    merge_rules, _, merge_edges = merge_state
    anchor_rules, _, anchor_edges = anchor_state
    target_rules, _, target_edges = target_state
    limits: dict[str, Counter[str]] = {}
    for rule in ("rust_feature_depends_on_app", "rust_platform_depends_on_upper_layer"):
        merge = merge_rules.get(rule, Counter())
        anchor = anchor_rules.get(rule, Counter())
        target = target_rules.get(rule, Counter())
        limits[rule] = Counter({
            key: max(0, target.get(key, 0) + anchor.get(key, 0) - merge.get(key, 0))
            for key in merge.keys() | anchor.keys() | target.keys()
        })
    edges = Counter({
        edge: max(0, target_edges.get(edge, 0) + anchor_edges.get(edge, 0) - merge_edges.get(edge, 0))
        for edge in merge_edges.keys() | anchor_edges.keys() | target_edges.keys()
    })
    graph: dict[str, set[str]] = defaultdict(set)
    for (source, target), count in edges.items():
        if count:
            graph[source].add(target)
    cycles = strongly_connected_components(graph)
    limits["rust_cyclic_feature_dependencies"] = Counter({
        f"{source}->{target}": count
        for (source, target), count in edges.items()
        if count and any({source, target}.issubset(set(cycle)) for cycle in cycles)
    })
    return limits, cycles


def compare_baseline_ratchet(
    candidate: dict,
    previous: dict,
    merge_rust_state: tuple[dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]] | None = None,
    anchor_rust_state: tuple[dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]] | None = None,
    target_rust_state: tuple[dict[str, Counter[str]], list[list[str]], Counter[tuple[str, str]]] | None = None,
) -> list[str]:
    """Reject baseline increases relative to the PR target branch."""
    failures: list[str] = []
    if previous.get("schema_version") != candidate.get("schema_version"):
        failures.append(
            "baseline schema changed; migrate the guard and baseline in an explicitly "
            "reviewed architecture change"
        )
        return failures
    previous_rules = previous.get("rules", {})
    previous_migrations = set(previous.get("migrations", []))
    candidate_migrations = set(candidate.get("migrations", []))
    previous_anchors = previous.get("migration_anchors", {})
    candidate_anchors = candidate.get("migration_anchors", {})
    for removed in sorted(previous_migrations - candidate_migrations):
        failures.append(f"baseline ratchet: migration marker removed: {removed}")
    for migration, anchor in previous_anchors.items():
        if candidate_anchors.get(migration) != anchor:
            failures.append(f"baseline ratchet: migration anchor changed: {migration}")
    if (
        RUST_DEPENDENCY_MIGRATION in candidate_migrations
        and candidate_anchors.get(RUST_DEPENDENCY_MIGRATION)
        != RUST_DEPENDENCY_MIGRATION_ANCHOR
    ):
        failures.append("baseline ratchet: invalid Rust dependency migration anchor")
    dependency_migration = (
        RUST_DEPENDENCY_MIGRATION in candidate_migrations
        and RUST_DEPENDENCY_MIGRATION not in previous_migrations
        and candidate_anchors.get(RUST_DEPENDENCY_MIGRATION)
        == RUST_DEPENDENCY_MIGRATION_ANCHOR
        and merge_rust_state is not None
        and anchor_rust_state is not None
        and target_rust_state is not None
    )
    migration_limits, migration_cycles = (
        merged_migration_limits(merge_rust_state, anchor_rust_state, target_rust_state)
        if dependency_migration else ({}, [])
    )
    migrated_edges = {
        "rust_cyclic_feature_dependencies",
        "rust_feature_depends_on_app",
        "rust_platform_depends_on_upper_layer",
    }
    for rule in ("rust_remote_control_host_api", "rust_cyclic_feature_dependencies"):
        if rule in previous_rules and rule not in candidate.get("rules", {}):
            failures.append(f"baseline ratchet: rule removed: {rule}")
    for rule, candidate_counts in candidate.get("rules", {}).items():
        if rule == "rust_remote_control_host_api" and rule not in previous_rules:
            for key, count in candidate_counts.items():
                ceiling = REMOTE_CONTROL_HOST_API_INITIAL_CEILING.get(key, 0)
                if count > ceiling:
                    failures.append(
                        f"baseline ratchet: {rule}: {key}: candidate allows {count}, "
                        f"initial legacy ceiling allows {ceiling}"
                    )
            continue
        previous_counts = previous_rules.get(rule, {})
        for key, count in candidate_counts.items():
            old_count = previous_counts.get(key, 0)
            semantic_limit = (
                migration_limits.get(rule, {}).get(key, 0)
                if dependency_migration and rule in migrated_edges else 0
            )
            allowed_count = semantic_limit if dependency_migration and rule in migrated_edges else old_count
            if count > allowed_count:
                failures.append(
                    f"baseline ratchet: {rule}: {key}: candidate allows {count}, "
                    f"target branch allows {old_count}"
                    + (f", three-way source limit is {semantic_limit}" if dependency_migration and rule in migrated_edges else "")
                )
    previous_cycles = previous.get("rust_feature_cycles", [])
    for cycle in candidate.get("rust_feature_cycles", []):
        if dependency_migration:
            if not cycle_allowed(cycle, migration_cycles):
                failures.append(
                    "baseline ratchet: cycle is absent or wider than three-way merged source: "
                    + " -> ".join(cycle)
                )
            continue
        if not cycle_allowed(cycle, previous_cycles):
            failures.append(
                "baseline ratchet: cycle is new or wider than target branch: "
                + " -> ".join(cycle)
            )
    return failures


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--root",
        type=Path,
        default=Path(__file__).resolve().parent.parent,
        help="repository root (defaults to the script's parent repository)",
    )
    parser.add_argument(
        "--baseline",
        type=Path,
        default=DEFAULT_BASELINE,
        help="baseline path, relative to repository root by default",
    )
    parser.add_argument(
        "--print-current",
        action="store_true",
        help="print the current findings as baseline JSON without checking",
    )
    parser.add_argument(
        "--base-ref",
        help="also require the checked-in baseline to only tighten relative to this Git ref",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    root = args.root.resolve()
    state = current_state(root)
    if args.print_current:
        print(json.dumps(state, indent=2, ensure_ascii=False))
        return 0

    baseline_path = args.baseline
    if not baseline_path.is_absolute():
        baseline_path = root / baseline_path
    if not baseline_path.exists():
        print(f"architecture guard: baseline not found: {baseline_path}", file=sys.stderr)
        return 2
    try:
        baseline = json.loads(read_text(baseline_path))
    except (OSError, json.JSONDecodeError) as error:
        print(f"architecture guard: invalid baseline: {error}", file=sys.stderr)
        return 2

    failures, progress = compare(state, baseline)
    if args.base_ref:
        try:
            relative_baseline = baseline_path.resolve().relative_to(root)
            previous_baseline = baseline_from_git(root, args.base_ref, relative_baseline)
        except (ValueError, OSError) as error:
            print(f"architecture guard: cannot compare target baseline: {error}", file=sys.stderr)
            return 2
        if previous_baseline is None:
            print(
                f"INFO: {args.base_ref} has no architecture baseline; accepting initial baseline"
            )
        else:
            try:
                anchor = baseline.get("migration_anchors", {}).get(RUST_DEPENDENCY_MIGRATION)
                if (
                    RUST_DEPENDENCY_MIGRATION in baseline.get("migrations", [])
                    and RUST_DEPENDENCY_MIGRATION
                    not in previous_baseline.get("migrations", [])
                ):
                    if anchor != RUST_DEPENDENCY_MIGRATION_ANCHOR:
                        raise ValueError("Rust dependency migration anchor is not approved")
                    merge_rust_state, anchor_rust_state, target_rust_state = migration_semantic_states_from_git(
                        root, anchor, args.base_ref
                    )
                else:
                    merge_rust_state = None
                    anchor_rust_state = None
                    target_rust_state = None
            except ValueError as error:
                print(f"architecture guard: {error}", file=sys.stderr)
                return 2
            failures.extend(compare_baseline_ratchet(
                baseline, previous_baseline, merge_rust_state, anchor_rust_state,
                target_rust_state
            ))
    if should_fail(failures, progress):
        print("architecture guard failed:", file=sys.stderr)
        if failures:
            print("new architecture debt is not allowed:", file=sys.stderr)
            for message in failures:
                print(f"  - {message}", file=sys.stderr)
        if progress:
            print(
                "architecture debt decreased; tighten the matching baseline in this change:",
                file=sys.stderr,
            )
            for message in progress:
                print(f"  - {message}", file=sys.stderr)
        print(
            "Fix new violations. Lower/remove stale baseline entries when debt decreases. "
            "Increase a baseline only in a dedicated, reviewed architecture decision.",
            file=sys.stderr,
        )
        return 1
    print("architecture guard passed: no architecture debt increased")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
