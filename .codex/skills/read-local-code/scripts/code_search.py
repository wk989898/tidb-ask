#!/usr/bin/env python3
"""
Search code in one or more local repos under ./code using ripgrep (rg) and print
an aggregated, ranked summary.

This script avoids flooding the console with raw `rg` output by grouping matches
by repo and file and limiting how many lines are printed per file.

Examples:
  code_search.py 'GetTiFlashReplica' --code-dir code --repos pingcap/tidb pingcap/pd
  code_search.py '(raftstore|apply_router)' --code-dir code --repos tikv/tikv
"""

from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path


DEFAULT_INCLUDE_GLOBS = [
    "*.go",
    "*.rs",
    "*.proto",
    "*.toml",
    "*.yaml",
    "*.yml",
    "*.sql",
    "*.py",
    "*.java",
    "*.kt",
    "*.ts",
    "*.tsx",
    "*.js",
    "*.jsx",
    "*.sh",
]

DEFAULT_EXCLUDE_GLOBS = [
    "!**/.git/**",
    "!**/vendor/**",
    "!**/target/**",
    "!**/node_modules/**",
    "!**/dist/**",
    "!**/build/**",
    "!**/bin/**",
    "!**/bazel-*/**",
]


@dataclass(frozen=True)
class Hit:
    path: str
    line_no: int
    line_text: str


def discover_repos(code_dir: Path) -> dict[str, Path]:
    repos: dict[str, Path] = {}
    if not code_dir.exists():
        return repos
    for org_dir in sorted(code_dir.iterdir()):
        if not org_dir.is_dir():
            continue
        for repo_dir in sorted(org_dir.iterdir()):
            if not repo_dir.is_dir():
                continue
            if not (repo_dir / ".git").is_dir():
                continue
            repo_id = f"{org_dir.name}/{repo_dir.name}"
            repos[repo_id] = repo_dir
    return repos


def resolve_repo_specs(code_dir: Path, specs: list[str]) -> dict[str, Path]:
    repos = discover_repos(code_dir)
    if not specs:
        return repos

    resolved: dict[str, Path] = {}
    for raw in specs:
        spec = raw.strip()
        if not spec:
            continue

        # Path to a repo directory.
        p = Path(spec)
        if p.is_dir() and (p / ".git").is_dir():
            repo_id = p.name
            try:
                repo_id = str(p.relative_to(code_dir))
            except ValueError:
                pass
            resolved[repo_id] = p
            continue

        # org/repo identifier
        if "/" in spec:
            if spec in repos:
                resolved[spec] = repos[spec]
                continue
            p = code_dir / spec
            if p.is_dir() and (p / ".git").is_dir():
                resolved[spec] = p
                continue
            raise SystemExit(f"[ERROR] repo not found: {spec}")

        # repo name only, may be ambiguous across orgs.
        matches = [(rid, path) for rid, path in repos.items() if rid.split("/", 1)[1] == spec]
        if not matches:
            raise SystemExit(f"[ERROR] repo not found: {spec}")
        if len(matches) > 1:
            cands = ", ".join(rid for rid, _ in matches[:10])
            raise SystemExit(f"[ERROR] ambiguous repo name '{spec}'. Use org/repo. Candidates: {cands}")
        rid, path = matches[0]
        resolved[rid] = path

    return resolved


def run_rg(
    query: str,
    paths: list[Path],
    include_globs: list[str],
    exclude_globs: list[str],
) -> str:
    rg = shutil.which("rg")
    if not rg:
        raise RuntimeError("ripgrep (rg) not found in PATH")

    cmd = [
        rg,
        "--line-number",
        "--no-heading",
        "--smart-case",
        "--color",
        "never",
    ]

    for g in exclude_globs:
        cmd.extend(["--glob", g])
    for g in include_globs:
        cmd.extend(["--glob", g])

    cmd.append(query)
    cmd.extend([str(p) for p in paths])

    env = os.environ.copy()
    # Ensure stable output.
    env.setdefault("LC_ALL", "C")

    p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=env)
    # rg returns 1 when no matches.
    if p.returncode not in (0, 1):
        raise RuntimeError(f"rg failed ({p.returncode}): {' '.join(cmd)}\n{p.stdout}".rstrip())
    return p.stdout or ""


def classify_repo(match_path: str, repo_roots: dict[str, str]) -> str | None:
    # repo_roots: repo_id -> root prefix string (with trailing slash).
    for repo_id, root in repo_roots.items():
        if match_path.startswith(root):
            return repo_id
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description="Search code under ./code with aggregated output.")
    parser.add_argument("query", help="Ripgrep pattern/regex")
    parser.add_argument("--code-dir", default="code", help="Code root (default: ./code)")
    parser.add_argument(
        "--repos",
        nargs="*",
        default=[],
        help="Repo specs (org/repo, repo name, or path). If omitted, searches all repos under code-dir.",
    )
    parser.add_argument("--max-repos", type=int, default=5, help="Show top N repos (default: 5)")
    parser.add_argument("--max-files", type=int, default=8, help="Show top N files per repo (default: 8)")
    parser.add_argument("--max-matches", type=int, default=3, help="Show up to N lines per file (default: 3)")
    parser.add_argument(
        "--all-files",
        action="store_true",
        help="Search all file types (still excluding common build/vendor dirs).",
    )
    parser.add_argument(
        "--glob",
        action="append",
        default=[],
        help=(
            "Include glob filter (repeatable). "
            "If any are provided, they replace the default include globs (unless --all-files). "
            "Example: --glob '*.rs' --glob '*.proto'"
        ),
    )
    parser.add_argument(
        "--exclude-glob",
        action="append",
        default=[],
        help="Additional exclude glob (repeatable). Example: --exclude-glob '!**/metrics/**'",
    )
    args = parser.parse_args()

    code_dir = Path(args.code_dir)
    repo_map = resolve_repo_specs(code_dir, args.repos)
    if not repo_map:
        print(f"[ERROR] no repos found under: {code_dir}", file=sys.stderr)
        return 2

    if args.all_files:
        include_globs = []
    else:
        include_globs = args.glob if args.glob else list(DEFAULT_INCLUDE_GLOBS)

    exclude_globs = list(DEFAULT_EXCLUDE_GLOBS) + list(args.exclude_glob)

    repo_roots: dict[str, str] = {}
    search_paths: list[Path] = []
    for repo_id, repo_dir in repo_map.items():
        root = repo_dir.as_posix().rstrip("/") + "/"
        repo_roots[repo_id] = root
        search_paths.append(repo_dir)

    try:
        raw = run_rg(args.query, search_paths, include_globs, exclude_globs)
    except Exception as e:
        print(f"[ERROR] {e}", file=sys.stderr)
        return 2

    # repo_id -> file_rel -> list[Hit]
    hits: dict[str, dict[str, list[Hit]]] = defaultdict(lambda: defaultdict(list))
    counts: dict[str, dict[str, int]] = defaultdict(lambda: defaultdict(int))

    for line in raw.splitlines():
        if ":" not in line:
            continue
        parts = line.split(":", 2)
        if len(parts) != 3:
            continue
        path_str, line_no_str, text = parts
        try:
            line_no = int(line_no_str)
        except ValueError:
            continue

        repo_id = classify_repo(path_str, repo_roots)
        if not repo_id:
            # Fallback: if rg prints relative paths, try with cwd + repo roots later.
            # For now, keep it grouped under an "unknown" bucket.
            repo_id = "unknown"

        # Make file path relative to the repo root (for readability).
        file_rel = path_str
        root = repo_roots.get(repo_id)
        if root and file_rel.startswith(root):
            file_rel = file_rel[len(root) :]

        counts[repo_id][file_rel] += 1
        if len(hits[repo_id][file_rel]) < args.max_matches:
            hits[repo_id][file_rel].append(Hit(path=path_str, line_no=line_no, line_text=text.rstrip()))

    if not counts:
        print("[OK] No matches")
        return 0

    repo_totals = [(rid, sum(file_counts.values())) for rid, file_counts in counts.items()]
    repo_totals.sort(key=lambda x: (-x[1], x[0]))
    repo_totals = repo_totals[: max(1, args.max_repos)]

    print(f"Query: {args.query}")
    print(f"Code dir: {code_dir}")
    if args.repos:
        print(f"Repos: {', '.join(args.repos)}")
    else:
        print("Repos: (all under code-dir)")
    print()

    for repo_id, total in repo_totals:
        print(f"- {repo_id} ({total} match(es))")
        file_counts = list(counts[repo_id].items())
        file_counts.sort(key=lambda x: (-x[1], x[0]))
        file_counts = file_counts[: max(1, args.max_files)]
        for file_rel, cnt in file_counts:
            print(f"  - {file_rel} ({cnt})")
            for h in hits[repo_id][file_rel]:
                print(f"    - L{h.line_no}: {h.line_text.strip()}")
        print()

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
