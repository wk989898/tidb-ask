---
name: read-local-doc
description: Read, search, and summarize documentation from the local ./doc directory (a git checkout of pingcap/docs). Use when answering TiDB/TiKV/PingCAP questions that must be grounded in the local docs, when the user asks to look up or quote files under ./doc, or when version-specific doc differences matter and you need to switch the doc repo to a requested release branch (for example release-7.5) before searching.
---

# Read Local Doc

## Overview

以本地 `./doc` 目录中的 Markdown 文档为准，通过“切换到目标版本 -> 搜索 -> 精读相关段落 -> 提炼回答”的方式，给出带出处（文件路径+行号/标题）的回答。

## Workflow

### 0) 确认文档目录存在且可读

- 默认把当前工作目录下的 `./doc` 当作文档根目录。
- 先确认它是一个 git 仓库：`git -C doc rev-parse --is-inside-work-tree`。
- 如果 `./doc` 不存在或不是目标文档仓库，先让用户切换到包含 `doc/` 的目录，或提供正确路径。

### 1) 选择要使用的文档版本（非常重要）

不同 TiDB 版本（尤其是不同 minor 版本）在参数、语义、默认值和限制上可能不同，必须先确定版本再查。

- 优先让用户明确版本：例如 `v8.3`、`8.3.0`、`release-8.3`、`master`/`latest`、`cloud`。
- 版本映射规则（建议默认使用这个）：
  - `vX.Y(.Z)` / `X.Y(.Z)` -> `release-X.Y`
  - `cloud` / `tidb-cloud` -> `release-cloud`
  - `master` / `latest` -> `master`
- 如果用户完全不关心版本：使用 `./doc` 当前检出的分支即可；回答里要说明你用了哪个分支/commit。
- 如果用户问“不同版本有什么差异”：至少对 2 个版本分别查证，明确指出差异来自哪里。

### 2) 切换 `./doc` 到目标版本（git）

先确保工作区干净，避免切分支失败或产生脏状态：

- `git -C doc status --porcelain` 必须为空；否则先 stash/丢弃更改。

推荐使用脚本（更不容易踩 shallow clone/远端分支的坑）：

- `python3 ~/.codex/skills/read-local-doc/scripts/switch_doc_version.py <version> --doc-dir doc`
  - 对 `latest/master/main` 会默认拉取远端最新提交（避免浅克隆悄悄落后）；其他版本在分支本地不存在时也会按需 fetch 目标分支。

（如果 `$CODEX_HOME` 不同或 skills 目录不在 `~/.codex/skills`，按实际安装路径调整。）

手工方式（适合临时操作、也适合 debug）：

- 获取目标版本分支（shallow）：`GIT_ASKPASS= GIT_TERMINAL_PROMPT=0 git -C doc fetch --depth 1 origin refs/heads/release-7.5`
- 切换/创建本地分支：`git -C doc switch -C release-7.5 FETCH_HEAD`

记录“本次回答依据哪个版本文档”：

- `git -C doc rev-parse --abbrev-ref HEAD`
- `git -C doc rev-parse --short HEAD`

### 3) 搜索文档（先广后窄）

目标是：用尽量少的文件/段落读到“能直接回答用户问题”的权威描述。

#### 3.1 生成搜索关键词

- 从用户问题里提取 3–8 个关键 token：参数名（如 `tidb_slow_log_rules`）、SQL 语法、错误信息片段、功能名（如 `TiFlash`）、组件名（如 `PD`/`TiKV`）。
- 适当扩展同义词/别名：如 “GC” 也可能写成 “garbage collection”；“placement rule” 也可能写成 “placement rules”。

#### 3.2 用 `rg` 搜索（推荐）

- 单关键词：`rg -n --smart-case --glob '*.md' 'keyword' doc`
- 多关键词（或）：`rg -n --smart-case --glob '*.md' '(foo|bar|baz)' doc`
- 找标题：`rg -n '^#{1,6} ' doc/path/file.md`
- 更少噪声：必要时加 `--glob '!.git/**'`（一般不需要）

也可用脚本快速按“文件聚合+排序”查看候选文档（适合先扫一眼再精读）：

- `python3 ~/.codex/skills/read-local-doc/scripts/doc_search.py 'keyword' --doc-dir doc --max-files 8 --max-matches 3`

### 4) 精读相关段落（不要整篇搬运）

- 对命中较多的 3–5 个候选文件：优先找最接近问题的标题/小节，再读取该小节附近内容。
- 常用方式：
  - 先用 `rg -n '^#{1,6} ' file.md` 看目录结构
  - 再用 `sed -n '<start>,<end>p' file.md` 精读某一段

### 5) 基于文档输出回答（带出处）

- 回答开头标注“本次依据的文档版本”：`branch + short commit`。
- 只摘取能支撑结论的关键句/关键表格项；避免整页复制导致噪声。
- 用“文件路径 + 行号/标题”做出处，方便用户复核与跳转；对用户输出时避免暴露本地目录名/工具名（路径尽量使用文档仓库内相对路径）。
- 如果文档没有覆盖用户想问的特定细节：明确你查过哪些文件/关键词，并提出 1–2 个最小化的追问。

## Troubleshooting

### Git 卡在认证/Askpass 报错

某些环境里 `GIT_ASKPASS` 可能指向不存在的脚本导致 git 失败。运行 git 命令时加：

- `GIT_ASKPASS= GIT_TERMINAL_PROMPT=0 <git command>`

### Shallow clone 看不到远端 release 分支

这是正常的（很多浅克隆只抓了 `master`）。按需拉取目标分支即可：

- `git -C doc fetch --depth 1 origin refs/heads/release-8.3`
- `git -C doc switch -C release-8.3 FETCH_HEAD`
