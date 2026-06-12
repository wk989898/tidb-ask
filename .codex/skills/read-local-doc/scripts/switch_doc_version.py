#!/usr/bin/env python3
"""
Switch the local ./doc checkout to a requested docs version branch.

This script is designed for shallow clones of pingcap/docs where only `master`
is fetched by default (remote.origin.fetch is narrow). It fetches the requested
branch into FETCH_HEAD and creates/resets a local branch from it.

Examples:
  switch_doc_version.py 8.3 --doc-dir doc
  switch_doc_version.py v7.5.2 --doc-dir /path/to/doc
  switch_doc_version.py release-7.5 --doc-dir doc
  switch_doc_version.py master --doc-dir doc --update
  switch_doc_version.py cloud --doc-dir doc
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path


class CommandError(RuntimeError):
    pass


def run(cmd: list[str], *, cwd: Path | None = None) -> str:
    env = os.environ.copy()
    # Avoid broken VSCode askpass integration and disable interactive prompts.
    env["GIT_ASKPASS"] = ""
    env["SSH_ASKPASS"] = ""
    env["GIT_TERMINAL_PROMPT"] = "0"

    p = subprocess.run(
        cmd,
        cwd=str(cwd) if cwd else None,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    if p.returncode != 0:
        raise CommandError(f"Command failed ({p.returncode}): {' '.join(cmd)}\n{p.stdout}".rstrip())
    return (p.stdout or "").strip()


def normalize_version_to_branch(version: str) -> str:
    v = version.strip()
    if not v:
        raise ValueError("empty version")

    lower = v.lower()
    if lower in {"master", "main", "latest"}:
        return "master"
    if lower in {"cloud", "tidb-cloud", "tidbcloud"}:
        return "release-cloud"
    if lower.startswith("release-"):
        return lower

    if lower.startswith("v"):
        lower = lower[1:]

    # Match 8.3 or 8.3.0 etc.
    m = re.match(r"^(?P<major>\d+)\.(?P<minor>\d+)(?:\.\d+)?$", lower)
    if not m:
        raise ValueError(
            "unsupported version format. Expected one of: master|latest, cloud|tidb-cloud, "
            "release-X.Y, X.Y, vX.Y, X.Y.Z, vX.Y.Z"
        )

    major = m.group("major")
    minor = m.group("minor")
    return f"release-{major}.{minor}"


def main() -> int:
    parser = argparse.ArgumentParser(description="Switch ./doc to a requested pingcap/docs branch.")
    parser.add_argument("version", help="Version like 8.3, v7.5.2, release-7.5, master, cloud")
    parser.add_argument("--doc-dir", default="doc", help="Path to the local docs checkout (default: ./doc)")
    parser.add_argument("--remote", default="origin", help="Git remote name (default: origin)")
    parser.add_argument(
        "--update",
        action="store_true",
        help="Fetch and reset the local branch to the latest remote tip (shallow)",
    )
    args = parser.parse_args()

    doc_dir = Path(args.doc_dir).resolve()
    if not doc_dir.exists():
        print(f"[ERROR] doc dir not found: {doc_dir}", file=sys.stderr)
        return 2

    try:
        run(["git", "-C", str(doc_dir), "rev-parse", "--is-inside-work-tree"])
    except CommandError as e:
        print(f"[ERROR] not a git repo: {doc_dir}\n{e}", file=sys.stderr)
        return 2

    # Refuse to switch if there are local changes.
    dirty = run(["git", "-C", str(doc_dir), "status", "--porcelain"])
    if dirty:
        print("[ERROR] doc repo has local changes; refuse to switch versions.", file=sys.stderr)
        print("Run `git -C doc status` and stash/commit/discard changes first.", file=sys.stderr)
        return 3

    try:
        target_branch = normalize_version_to_branch(args.version)
    except ValueError as e:
        print(f"[ERROR] {e}", file=sys.stderr)
        return 2

    # For "latest"/"master"/"main", default to updating to the newest remote tip
    # so local shallow clones don't silently drift behind.
    update = bool(args.update)
    if not update:
        v = args.version.strip().lower()
        if v in {"latest", "master", "main"}:
            update = True

    current_branch = run(["git", "-C", str(doc_dir), "rev-parse", "--abbrev-ref", "HEAD"])

    # Fast path: already on the branch and no update requested.
    if current_branch == target_branch and not update:
        head = run(["git", "-C", str(doc_dir), "rev-parse", "--short", "HEAD"])
        print(f"[OK] Already on {target_branch} ({head})")
        return 0

    # If we are switching to an existing local branch and update is not needed, just switch.
    if not update:
        try:
            run(["git", "-C", str(doc_dir), "show-ref", "--verify", "--quiet", f"refs/heads/{target_branch}"])
            run(["git", "-C", str(doc_dir), "switch", target_branch])
            head = run(["git", "-C", str(doc_dir), "rev-parse", "--short", "HEAD"])
            print(f"[OK] Switched to {target_branch} ({head})")
            return 0
        except CommandError:
            # Branch does not exist locally; fall through to fetch+create.
            pass

    # Fetch target branch tip into FETCH_HEAD (shallow) and create/reset local branch.
    try:
        run(
            [
                "git",
                "-C",
                str(doc_dir),
                "fetch",
                "--depth",
                "1",
                args.remote,
                f"refs/heads/{target_branch}",
            ]
        )
    except CommandError as e:
        print(f"[ERROR] failed to fetch {target_branch} from remote '{args.remote}'.", file=sys.stderr)
        print(str(e), file=sys.stderr)
        print(
            "Tip: list available release branches with:\n"
            "  git -C doc ls-remote --heads origin 'refs/heads/release-*' | head",
            file=sys.stderr,
        )
        return 4

    if current_branch == target_branch:
        # Updating the currently checked-out branch: reset to FETCH_HEAD.
        run(["git", "-C", str(doc_dir), "reset", "--hard", "FETCH_HEAD"])
    else:
        # Create or reset the local branch and check it out.
        run(["git", "-C", str(doc_dir), "switch", "-C", target_branch, "FETCH_HEAD"])

    head = run(["git", "-C", str(doc_dir), "rev-parse", "--short", "HEAD"])
    print(f"[OK] Now on {target_branch} ({head})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
