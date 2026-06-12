#!/usr/bin/env python3
"""
Search markdown docs under ./doc using ripgrep and print a compact, ranked summary.

This is intended to be used by the `read-local-doc` skill as a quick way to
identify which files/sections to read next (instead of dumping raw rg output).

Examples:
  doc_search.py 'tidb_slow_log_rules' --doc-dir doc
  doc_search.py '(tiflash|tiflash learner)' --doc-dir doc --max-files 12
"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class Match:
    line_no: int
    line_text: str
    heading: str | None


def run_rg(query: str, doc_dir: Path, glob: str, max_count_per_file: int) -> str:
    rg = shutil.which("rg")
    if not rg:
        raise RuntimeError("ripgrep (rg) not found in PATH")

    cmd = [
        rg,
        "--line-number",
        "--no-heading",
        "--smart-case",
        "--max-count",
        str(max_count_per_file),
        "--glob",
        glob,
        query,
        str(doc_dir),
    ]
    p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    # rg returns 1 when there are no matches; treat that as non-fatal.
    if p.returncode not in (0, 1):
        raise RuntimeError(f"rg failed ({p.returncode}): {' '.join(cmd)}\n{p.stdout}".rstrip())
    return p.stdout or ""


def nearest_heading(lines: list[str], idx: int) -> str | None:
    # idx is 0-based line index in `lines`.
    for j in range(idx, -1, -1):
        s = lines[j].strip()
        if s.startswith("#"):
            return s
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description="Search docs under ./doc and show ranked matches.")
    parser.add_argument("query", help="Ripgrep regex/pattern")
    parser.add_argument("--doc-dir", default="doc", help="Doc root (default: ./doc)")
    parser.add_argument("--glob", default="*.md", help="Glob filter for rg (default: *.md)")
    parser.add_argument("--max-files", type=int, default=8, help="Show top N files (default: 8)")
    parser.add_argument(
        "--max-matches",
        type=int,
        default=3,
        help="Show up to N matching lines per file (default: 3)",
    )
    args = parser.parse_args()

    doc_dir = Path(args.doc_dir)
    if not doc_dir.exists():
        print(f"[ERROR] doc dir not found: {doc_dir}", file=sys.stderr)
        return 2

    try:
        raw = run_rg(args.query, doc_dir, args.glob, args.max_matches)
    except Exception as e:
        print(f"[ERROR] {e}", file=sys.stderr)
        return 2

    hits: dict[Path, list[tuple[int, str]]] = defaultdict(list)
    # Output format: /path/to/file.md:123:the line...
    for line in raw.splitlines():
        # rg may print warnings like "Binary file matches"; skip those.
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
        path = Path(path_str)
        hits[path].append((line_no, text.rstrip()))

    if not hits:
        print("[OK] No matches")
        return 0

    ranked = sorted(hits.items(), key=lambda kv: (-len(kv[1]), str(kv[0])))
    ranked = ranked[: max(1, args.max_files)]

    print(f"Query: {args.query}")
    print(f"Doc dir: {doc_dir}")
    print()

    for path, occurrences in ranked:
        rel = path
        try:
            rel = path.relative_to(doc_dir)
        except ValueError:
            pass

        # Read file once to annotate headings.
        try:
            lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
        except OSError:
            lines = []

        matches: list[Match] = []
        for line_no, text in occurrences[: args.max_matches]:
            heading = None
            if lines and 1 <= line_no <= len(lines):
                heading = nearest_heading(lines, line_no - 1)
            matches.append(Match(line_no=line_no, line_text=text, heading=heading))

        print(f"- {rel} ({len(occurrences)} match(es))")
        for m in matches:
            if m.heading:
                print(f"  - {m.heading}")
            print(f"  - L{m.line_no}: {m.line_text.strip()}")
        print()

    return 0


if __name__ == "__main__":
    raise SystemExit(main())

