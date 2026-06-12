#!/usr/bin/env python3
"""
Switch one or more local repos under ./code to a requested version/ref.

This is designed for shallow clones created with `git clone --depth 1 --single-branch`,
where only the default branch exists locally and release branches/tags must be fetched
explicitly.

Examples:
  switch_code_version.py 7.5 --code-dir code --repos pingcap/tidb pingcap/pd tikv/tikv
  switch_code_version.py v8.1.2 --code-dir code --repos pingcap/tidb
  switch_code_version.py latest --code-dir code --repos pingcap/tidb
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from dataclasses import dataclass
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


def resolve_repo_specs(code_dir: Path, specs: list[str], *, all_repos: bool) -> dict[str, Path]:
    repos = discover_repos(code_dir)
    if all_repos:
        return repos
    if not specs:
        raise ValueError("no repos specified (use --repos ... or --all)")

    resolved: dict[str, Path] = {}
    for raw in specs:
        spec = raw.strip()
        if not spec:
            continue

        # Path spec.
        p = Path(spec)
        if p.is_dir() and (p / ".git").is_dir():
            repo_id = p.name
            try:
                repo_id = str(p.relative_to(code_dir))
            except ValueError:
                pass
            resolved[repo_id] = p
            continue

        # org/repo
        if "/" in spec:
            if spec in repos:
                resolved[spec] = repos[spec]
                continue
            p = code_dir / spec
            if p.is_dir() and (p / ".git").is_dir():
                resolved[spec] = p
                continue
            raise ValueError(f"repo not found: {spec}")

        # repo name only
        matches = [(rid, path) for rid, path in repos.items() if rid.split("/", 1)[1] == spec]
        if not matches:
            raise ValueError(f"repo not found: {spec}")
        if len(matches) > 1:
            cands = ", ".join(rid for rid, _ in matches[:10])
            raise ValueError(f"ambiguous repo name '{spec}'. Use org/repo. Candidates: {cands}")
        rid, path = matches[0]
        resolved[rid] = path

    return resolved


def normalize_version(version: str) -> tuple[list[str], list[str]]:
    """
    Return (branch_candidates, tag_candidates).
    """
    v = version.strip()
    if not v:
        raise ValueError("empty version")

    lower = v.lower()
    if lower in {"latest", "master", "main"}:
        # Many repos have either master or main as default.
        return (["master", "main"], [])

    if lower.startswith("release-"):
        return ([lower], [])

    if lower.startswith("v"):
        lower_no_v = lower[1:]
    else:
        lower_no_v = lower

    # X.Y or X.Y.Z
    m = re.match(r"^(?P<major>\d+)\.(?P<minor>\d+)(?:\.(?P<patch>\d+))?$", lower_no_v)
    if not m:
        # Treat as a raw ref name. Try both as a branch and as a tag so that
        # values like "v8.0.0-rc.1" work without needing extra flags.
        return ([v], [v])

    major = m.group("major")
    minor = m.group("minor")
    patch = m.group("patch")

    branches = [f"release-{major}.{minor}"]
    tags: list[str] = []
    if patch is not None:
        tags.append(f"v{major}.{minor}.{patch}")
    # Some repos tag at major.minor level (rare), but it's cheap to try.
    tags.append(f"v{major}.{minor}")

    return (branches, tags)


def ensure_clean(repo_dir: Path) -> None:
    dirty = run(["git", "-C", str(repo_dir), "status", "--porcelain"])
    if dirty:
        raise CommandError("working tree not clean")


def branch_exists_local(repo_dir: Path, branch: str) -> bool:
    try:
        run(["git", "-C", str(repo_dir), "show-ref", "--verify", "--quiet", f"refs/heads/{branch}"])
        return True
    except CommandError:
        return False


def fetch_branch(repo_dir: Path, remote: str, branch: str) -> None:
    run(["git", "-C", str(repo_dir), "fetch", "--depth", "1", remote, f"refs/heads/{branch}"])


def fetch_tag(repo_dir: Path, remote: str, tag: str) -> None:
    run(["git", "-C", str(repo_dir), "fetch", "--depth", "1", remote, f"refs/tags/{tag}"])


def switch_to_fetch_head(repo_dir: Path, local_branch: str) -> None:
    run(["git", "-C", str(repo_dir), "switch", "-C", local_branch, "FETCH_HEAD"])


def head_short(repo_dir: Path) -> str:
    return run(["git", "-C", str(repo_dir), "rev-parse", "--short", "HEAD"])


def current_branch(repo_dir: Path) -> str:
    return run(["git", "-C", str(repo_dir), "rev-parse", "--abbrev-ref", "HEAD"])


def sanitize_branch(name: str) -> str:
    # Avoid weird characters; keep it a simple branch name.
    name = name.strip()
    name = re.sub(r"[^A-Za-z0-9._/-]+", "-", name)
    name = name.strip("-")
    name = name.replace("/", "-")
    return name or "ref"


@dataclass(frozen=True)
class SwitchResult:
    repo_id: str
    repo_dir: Path
    ok: bool
    ref: str | None
    head: str | None
    message: str


def try_switch_repo(
    repo_id: str,
    repo_dir: Path,
    *,
    remote: str,
    branches: list[str],
    tags: list[str],
    update: bool,
) -> SwitchResult:
    try:
        run(["git", "-C", str(repo_dir), "rev-parse", "--is-inside-work-tree"])
        ensure_clean(repo_dir)
    except CommandError as e:
        return SwitchResult(repo_id, repo_dir, False, None, None, f"not ready: {e}")

    # First try branches.
    last_error: str | None = None
    for br in branches:
        try:
            if branch_exists_local(repo_dir, br) and not update:
                run(["git", "-C", str(repo_dir), "switch", br])
                return SwitchResult(repo_id, repo_dir, True, br, head_short(repo_dir), "switched (local)")

            fetch_branch(repo_dir, remote, br)
            switch_to_fetch_head(repo_dir, br)
            return SwitchResult(repo_id, repo_dir, True, br, head_short(repo_dir), "switched (fetched)")
        except CommandError as e:
            last_error = str(e).splitlines()[-1] if str(e) else "failed"
            continue

    # Then try tags.
    for tag in tags:
        try:
            fetch_tag(repo_dir, remote, tag)
            local = f"tag-{sanitize_branch(tag)}"
            switch_to_fetch_head(repo_dir, local)
            return SwitchResult(repo_id, repo_dir, True, tag, head_short(repo_dir), f"switched (tag -> {local})")
        except CommandError as e:
            last_error = str(e).splitlines()[-1] if str(e) else "failed"
            continue

    # As a last resort, try 'master'/'main' if user asked for latest but branch list didn't include it.
    if branches == ["master", "main"]:
        try:
            cur = current_branch(repo_dir)
            return SwitchResult(repo_id, repo_dir, True, cur, head_short(repo_dir), "kept current default branch")
        except CommandError:
            pass

    return SwitchResult(repo_id, repo_dir, False, None, None, last_error or "ref not found")


def main() -> int:
    parser = argparse.ArgumentParser(description="Switch local repos under ./code to a requested version.")
    parser.add_argument("version", help="Version like 7.5, v8.1.2, release-7.5, master/main/latest")
    parser.add_argument("--code-dir", default="code", help="Code root directory (default: ./code)")
    parser.add_argument(
        "--repos",
        nargs="*",
        default=[],
        help="Repo specs (org/repo, repo name, or path). Required unless --all is set.",
    )
    parser.add_argument("--all", action="store_true", help="Apply to all repos under code-dir (may be slow).")
    parser.add_argument("--remote", default="origin", help="Git remote name (default: origin)")
    parser.add_argument("--update", action="store_true", help="Always fetch and reset (shallow) even if local branch exists.")
    args = parser.parse_args()

    # For "latest"/"master"/"main", default to updating to the newest remote tip
    # so local shallow clones don't silently drift behind.
    update = bool(args.update)
    if not update:
        v = args.version.strip().lower()
        if v in {"latest", "master", "main"}:
            update = True

    code_dir = Path(args.code_dir)
    try:
        repo_map = resolve_repo_specs(code_dir, args.repos, all_repos=args.all)
    except ValueError as e:
        print(f"[ERROR] {e}", file=sys.stderr)
        return 2

    if not repo_map:
        print(f"[ERROR] no repos found under: {code_dir}", file=sys.stderr)
        return 2

    branches, tags = normalize_version(args.version)

    print(f"Target version: {args.version}")
    print(f"Branch candidates: {', '.join(branches) if branches else '(none)'}")
    if tags:
        print(f"Tag candidates: {', '.join(tags)}")
    print(f"Repos: {len(repo_map)}")
    print()

    results: list[SwitchResult] = []
    for repo_id, repo_dir in sorted(repo_map.items()):
        r = try_switch_repo(
            repo_id,
            repo_dir,
            remote=args.remote,
            branches=branches,
            tags=tags,
            update=update,
        )
        results.append(r)
        if r.ok:
            print(f"[OK] {repo_id} -> {r.ref} ({r.head})  {r.message}")
        else:
            print(f"[FAIL] {repo_id}  {r.message}")

    failed = [r for r in results if not r.ok]
    if failed:
        print()
        print(f"Done with failures: {len(failed)}/{len(results)}")
        return 1

    print()
    print("Done: all selected repos switched successfully.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
