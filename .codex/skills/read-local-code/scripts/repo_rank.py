#!/usr/bin/env python3
"""
Rank local repos under ./code by relevance to a query.

The goal is to quickly narrow down which repo(s) to inspect before doing deeper
code searches.

Examples:
  repo_rank.py 'pd scheduling region merge' --code-dir code --top 15
  repo_rank.py 'TiKV raftstore apply' --code-dir code --top 20
"""

from __future__ import annotations

import argparse
import re
import sys
from dataclasses import dataclass
from pathlib import Path


SCAN_FILES = [
    "README.md",
    "README.rst",
    "README.adoc",
    "go.mod",
    "Cargo.toml",
    "package.json",
    "pyproject.toml",
    "pom.xml",
    "build.gradle",
    "Makefile",
]

STOPWORDS = {
    "a",
    "an",
    "and",
    "are",
    "as",
    "at",
    "be",
    "by",
    "for",
    "from",
    "how",
    "i",
    "in",
    "is",
    "it",
    "of",
    "on",
    "or",
    "that",
    "the",
    "this",
    "to",
    "what",
    "when",
    "where",
    "why",
    "with",
}


@dataclass
class RepoScore:
    repo_id: str
    path: Path
    score: int
    reasons: list[str]


def discover_repos(code_dir: Path) -> dict[str, Path]:
    repos: dict[str, Path] = {}
    if not code_dir.exists():
        return repos
    for org_dir in sorted(code_dir.iterdir()):
        if not org_dir.is_dir():
            continue
        # Expect code/<org>/<repo>
        for repo_dir in sorted(org_dir.iterdir()):
            if not repo_dir.is_dir():
                continue
            if not (repo_dir / ".git").is_dir():
                continue
            repo_id = f"{org_dir.name}/{repo_dir.name}"
            repos[repo_id] = repo_dir
    return repos


def extract_keywords(query: str) -> list[str]:
    # Extract mostly-ascii tokens. This intentionally doesn't try to segment Chinese.
    tokens = re.findall(r"[A-Za-z0-9_./:-]+", query)
    out: list[str] = []
    seen: set[str] = set()
    for t in tokens:
        t = t.strip().strip("`'\"").lower()
        if not t:
            continue
        if t in STOPWORDS:
            continue
        # Keep short but meaningful tokens like "pd", "gc".
        if len(t) == 1:
            continue
        if t not in seen:
            seen.add(t)
            out.append(t)
    return out


def read_text_truncated(path: Path, limit_bytes: int = 200_000) -> str:
    try:
        data = path.read_bytes()
    except OSError:
        return ""
    if len(data) > limit_bytes:
        data = data[:limit_bytes]
    return data.decode("utf-8", errors="replace")


def score_repo(repo_id: str, repo_dir: Path, keywords: list[str]) -> RepoScore:
    repo_lower = repo_id.lower()
    repo_name_lower = repo_dir.name.lower()

    score = 0
    reasons: list[str] = []

    def add(reason: str, points: int) -> None:
        nonlocal score
        score += points
        reasons.append(f"{reason}+{points}")

    # Name-based scoring (very strong signal).
    for kw in keywords:
        if kw in repo_name_lower:
            add(f"name:{kw}", 30)
        elif kw in repo_lower:
            add(f"id:{kw}", 25)

    # File-based scoring (weaker, but helps when name isn't obvious).
    file_hits: dict[str, set[str]] = {}
    for rel in SCAN_FILES:
        p = repo_dir / rel
        if not p.is_file():
            continue
        content = read_text_truncated(p).lower()
        if not content:
            continue
        matched = {kw for kw in keywords if kw in content}
        if matched:
            file_hits[rel] = matched

    for rel, matched in sorted(file_hits.items()):
        if rel in {"go.mod", "Cargo.toml"}:
            per_kw = 18
        elif rel.startswith("README"):
            per_kw = 10
        else:
            per_kw = 6
        add(f"{rel}:{','.join(sorted(matched))}", per_kw * len(matched))

    # Small boost for common TiDB/TiKV ecosystem repos if keywords mention them.
    if any(kw in {"tidb", "tikv", "pd", "tiflash", "br", "dumpling", "lightning"} for kw in keywords):
        if repo_dir.name.lower() in {"tidb", "tikv", "pd", "tiflash", "br", "kvproto"}:
            add("core-repo", 8)

    return RepoScore(repo_id=repo_id, path=repo_dir, score=score, reasons=reasons)


def main() -> int:
    parser = argparse.ArgumentParser(description="Rank repos under ./code for a query.")
    parser.add_argument("query", help="Question or keyword string")
    parser.add_argument("--code-dir", default="code", help="Code root directory (default: ./code)")
    parser.add_argument("--top", type=int, default=15, help="Show top N repos (default: 15)")
    parser.add_argument(
        "--min-score",
        type=int,
        default=1,
        help="Only show repos with score >= N (default: 1)",
    )
    args = parser.parse_args()

    code_dir = Path(args.code_dir)
    repos = discover_repos(code_dir)
    if not repos:
        print(f"[ERROR] no git repos found under: {code_dir}", file=sys.stderr)
        return 2

    keywords = extract_keywords(args.query)
    if not keywords:
        print("[ERROR] no searchable keywords detected. Provide a query containing identifiers (repo names, symbols, errors).", file=sys.stderr)
        return 2

    scored = [score_repo(repo_id, repo_dir, keywords) for repo_id, repo_dir in repos.items()]
    scored = [s for s in scored if s.score >= args.min_score]
    scored.sort(key=lambda s: (-s.score, s.repo_id))

    if not scored:
        print("[OK] No repos matched.")
        return 0

    top = max(1, args.top)
    scored = scored[:top]

    print(f"Query: {args.query}")
    print(f"Keywords: {', '.join(keywords)}")
    print(f"Code dir: {code_dir}")
    print()

    for s in scored:
        rel = s.path
        try:
            rel = s.path.relative_to(code_dir)
        except ValueError:
            pass
        reasons = ", ".join(s.reasons[:6])
        if len(s.reasons) > 6:
            reasons += ", ..."
        print(f"- [{s.score}] {s.repo_id} ({rel})")
        if reasons:
            print(f"  - reasons: {reasons}")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())

