---
name: read-local-code
description: Read, search, and analyze source code from the local ./code directory (a set of git checkouts under code/{org}/{repo}, such as pingcap/* and tikv/*). Use when questions must be answered by inspecting local code (implementations, call flows, config defaults, RPC/proto boundaries, error causes). Supports selecting relevant repos for a question, searching within one or more repos, combining evidence across repos, and switching repos to a requested version/ref (for example release-7.5 or v8.1.2) before searching.
---

# Read Local Code

## Overview

以本地 `./code` 目录下的源码仓库为准，按“选版本 -> 选 repo -> 搜索 -> 精读 -> 多 repo 串联 -> 给出带出处的结论”的流程回答问题。

## Workflow

### 0) 确认 `./code` 目录结构

- 默认假设仓库在当前工作目录下：`./code/<org>/<repo>/`。
- 先确认目标 repo 是 git 仓库：`test -d code/pingcap/tidb/.git` 或 `git -C code/pingcap/tidb rev-parse --is-inside-work-tree`。
- 如果 `./code` 不存在或仓库不完整：先让用户准备好代码（clone 到 `./code`）或提供正确路径。

### 1) 先确定要看哪个版本（非常重要）

不同版本的行为、参数默认值、甚至接口定义可能不同；必须先确定版本再查代码。

- 优先让用户明确版本（例：`v8.1.2`、`8.1`、`release-8.1`、`master`/`main`/`latest`）。
- 建议的默认映射（不保证每个 repo 都有该分支/标签）：
  - `vX.Y(.Z)` / `X.Y(.Z)` -> 优先尝试 `release-X.Y`（需要时再尝试 tag `vX.Y.Z`）
  - `master`/`main`/`latest` -> 优先保持当前默认分支；必要时在 `master` 和 `main` 间兜底
- 如果用户的问题明确是“线上集群某版本的行为”：必须切到那个版本再搜。
- 如果用户不关心版本：用仓库当前检出的分支，但回答里要说明“基于哪个 commit”。

### 2) 根据问题选择相关 repo（先少后多）

优先通过 repo 名称和少量元信息筛选候选 repo，再进入 repo 内进行精确搜索。

推荐使用脚本对 `./code` 下的 repo 做快速打分：

- `python3 ~/.codex/skills/read-local-code/scripts/repo_rank.py '<question or keywords>' --code-dir code --top 15`

常见的多 repo 场景（举例）：

- TiDB <-> PD：通常需要同时看 `pingcap/tidb` 与 `pingcap/pd`（以及可能的 `kvproto`/client）。
- TiDB <-> TiKV：通常需要同时看 `pingcap/tidb`（Go client）与 `tikv/tikv`（Rust server），以及 RPC/proto 定义。

### 3) 切换选中的 repo 到目标版本（git）

很多仓库都是 shallow clone，默认只包含默认分支（remote.fetch 很窄），因此必须按需 fetch 目标分支/标签。

推荐用脚本批量切换多个 repo：

- `python3 ~/.codex/skills/read-local-code/scripts/switch_code_version.py 7.5 --code-dir code --repos pingcap/tidb pingcap/pd tikv/tikv`
  - 对 `latest/master/main` 会默认拉取远端最新提交（避免浅克隆落后）；其他版本在 ref 本地不存在时会按需 fetch 分支/标签。

脚本会：

- 检查 repo 是否干净（有本地改动则拒绝切换）
- 对每个 repo 依次尝试候选 ref（如 `release-7.5` / `v7.5.2` / `master|main`）
- 成功后输出每个 repo 的 `branch/ref + short commit`

### 4) 在选定 repo 内搜索代码（先广后窄）

#### 4.1 搜索关键词建议

- 直接用：报错信息片段、日志关键字、配置名、metrics 名、SQL 关键字、CLI flag 名。
- 搜索入口：`main`/`cmd/*`、HTTP handler、gRPC service、配置加载逻辑、关键结构体方法。

#### 4.2 用 `rg` 搜索（推荐）

单 repo 精确搜索示例：

- `rg -n --smart-case --glob '!**/.git/**' --glob '!**/vendor/**' --glob '!**/target/**' --glob '!**/node_modules/**' 'pattern' code/pingcap/tidb`

多 repo 搜索时，建议用脚本输出“按 repo/文件聚合”的结果，避免被海量 raw 输出淹没：

- `python3 ~/.codex/skills/read-local-code/scripts/code_search.py 'pattern' --code-dir code --repos pingcap/tidb pingcap/pd --max-repos 5 --max-files 8 --max-matches 3`

### 5) 多 repo 联合分析（跨边界串联证据）

当问题涉及多个组件时，用“边界”来组织阅读路径：

- **API/RPC 边界**：先找 proto/接口定义，再分别在 client/server repo 里定位实现与调用。
- **配置边界**：追配置项从“解析/默认值”到“实际使用点”，必要时对比不同 repo 的同名配置（是否真的共享）。
- **错误边界**：先定位报错字符串的产生点，再回溯调用链；很多跨 repo 的错误是“下游返回，上游包装”。

### 6) 输出答案（带出处 + 版本信息）

- 每个涉及的 repo 都标注引用版本：`org/repo @ branch/ref (short commit)`
- 关键结论都附上出处：文件路径 + 行号（或至少函数/类型名 + 文件路径）；对用户输出时避免暴露本地目录名/工具名（路径尽量用 `org/repo/...` + repo 内相对路径）
- 对不确定/版本差异大的点：明确说明“在 X.Y 版本是这样，在 A.B 版本可能不同”，并给出你切换版本后的证据

## Troubleshooting

### Git 认证/Askpass 报错

某些环境里 `GIT_ASKPASS` 可能指向不存在的脚本导致 git 命令失败。脚本已默认禁用交互式认证；手工运行时可加：

- `GIT_ASKPASS= GIT_TERMINAL_PROMPT=0 <git command>`

### Shallow clone 看不到 release 分支或 tags

这是正常的：`--depth 1 --single-branch` 默认只拉了一个分支。需要按需 fetch 目标 ref（脚本会做）。

### Repo 有本地修改导致无法切分支

先 `git -C <repo> status`，然后 stash/提交/丢弃更改，再重试版本切换。
