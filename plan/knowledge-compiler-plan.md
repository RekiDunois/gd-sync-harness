# Knowledge Compiler V1 最终实施计划

> 状态：已完成 design grill，并已按当前 committed-policy / single-worker / DB schema v9 / ignored-orphan prune / batched-prune 实现复核；本文中的 V1 决策视为锁定实现约束。
> 项目：`gd-sync-harness` / `knowledge-sync`  
> 目标：在现有 Go 单体中加入 deterministic Knowledge Compiler，并通过独立 DerivedSync lane 将当前派生产物发布到 Google Drive，供 ChatGPT 使用。

---

## 0. 结论摘要

V1 不是另一个 agent，也不是普通同步的一个特殊文件夹。

最终架构是：

```text
local profile source / Vault
        |
        | shared committed eligibility policy
        v
Deterministic Knowledge Compiler
        |
        | immutable local generation
        v
~/.local/share/knowledge-sync/compiler/<profile_uuid>/
        |
        | desired derived generation
        v
existing single worker
        |
        | dedicated DerivedPublisher correctness flow
        v
Drive/<profile remote root>/.knowledge-derived/
        |
        v
ChatGPT
```

同时保留两条严格分离的数据 lane：

```text
Ordinary lane
local source files
  -> normal reconciliation / fast upsert / prune
  -> profile Drive root

Derived lane
local compiler immutable generation
  -> DerivedPublisher
  -> reserved remote .knowledge-derived/
```

核心边界：

1. compiler 只计算确定性结构事实，不调用 LLM、embedding 或远端 AI；
2. compiler 输入严格复用 ordinary lane 的 committed-policy strict eligibility helper；`profile_ignore_policy` + committed snapshot 是唯一 path-policy authority；
3. compiler 输出位于 app-owned state dir，**不写入 Vault**；
4. `.knowledge-derived/**` 是 system-reserved namespace，普通 sync 永远不上传、修改或删除它；
5. DerivedPublisher 是该 namespace 的唯一 data-plane owner；
6. DerivedSync 与普通 sync 共享 rclone execution、worker、profile lock / remote lease、progress / error / attempt 框架，但不共享 correctness algorithm；
7. 远端只暴露平铺的 current view，不暴露 generation UUID 目录；
8. local immutable generation + manifest-last remote commit 共同提供 crash-safe、可验证的 publication 语义；
9. syntax facts 必须来自固定版本 Goldmark parser pipeline 的结构化结果；通常使用固定版本第三方 parser/extension，若其无法满足已表征的语法契约，允许使用 narrowly scoped project-owned Goldmark extension，但不得引入 raw-text fallback scanner 或第二套 Markdown lexer；
10. full compile 是 V1 correctness authority；自动化、增量 cache、partition、semantic layer 全部后置。
11. `compile` / `compiler status` / local `compiler clean` 不发现、不探测、不执行 rclone；远端工作只由 worker 执行；
12. remote publication knowledge 与 remote binding fingerprint 绑定，migration 后不能复用旧 binding 的 published 结论。

---

# Part A：产品边界与目标

## 1. 目标

增加一个本地 deterministic **Knowledge Compiler**，用于回答必须穷举整个 Chat-visible corpus 才能可靠回答的结构问题，例如：

- 哪些 note 是 hard orphan；
- 哪些 note 没有 backlink；
- 每个 note 的确切 incoming / outgoing link 结构；
- 哪些本地 link unresolved 或 ambiguous；
- 哪些 attachment 被引用；
- 每个 tag 出现在哪些 note、出现多少次；
- frontmatter 字段的实际存在率、null/type 分布；
- Chat-visible corpus 的精确文件集合与结构统计。

原则：

> parser + filesystem traversal + deterministic resolver + graph aggregation 能精确回答的事实，一律在本地编译，不交给模型猜。

ChatGPT 继续负责：

- 对 source note 做语义理解；
- 根据 deterministic report 解释知识结构；
- 比较主题、提出重组建议；
- 对 orphan / broken-link / tag 分布做语义分析。

ChatGPT 不成为 exhaustive structural counts 的 source of truth。

---

## 2. V1 非目标

V1 不实现：

- 自动修改或重写 note；
- 自动补 wikilink；
- semantic orphan detection；
- topic clustering；
- conceptual duplicate detection；
- vector DB；
- embedding；
- 本地或远端 LLM 调用；
- Obsidian GUI / Electron / plugin runtime；
- `.obsidian/` plugin state 解析；
- 自动 quiet-window compile；
- event-driven incremental compiler；
- persistent parsed-file cache；
- JSONL partition；
- semantic artifacts。

这些只有在 V1 correctness、规模和 Chat 使用数据出来后再决定。

---

# Part B：与现有 knowledge-sync 的集成边界

## 3. 同一个 profile，不创建第二套 source identity

Compiler 运行在现有 `knowledge-sync` profile 上。

至少读取：

```text
profile id
profile uuid
profile type
source path
committed source eligibility policy
remote binding metadata（仅 DerivedSync 使用）
```

V1 支持 profile type：

```text
obsidian
generic
```

两者使用同一套 Markdown parser / resolver / artifact schema 语义；profile type 不分叉 parser contract。

Compiler 不创建第二个 profile，不允许重叠 source root，也不拥有独立的用户可编辑 exclude policy。

Source identity 必须按 resolved filesystem path 校验：

```text
source root itself must be a real directory, not a symlink
source root must not contain, equal, or sit inside app state root
all non-forgotten profiles, including tombstoned profiles, reserve their source roots
restore must re-run local + remote overlap validation before reactivation
```

这保证 `~/.local/share/knowledge-sync/compiler/...` 不可能落进任何 profile source。现有 profile 若违反该约束，compiler fail closed 并给出 actionable validation error；不得一边写 generation，一边依赖 source-stability retry 消化自己制造的 events。

---

## 4. Compiler corpus 必须严格复用 committed eligibility policy

Compiler input corpus 定义为：

> 当前本地 source 中，按照 profile 已提交的普通同步 eligibility contract 判定为 active + eligible 的 regular-file 集合。

必须复用普通同步的 authoritative eligibility helper；不得在 `internal/compiler` 里复制一套 exclude / gitignore / max-file-size / symlink 判定逻辑。

当前实现中，path-policy authority 固定为：

```text
profile_ignore_policy row
  + profile_ignore_snapshot_files committed bytes
  -> policy.Snapshot
```

读取 API 必须返回一个不可歧义的 committed policy bundle：

```text
policy row exists + zero snapshot files = valid empty policy
policy row missing / hash inconsistent / partial rows = hard error
```

不得继续沿用“`GetCommittedSnapshot == nil` 同时表示 valid empty 和 policy missing”的语义，也不得在缺失时回落到空 matcher。

`Profile.Excludes` / `profile_excludes` 是 legacy migration input，不是 runtime policy source。旧 structured excludes 只有在已经被物化为 `policy_source=legacy_migrated` 的 committed snapshot 后才影响 compiler；compiler 不得把 `Profile.Excludes` 再叠加一次。

当前 profile creation 仍会把部分 Obsidian defaults 写入 legacy `profile_excludes`，但 active scanner 不消费这些 row。Compiler 必须忠实跟随 committed ordinary corpus，不能在 compiler 内“顺手恢复”这些 defaults；若产品仍要求这些 defaults 生效，应作为独立 ordinary policy/profile-creation 修复，把它们显式物化进 committed policy 后再由两条 lane 共同读取。

当前 profile row 与 initial committed policy 分两个 transaction 创建，enabled worker 可能在中间窗口看到“有 profile、无 policy”。Phase 0 必须改成单个 DB transaction 创建 profile/runtime/sync-state/policy bundle，或让 profile 保持不可运行直到 policy transaction 成功；不得让 worker/compiler 在该窗口用空 policy 继续。

磁盘上的 `.gitignore` 修改在执行 `profile ignore update` 并形成新的 committed snapshot 前，不改变 ordinary 或 compiler corpus。

共享 eligibility contract 至少包括：

```text
committed ignore snapshot
max_file_size
regular-file-only rule
no-follow symlink rule
system-reserved paths
```

共享 helper 必须同时提供：

```text
strict full traversal
single-path classification for fast events
sorted metadata entries: rel_path + size + mtime_unix_nano
```

`os.ReadDir` / `Lstat` / eligible regular-file metadata 读取失败是 hard error，不得像当前普通 scanner 的旧行为一样静默 `continue`。被 committed policy 或 system reservation 排除的 subtree 不进入、不读取，也不把 path 写入 warning。

Manifest / run 中区分两个 hash：

```text
policy_hash
  = profile_ignore_policy.policy_hash

eligibility_contract_hash
  = versioned SHA-256(
      policy_hash,
      max_file_size,
      regular-file rule version,
      symlink rule version,
      system-reservation version
    )
```

`eligibility_contract_hash` 使用 versioned canonical encoding，不能依赖 Go map iteration。

安全 invariant：

如果一个 path 对 ordinary source corpus 不 eligible，compiler 不得读取它，也不得向任何 derived artifact 泄漏：

```text
filename
path
tag
frontmatter
link membership
content hash
content-derived metadata
```

Compiler 建模的是 **active eligible local corpus**，不是 Drive 上物理存在的全部对象；被 policy suppress 的旧 remote object 不进入 compiler graph。

---

## 5. `.knowledge-derived/` 从 V1 起就是 system-reserved namespace

Compiler 产物不位于 source / Vault，但远端 current view 固定使用：

```text
<profile remote root>/.knowledge-derived/
```

因此该 path 从 V1 起属于 system reservation：

```text
ordinary lane:
  never uploads .knowledge-derived/**
  never modifies .knowledge-derived/**
  never deletes .knowledge-derived/**

derived lane:
  sole owner
```

用户 exclude / negation policy 不得覆盖该 reservation。

实现上不能把它仅仅表达成一个普通 rclone `--exclude` 后继续依赖 `--delete-excluded`，因为那会把“排除”与“保护”语义混在一起。

必须在 app-owned desired/protected/delete decision 层把它标记为 hard protected namespace，保证 ordinary reconcile 的 deletion plan 永远不会包含该 subtree。

当前 active/suppressed managed-ledger 架构需要增加：

```text
manifest state = protected
```

Additive migration 把历史上路径等于 `.knowledge-derived` 或以 `.knowledge-derived/` 开头的 ordinary manifest row 转成 `protected`。`protected` row：

```text
never active upload input
never proven ordinary delete
never suppression/prune candidate
never targeted remote-delete input
```

同一个 additive migration 必须在把 ledger row 转成 `protected` 前，找出所有包含 reserved target 的非终态 prune request，并原子标记为 `stale`；不能让 pre-upgrade `prune_targets` 保留已经冻结的删除授权。即使 migration 已处理，prune executor 仍必须在每个 remote delete batch 前重新执行 canonical-path + reserved-namespace guard。

所有来自 event / manifest / prune / migration DB row 的 remote target 在拼接 remote path 前必须满足 canonical relative-path contract：

```text
non-empty slash-separated relative path
no leading slash
no backslash or NUL
no empty, ".", or ".." segment
path.Clean(value) == value
```

先验证 canonical path，再检查 root first segment 是否为 `.knowledge-derived`。Malformed historical target 一律 fail closed，不能靠 prefix check 猜测。

除 ledger state 外，以下边界还必须各自有最终防线：

```text
strict full scan / files-from
fast-event single-path eligibility
disappearance classification
prune preview and frozen targets
ordinary targeted delete execution
worker-owned migration copy
explicit root-content purge gate
```

历史 remote `.knowledge-derived/` 若 non-empty 且没有 valid derived sidecar，仍按首次 claim 规则 fail closed；ordinary lane 不得替用户删除或自动 adopt。状态输出必须提示用户先把该 subtree 移走或清空后重试。

### 5.1 现有 prune 模型与 reserved namespace 的关系

当前代码已有两种需要纳入 V1 保护契约的 prune target：

```text
suppressed-managed target
  -> 来自 policy refresh 后的 suppressed manifest row
  -> CreatePrunePreview 冻结

ignored-unmanaged target
  -> `prune discover` 从远端发现、同时满足 committed ignore matcher 且不在 managed ledger
  -> CreatePrunePreviewFromUnmanagedPaths 冻结
```

两类 target 都必须记录 immutable candidate set、policy hash、candidate digest、authorization limit，并经历同一套 ownership / canonical-path / reserved-path gate。unmanaged target 不得因为进入 prune request 而被写入 manifest，也不得在 missing 处理时删除 active manifest ownership；只有 missing 的 suppressed-managed target 可以清除对应的 suppressed ownership。

`prune discover` 是非破坏性的远端读取，可以由 CLI 发起，但必须先验证 ordinary sidecar 和 live root binding；冻结 target 前与写入 `prune_targets` 前都必须调用同一个 strict validator。发现或候选归一化不得把反斜杠替换成 `/`、不得 `TrimSpace`、不得用 `path.Clean` 掩盖 `.` / `..`，任何 malformed candidate 直接拒绝。

当前 batched prune 的行为固定纳入实现约束：

```text
existence classification: one files-from-raw recursive listing
delete batch size: at most 512 targets
delete transport: rclone delete --files-from-raw with batch max-delete
after each batch: transactional target/request counters checkpoint
partial batch failure: leave unconfirmed targets pending/retrying
retry: re-list; already absent targets become missing, remainder resumes
```

每个批次执行前仍要重新验证 canonical target 和 `.knowledge-derived` reservation；prune executor 使用 `delete` 批处理而非逐目标 `deletefile`，但这不降低最终防线。未知或越界 path 必须在 listing 前失败，不能进入 rclone files-from list。

---

# Part C：本地 compiler state 与 generation 模型

## 6. 本地产物位置

使用现有 app state root：

```text
~/.local/share/knowledge-sync/
```

每个 profile 使用稳定 `profile_uuid` 隔离：

```text
~/.local/share/knowledge-sync/compiler/<profile_uuid>/
  MANIFEST.json
  generations/
    <generation-uuid>/
      MANIFEST.json
      README.md
      FILE_INDEX.jsonl
      LINKS.jsonl
      ORPHANS.jsonl
      ORPHANS.md
      BROKEN_LINKS.jsonl
      BROKEN_LINKS.md
      TAG_INDEX.jsonl
      FRONTMATTER_STATS.json
      WARNINGS.jsonl
      KNOWLEDGE_HEALTH.md
  staging/
```

绝不生成：

```text
<profile source>/.knowledge-derived/
```

因此：

- Vault 中没有 generated reports；
- fswatch 不会因为 compiler 输出制造普通 source events；
- local generation GC 不会触发普通 reconciliation；
- compiler output 不受 profile `max_file_size` / gitignore eligibility 约束；
- output transport 只走 DerivedSync。

---

## 7. Run ID、Generation ID、source snapshot

三个 identity 分开：

### 7.1 compiler run

每次用户发起 compile 创建：

```text
compiler_run_id = UUID
```

如果第一次 source-stability 检查失败并自动重试一次，**重试继续使用同一个 run ID**。

### 7.2 compile generation

每次成功 local publication 生成：

```text
compile_generation_id = UUID
```

失败 compile 不生成 published generation。

### 7.3 source snapshot

`source_snapshot_id` 是 deterministic SHA-256，用于描述一次有效 compiler input snapshot。

输入至少包括：

```text
snapshot_encoding_version
policy_hash
eligibility_contract_hash
sorted eligible membership
all eligible file size + mtime_unix_nano metadata
Markdown content SHA-256
```

Canonical encoding 必须稳定、versioned，不能依赖 Go map iteration。

同一 source/schema/compiler 下结构化结果必须 deterministic；run UUID、generation UUID、timestamp 可以不同。

---

## 8. Local root `MANIFEST.json` 只是 current pointer

本地 compiler root：

```text
compiler/<profile_uuid>/MANIFEST.json
```

不是 full artifact manifest，而是小型 local commit pointer，例如：

```json
{
  "schema_version": 1,
  "profile_id": "example-profile",
  "profile_uuid": "...",
  "current_generation_id": "uuid-C",
  "generation_manifest_path": "generations/uuid-C/MANIFEST.json",
  "generation_manifest_bytes": 1234,
  "generation_manifest_sha256": "...",
  "published_at": "..."
}
```

它只用于本地 crash-safe current selection，**绝不上传到 Drive**。

远端 `.knowledge-derived/MANIFEST.json` 使用的是当前 generation directory 里的 full immutable manifest。

---

## 9. Immutable generation publication

Local compile publication 顺序：

```text
create staging/<run-or-generation>/
  -> generate all detail artifacts
  -> fsync / close
  -> write full generation MANIFEST.json last inside staging
  -> fsync directory as appropriate
  -> atomic rename staging -> generations/<generation-id>
  -> atomically replace root MANIFEST.json pointer
  -> update SQLite operational state
  -> retention GC
```

Failure boundary 固定为：

```text
before root pointer replacement
  -> compile failed
  -> old pointer remains current

root pointer replacement succeeds
  -> local publication committed
  -> new generation is authoritative even if later SQLite update fails

SQLite update fails after pointer commit
  -> do not roll back/delete the new generation
  -> return local_published_state_pending_repair
  -> next app/worker recovery verifies pointer + manifest and repairs run/profile state
```

因此“失败 compile 保留 old current”只适用于 root-pointer commit 前；pointer commit 后不能再把该 run 记录成普通 failed，也不能声称 old generation 仍 authoritative。

原则：

> local root pointer 是 local publication fact；SQLite 是 operational state，不是 publication authority。

Crash repair 必须能从 root pointer + immutable generation manifest 恢复 SQLite 状态。

---

## 10. Full generation `MANIFEST.json`

每个 immutable generation 目录中的 `MANIFEST.json` 是 portable full manifest；它同时也是远端最终发布的 `MANIFEST.json`。

至少包含：

```text
schema_version
compiler_version
profile_id
profile_uuid
compiler_run_id
compile_generation_id
compiled_at
source_snapshot_id
policy_hash
eligibility_contract_hash
aggregate counts
artifact inventory
```

Artifact inventory 以 generation-root-relative filename 为 key：

```json
{
  "artifacts": {
    "FILE_INDEX.jsonl": {
      "sha256": "...",
      "bytes": 12345,
      "records": 20384
    },
    "KNOWLEDGE_HEALTH.md": {
      "sha256": "...",
      "bytes": 4096
    }
  }
}
```

`artifacts` inventory 覆盖 generation 中除 `MANIFEST.json` 自身之外的所有 artifact。`MANIFEST.json` 不得收录自己的 hash/bytes，否则形成不可解的自引用；generation manifest 自身的 bytes/hash 只由 local root pointer 记录，并通过 remote manifest-last commit 作为远端 commit marker。

禁止写入：

```text
absolute local paths
~/.local/share/...
generations/<uuid>/... remote-only references
```

这样同一个 manifest 在：

```text
local generations/C/
remote .knowledge-derived/
```

都成立。

---

## 11. Local retention 与 generation pin

正常 steady state 仅保留：

```text
current
previous
```

即 2 个 immutable generations。

GC 只在新的 local root pointer 成功 commit 后运行。

如果 GC 删除失败：

```text
compile 仍然成功
记录 local GC warning/debt
后续 compile / clean 再收敛
```

不能因为磁盘清理失败反判已经成功的 compile 失败。

### 11.1 in-flight pin

DerivedPublisher 开始读取 generation A 时必须创建 in-flight pin：

```text
active_publish_generation = A
```

GC 的可删除条件：

```text
not current
and not previous
and not active_publish_generation
```

因此允许临时出现：

```text
A = in-flight pinned
B = previous
C = current
```

网络 attempt 完成后解除 pin，再做 best-effort GC。

Compiler lock 覆盖一个完整 local compile，以及 local publication / clean / GC；它不持有 profile lock，因此 ordinary sync 和已开始的 DerivedSync 网络上传不被 local compile 阻塞。DerivedPublisher 只在 pin / unpin / state mutation 时短暂持有 compiler lock，整个网络上传期间必须释放该锁，从而允许下一次 compile 生成更新的 desired generation。

### 11.2 lock ordering 与 lifecycle serialization

需要同时触碰 profile lifecycle 与 compiler state 的路径固定使用：

```text
profile lock -> compiler lock
```

包括：

```text
profile ignore update
profile remove / restore / forget
profile migrate cutover
profile disable / enable 的 compiler-state transition
worker pin / unpin
```

`compile` / `compiler clean` 只获取 compiler lock，绝不在持有 compiler lock 时再请求 profile lock。上述 lifecycle command 必须在取得 compiler lock 后复查 profile identity / tombstone / deletion-requested 状态，避免 compile 与 forget/remove 交错后重新创建已删除 identity 的 state。

当前 `flock.Acquire` 遇到 live owner 会立即返回 `ErrLocked`。要求“已开始的 compile 先完成”时，CLI mutation 必须使用 shared cancellable wait wrapper（带状态提示、受 context cancellation 控制，禁止 tight spin）；worker 的周期性 scheduler 可继续 non-blocking skip，下个 pass 重试。不能在文档里假设现有 lock primitive 自动等待。

Derived attempt 的 generation selection 与 pin 必须在同一个 compiler-lock critical section 内完成：

```text
re-read lifecycle + desired revision + binding
verify root pointer hash and generation manifest
verify generation directory exists
create derived run claim + active pin
release compiler lock
start network work
```

Recovery 不得只遍历 enabled active profiles；它必须扫描全部 `compiler_profile_state`，包括 disabled/deleting/tombstoned identities，才能把 orphaned derived run 标为 interrupted 并清理 stale pin。删除 identity 的 forget 仍受同一 compiler lock 串行化。

---

# Part D：source stability 与 corpus correctness

## 12. Stable source snapshot

Compiler 必须保证发布的 artifacts 对应一个稳定 source snapshot。

流程：

```text
scan 1
  -> determine eligible membership
  -> all eligible file size/mtime_unix_nano
  -> Markdown SHA-256
  -> policy_hash
  -> eligibility_contract_hash
  -> parse/build artifacts

pre-publication validation
  -> recompute eligible membership
  -> re-read all eligible file size/mtime_unix_nano
  -> rehash Markdown
  -> re-read committed policy hash
  -> recompute eligibility_contract_hash
```

任一变化：

```text
source unstable
```

行为：

1. 自动完整重试一次；
2. 重试使用同一个 `compiler_run_id`；
3. 第二次仍不稳定，run 失败为 `source_unstable`；
4. 旧 current generation 保持不变。

不发布“尽力而为”的 mixed snapshot。

---

## 13. File hashing

Markdown note：

```text
content_sha256 = SHA-256(raw file bytes)
size + mtime_unix_nano retained
```

非 Markdown attachment：

```text
content_sha256 = null / omitted
size + mtime_unix_nano retained
```

V1 不为了 FILE_INDEX 对 PDF、图片、视频等全部做 content hash；Derived artifact 自身仍由 generation manifest 做完整 SHA-256。

---

# Part E：Parser 与 syntax contract

## 14. Parser architecture

V1 使用纯 Go parser stack，不依赖 Obsidian、Node、Electron 或 plugin runtime。

候选 stack：

```text
github.com/yuin/goldmark
go.abhg.dev/goldmark/wikilink
go.abhg.dev/goldmark/hashtag   (ObsidianVariant)
go.abhg.dev/goldmark/frontmatter
go.yaml.in/yaml/v3
```

候选 package 名不是预先接受的 correctness 结论。实施先用精确版本建立 characterization spike；只有全部 mandatory fixtures 通过后，该组合才成为 accepted parser stack，并作为 direct dependencies 固定在 `go.mod`。禁止运行时依赖 floating/latest 行为。

Mandatory gate 至少包括：

```text
complete initial YAML frontmatter only
unmatched opening --- does not consume the body
malformed complete YAML produces warning while body still parses
fenced/inline code suppresses fake links and tags
escaped \| wikilink inside Markdown table
wikilink/embed/Markdown-link structured facts
```

如果候选组合不能满足 gate，必须换第三方 parser/extension 或显式重新 review syntax contract；不得通过 raw-text fallback scanner、正则补边或 project-owned 第二 lexer 绕过 gate。

Note kind 的 extension contract 固定为：ASCII case-insensitive `.md`；canonical path identity 仍保持原始大小写，`extension` 字段仍保留 filesystem 实际拼写。

可以在开发阶段使用 Obsidian 作为 compatibility oracle 做手工/fixture 对照，但：

- 不是 runtime dependency；
- 不是默认 CI dependency；
- V1 correctness 不依赖 Obsidian 可执行文件存在。

---

## 15. 禁止二次手写 Markdown lexer

这是硬约束。

以下 syntax facts 必须来自选定 parser/extension 的结构化结果：

```text
wikilink
embed
Markdown link
inline tag
frontmatter region
code regions
source location（仅库可靠提供时）
```

Compiler 可以做：

```text
path resolver
candidate matching
dedup
aggregation
counts
sorting
serialization
```

Compiler 不可以为了“补一个 edge”“拿行号”“兼容表格转义”等，再扫描原始 Markdown 写第二套 link/tag lexer。

如果 parser 无法可靠提供某个 optional source location，该字段保持 absent/null，而不是自己重新解析文本。

---

## 16. Frontmatter 语义

仅识别文件开头的完整 YAML delimiter block：

```text
---
...
---
```

规则：

- 只认初始 frontmatter；
- unmatched opening `---` 不作为 hard compile failure；
- 完整 delimiter 内 YAML malformed：产生 warning，body 仍继续解析；
- arbitrary frontmatter field 只做 deterministic metadata/type aggregation，不赋予业务语义。

Tags / aliases：

- scalar 或 sequence of scalar 支持；
- mapping / mixed sequence 产生 warning；
- null 不产生 tag/alias value；
- aliases 仅作为 metadata，**不参与 resolver**。

`FRONTMATTER_STATS` 使用 compiler-owned、closed、fixture-locked type vocabulary，不暴露 Go runtime concrete type 名称。

---

## 17. Tag 语义

Inline tag syntax 以最终通过 characterization gate 的 hashtag extension 的 Obsidian-compatible variant 结构化结果为准；当前首选候选是 `go.abhg.dev/goldmark/hashtag` 的 `ObsidianVariant`。

Normalization：

```text
remove exactly one leading '#'
lowercase
preserve '/'
no Unicode normalization
```

Frontmatter scalar tag 不按空格或逗号额外 split；parser/YAML 提供什么 scalar，就按一个 value 处理。

每个 normalized tag 保留：

```text
note_count
occurrence_count
observed variants
referencing paths
```

Paths、variants 均 deterministic sort + dedup。

---

## 18. Link syntax scope

至少处理第三方 parser 能结构化识别的：

```text
[[Note]]
[[Folder/Note]]
[[Note#Heading]]
[[Note#^block]]
[[Note|Display]]
![[Attachment.pdf]]
![[Note#Heading]]
[Markdown link](relative/path.md)
```

真实 corpus characterization 必须覆盖 Markdown table 内 escaped pipe，例如：

```text
[[目标/Note\|Display]]
```

External URL 不进入 `LINKS.jsonl`。

只有 local-looking link/embed 才进入 local graph stream。

---

## 19. Markdown local URI 语义

对 Markdown local target：

- parser 负责识别 link syntax；
- resolver 对 path component 做一次 URL decode；
- `+` 不转换为空格；
- raw target 保留；
- fragment 保留；
- fragment 不在 V1 验证 heading/block 是否真实存在。

`<...>` 包围形式由 parser-level Markdown semantics 处理，不由 compiler 手工 strip guessing。

---

# Part F：Path identity 与 resolver

## 20. Canonical path

Canonical identity：

```text
relative path from profile source root
raw filesystem UTF-8 spelling
case-sensitive
no Unicode normalization
```

不要 basename-only identity；不要 lowercasing path；不要 NFC/NFD rewrite。

### 20.1 `path_id`

`FILE_INDEX` 增加稳定 machine key：

```text
path_id = lowercase hex SHA-256(canonical relative path UTF-8 bytes)
```

始终输出完整 64 hex，不截断。

Path 仍是 V1 authoritative identity；文件 rename/move 后 path 与 `path_id` 一起变化。

其他 artifact 不强制重复 source/target path_id，避免 schema 膨胀。

---

## 21. Resolver ownership

Resolver 是 project-owned deterministic layer，但只接收 parser 已识别的 target facts。

原则：

> uniquely resolvable -> resolved；存在多个 plausible eligible candidates -> ambiguous；没有 candidate -> unresolved；永不猜。

规则至少包括：

1. eligible corpus 外的 path 永远不是 candidate；
2. explicit local/canonical path 优先；
3. Markdown note target 可容忍 `.md` omission；
4. wikilink basename resolution 仅在 candidate 唯一时成功；
5. ambiguous candidate paths deterministic sort；
6. aliases 不参与 resolution；
7. case-sensitive；
8. symlink 不跟随；
9. heading/block fragment 在 file resolution 前分离，但完整 raw target + fragment 保留；
10. attachment 可解析为 `target_kind=attachment`；note 为 `target_kind=note`。

V1 不声称完全复制 Obsidian 的所有 undocumented resolver behavior；如果以后 compatibility fixtures 证明存在 materially different rule，通过 schema/compiler version 明确升级，不做 silent semantic change。

---

# Part G：Graph 与 deterministic classification

## 22. `LINKS.jsonl` 是 canonical edge stream

每个 local-looking parsed occurrence 形成一条 edge record；重复 link occurrence 不丢失。

建议字段层次：

```text
schema_version
compile_generation_id
source_path
link_kind
raw_target
display_text (optional)
fragment (optional)
resolution_status
resolved_target_path (nullable)
target_kind (note|attachment|null)
candidate_paths (ambiguous only, sorted)
source_location (optional; only parser-provided)
```

`resolution_status` 至少：

```text
resolved
unresolved
ambiguous
```

External URL 不记录为 edge。

---

## 23. Edge occurrence count 与 unique-note connectivity 分开

例如：

```markdown
A.md
[[B]]
[[B]]
[[B]]
```

必须同时表达：

```text
3 edge occurrences
1 unique outgoing note neighbor
```

因此 note-level aggregates 至少包括：

```text
incoming_edge_count
outgoing_edge_count
incoming_note_count
outgoing_note_count
self_link_count
unresolved_link_count
ambiguous_link_count
```

Orphan classification 使用 **unique non-self note counts**，不是 edge occurrences。

---

## 24. Self link / embeds connectivity

规则：

### Self link

- edge 保留在 `LINKS.jsonl`；
- 计入 `self_link_count`；
- 不增加 orphan connectivity。

因此只有 self-links 的 note 仍是 hard orphan。

### Note embed

`![[OtherNote]]`：

- 形成 edge；
- `target_kind=note`；
- 参与 note connectivity。

### Attachment embed

`![[image.png]]`：

- 形成 edge；
- `target_kind=attachment`；
- 不参与 note orphan connectivity。

---

## 25. Orphan definitions

V1 固定三类：

### Hard orphan

```text
incoming_note_count == 0
AND
outgoing_note_count == 0
```

### No-backlink note

```text
incoming_note_count == 0
```

### Outbound-only note

```text
incoming_note_count == 0
AND
outgoing_note_count > 0
```

`ORPHANS.jsonl` 保留 underlying counts，使未来 presentation/policy 可以不重新解析 source 就重新分类。

一个 note 只有 unresolved outgoing target 时：

```text
outgoing_note_count = 0
unresolved_link_count > 0
```

必须把这两个事实分开表达。

---

## 26. Broken local links

Broken local link 定义：

> parser 已识别为 local-looking target，但 resolver 无法在 eligible corpus 中唯一解析。

使用：

```text
unresolved
ambiguous
```

分别表示，无需把二者压成一个模糊 boolean。

普通 external URL 不属于 broken local link。

---

# Part H：Artifact contract

## 27. V1 artifact set

每个 generation 固定生成：

```text
README.md
MANIFEST.json
FILE_INDEX.jsonl
LINKS.jsonl
ORPHANS.jsonl
ORPHANS.md
BROKEN_LINKS.jsonl
BROKEN_LINKS.md
TAG_INDEX.jsonl
FRONTMATTER_STATS.json
WARNINGS.jsonl
KNOWLEDGE_HEALTH.md
```

原草案中的 `VAULT_HEALTH.md` 改名为：

```text
KNOWLEDGE_HEALTH.md
```

避免 generic profile 暴露 Obsidian-specific 名称。

所有 detail artifact 都属于同一个 `compile_generation_id`。

JSONL 每 row 明确携带 generation ID；aggregate JSON / Markdown 在 header/metadata 中携带 generation ID。

---

## 28. `README.md`

这是 ChatGPT / human 的稳定入口。

内容必须简洁说明：

```text
这个目录由 knowledge-sync 管理，不要手工写文件
当前 generation / compiled_at
一致性判断以 MANIFEST.json 为准
KNOWLEDGE_HEALTH.md：结构总览
ORPHANS.md：孤立 note 摘要
BROKEN_LINKS.md：broken/ambiguous 摘要
TAG_INDEX.jsonl：tag 精确索引
FILE_INDEX.jsonl：完整 eligible file index
LINKS.jsonl：完整 local edge stream
FRONTMATTER_STATS.json：frontmatter aggregate
WARNINGS.jsonl：compiler warnings
```

README 本身也是 generation artifact，进入 full manifest hash inventory。

---

## 29. `FILE_INDEX.jsonl`

一条记录对应一个 eligible regular file。

### 29.1 kind vocabulary

固定：

```text
note
attachment
```

规则：

```text
eligible file with ASCII case-insensitive .md extension -> note
other eligible regular file -> attachment
```

扩展名单独记录，保留 filesystem 实际拼写；不通过 MIME sniffing 改写 kind。

### 29.2 note fields

至少：

```text
schema_version
compile_generation_id
path
path_id
kind=note
extension
size
mtime_unix_nano
content_sha256
frontmatter field summary
tags
incoming_edge_count
outgoing_edge_count
incoming_note_count
outgoing_note_count
self_link_count
unresolved_link_count
ambiguous_link_count
```

### 29.3 attachment fields

至少：

```text
schema_version
compile_generation_id
path
path_id
kind=attachment
extension
size
mtime_unix_nano
content_sha256 = null/omitted
```

绝不把 source file content 放入 FILE_INDEX。

---

## 30. `ORPHANS.jsonl` / `ORPHANS.md`

Machine stream：

- path；
- generation；
- hard_orphan / no_backlink / outbound_only classification；
- incoming/outgoing unique-note counts；
- edge counts；
- unresolved/ambiguous counts；
- self link count。

`ORPHANS.md` 是 compact Chat-facing view：

```text
generation metadata
summary counts
hard orphans grouped by top-level folder
no-backlink summary
outbound-only summary
```

不要把 exhaustive edge detail 重复塞进 Markdown；详细事实留在 JSONL。

---

## 31. `BROKEN_LINKS.jsonl` / `BROKEN_LINKS.md`

JSONL 来源必须是同一份 resolved edge stream，不二次 parse source。

Markdown summary 按：

```text
resolution status
raw target
source folder
```

做 deterministic grouping。

Ambiguous entry 显示 sorted candidate paths。

---

## 32. `TAG_INDEX.jsonl`

每个 normalized tag 一行：

```text
schema_version
compile_generation_id
tag
note_count
occurrence_count
variants[]
note_paths[]
```

`variants`、`note_paths` deterministic sort + dedup。

---

## 33. `FRONTMATTER_STATS.json`

聚合 top-level frontmatter key：

```text
notes_total
notes_with_frontmatter
field name
present count
null count
observed stable type counts
```

不判断字段“是否正确”“是否重要”。

---

## 34. `WARNINGS.jsonl`

Warnings 属于正式 artifact。

至少需要稳定字段：

```text
schema_version
compile_generation_id
code
path (nullable when global)
source_location (optional)
message/details
```

必须 deterministic sort。

典型 warning：

```text
frontmatter_malformed
frontmatter_tag_type_unsupported
frontmatter_alias_type_unsupported
parser_nonfatal_issue
```

如果一个 eligible file 因 I/O / permission 等原因根本无法读取，compiler 不能继续声称 exhaustive snapshot；这种情况是 hard failure，不降级成 warning。

Warnings 只能包含 eligible corpus path，不得泄漏被排除 path。

---

## 35. `KNOWLEDGE_HEALTH.md`

Chat-facing deterministic summary，建议固定章节：

```text
Corpus
Links
Orphans
Broken / Ambiguous Links
Tags
Frontmatter
Compiler Warnings
```

必须区分：

```text
fact
schema-defined classification
```

例如：

```text
Fact: 1,243 notes have zero resolved incoming note neighbors.
Schema v1 classification: 412 are hard orphans.
```

不加入 model inference。

---

## 36. Deterministic ordering

所有输出必须定义 stable ordering：

```text
paths -> canonical byte/string order
candidate paths -> sorted
links -> source path + parser occurrence order / stable source location + target fields
warnings -> path + code + source location
tags -> normalized tag
frontmatter keys -> key sort
```

禁止依赖 Go map iteration。

同一 source snapshot / schema / compiler behavior 下，除 UUID/timestamp metadata 外结构化顺序和计数必须一致。

---

# Part I：SQLite control plane

## 37. SQLite 是 operational state，不是 knowledge graph authority

不要把 V1 graph 全量存 SQLite。

Canonical structural artifacts 位于 immutable generation files；SQLite 用于：

```text
run lifecycle
last local success
current desired derived state
remote publication state
remote binding identity
errors / retry
in-flight generation pin
status
```

未来 persistent parse cache 是否值得做由 benchmark 决定。

---

## 38. `compiler_runs`

Additive table，至少表达：

```text
id = compiler_run_id
profile_id
candidate_generation_id
started_at
completed_at
status
compiler_version
schema_version
source_snapshot_id
policy_hash
eligibility_contract_hash
file_count
warning_count
error
```

Status：

```text
running
succeeded
failed
interrupted
```

Worker/app recovery 时 stale `running` run 可修复为 interrupted，或在 local root pointer 已证明 publication 成功时 repair 为 succeeded。

Full generation MANIFEST 必须记录 `compiler_run_id`，从而支持：

```text
root pointer -> generation C
C/MANIFEST.compiler_run_id -> R
R still running after crash
=> verify manifest
=> repair R succeeded
```

### 38.1 `compiler_derived_runs`

Derived attempt history 使用独立 additive table，不写入普通 `sync_runs` 的 integer source-generation 模型。至少表达：

```text
id
profile_id
kind                       // publish | purge
target_generation_id       // nullable for purge
target_binding_fingerprint
target_desired_revision
status
phase
started_at
completed_at
progress counters
error_code
error_classification
error
```

它复用现有 retry vocabulary、progress model 和 attempt shell，但 success/failure commit 只更新 compiler state，不更新 `profile_sync_state.last_error`。

---

## 39. `compiler_profile_state`

每 profile 维护 local + derived operational state。

概念字段：

```text
profile_id
last_success_generation_id
last_success_at
last_source_snapshot_id
last_policy_hash
last_eligibility_contract_hash
last_compile_error
local_mode                  // generation | absent
local_clean_state           // none | committing
local_clean_operation_id
forget_state                // none | committing
forget_operation_id

desired_derived_mode       // generation | absent
desired_derived_generation_id
desired_derived_revision
current_remote_binding_fingerprint
remote_published_generation_id
remote_published_binding_fingerprint
remote_state                // absent | absent_unclaimed | generation | unknown
remote_state_binding_fingerprint
derived_state               // pending | syncing | current | failed | blocked_disabled
active_publish_generation_id
derived_retry_target_key
derived_retry_classification
derived_consecutive_failures
derived_limited_failures
derived_next_retry_at
derived_terminal_error_code
last_derived_error
last_derived_success_at
```

重点：

- desired state 是 **level-triggered current desire**，不是 per-generation event queue；
- 每次 accepted compile/clean mutation 递增 `desired_derived_revision`；它只用于 supersede/wait/retry identity，不是 publication queue；
- `desired_derived_mode=absent` 用于 clean/purge，不和“从未编译”混淆；
- `current_remote_binding_fingerprint = SHA-256("derived-binding-v1\0" + canonical_remote_name + "\0" + remote_folder_id)`；
- `remote_published_generation_id` 与 `remote_published_binding_fingerprint` 共同构成 worker 在成功 MANIFEST commit 后写入的 durable knowledge；
- 只有 published binding fingerprint 等于 current binding fingerprint 时，published generation 才能满足 current predicate；
- `remote_state` 也只有在 `remote_state_binding_fingerprint == current_remote_binding_fingerprint` 时可用于 current/absent 判定；
- migration cutover 必须先切换 current binding fingerprint，并把新 binding 的 remote publication knowledge / remote state 置为 none/unknown；
- retry target key 使用 versioned encoding `(desired mode, generation-or-empty, desired revision, current binding fingerprint)`；
- 新 target 自动清除旧 target 的 retry/terminal gate；同一 absent desire 下用户重跑 clean 也因 revision 增加而获得新的显式 attempt；
- failure commit 只有在 attempt target 仍等于 current target 时才更新 profile-level retry gate，否则只结束 history row；
- retryable/limited/terminal 的 backoff 与 exit-code mapping 复用现有 framework，必须持久化 `next_retry_at`，禁止 worker busy-loop；
- `compiler status` 不为每次显示去实时查询 Drive。

---

## 40. 不引入旧式 publication intent queue

不要实现旧草案式：

```text
compiler_publication per-generation queue
compiler_generation_gc remote intent queue
ordinary pending_events carrying compiler outputs
```

连续 compile：

```text
B
C
D
```

只把：

```text
desired_derived_generation_id = D
```

作为最终 durable desire。

如果 B/C 尚未开始，可直接跳过；如果某一代已经在上传，允许它完成，下一 worker pass 再收敛到最新 desired。

---

# Part J：Dedicated DerivedSync data lane

## 41. DerivedSync 不走普通 `ArgsFor()`

V1 **不提供**：

```text
rclone.derived_sync_args
```

DerivedPublisher 不继承：

```text
global_args
full_sync_args
fast_upsert_args
dry_run_args
verify_args
unsafe ordinary sync args
```

避免当前或未来普通 sync tuning / correctness flag 污染 derived lane。

共享的仅是 infrastructure：

```text
rclone binary
rclone config / credentials
remote identity
exec wrapper
structured progress plumbing
worker singleton
profile lock
remote lease
logging / sanitized error framework
```

以后如果真实 benchmark 证明需要 derived tuning，只增加少量 typed option，而不是开放 arbitrary rclone flag bag。

---

## 42. 复用 shared execution / attempt framework

不要在 DerivedPublisher 里复制：

```text
exec.CommandContext wrapper
progress JSON parser
worker lease lifecycle
retry classification framework
activity publishing
success/failure persistence shell
```

现有 `internal/exec.Rclone` 继续是 subprocess execution authority。

应抽取/复用更低层 remote operation helpers（具体命名可按实现调整）：

```text
RemoteOps.Sync
RemoteOps.Check
RemoteOps.CopyTo
RemoteOps.Purge
```

普通 Reconciler 与 DerivedPublisher 各自组合这些 primitive。

两条 lane 共享：

```text
operation attempt starts
-> lock / lease / activity / progress
-> operation-specific Execute()
-> shared failure classification / durable commit
```

但 correctness predicate 不相同。

---

## 43. DerivedPublisher 三阶段 publication

目标 generation C：

### Phase 1 — sync detail view

概念 command：

```text
rclone sync \
  <local generations/C/> \
  <remote profile root>/.knowledge-derived/ \
  --exclude /MANIFEST.json
```

Correctness flags 由 DerivedPublisher 代码拥有。

目的：

- 上传 C 的所有 detail artifacts；
- 删除 remote 中不属于当前 generation 的 obsolete detail artifacts；
- 保留旧 remote `MANIFEST.json`，直到最后 commit。

禁止 `--delete-excluded` 破坏旧 MANIFEST commit marker。

### Phase 2 — complete content check

概念 command：

```text
rclone check \
  <local generations/C/> \
  <remote profile root>/.knowledge-derived/ \
  --exclude /MANIFEST.json
```

固定语义：

```text
no --size-only
no --one-way
no --download by default
```

要求 rclone 使用 backend 可用 hash 做内容检查，并同时发现 destination stale extra detail files。

Phase 2 非零：**绝不进入 Phase 3**。

### Phase 3 — commit MANIFEST

```text
rclone copyto \
  local generations/C/MANIFEST.json \
  remote .knowledge-derived/MANIFEST.json
```

只有 Phase 3 成功后，本次 derived operation 才满足 completion predicate。

---

## 44. Remote publication success

Derived publication 成功条件：

```text
derived ownership valid
AND Phase 1 sync success
AND Phase 2 full check success
AND Phase 3 MANIFEST copy success
```

然后 shared attempt framework 才可以 commit：

```text
remote_published_generation_id = C
remote_published_binding_fingerprint = current_remote_binding_fingerprint
remote_state = generation
remote_state_binding_fingerprint = current_remote_binding_fingerprint
derived_state = current only if desired is still generation C; otherwise pending
```

Phase 3 后的 DB commit 必须在 compiler lock 下重新读取 desired state。若上传期间另一条 compile 把 desired 改成 D，或 clean 把 desired 改成 absent，worker 仍可准确记录“remote 现在是 C”，但不能把 derived state 错标为 current；解除 C pin 后下一 pass 继续收敛 D 或 absent。

如果 Phase 1/2/3 任一步失败：

```text
desired state remains the latest durable value (C, newer D, or absent); attempt never overwrites it
remote_published generation/binding pair remains previous known value
attempt failure uses shared retry classification
```

### 44.1 crash after Phase 3 but before DB success commit

不需要每次 status 远程查询修复。

重启后 durable state 仍显示 desired != remote_published，worker 可以幂等地重新执行 C publication；成功后再写 DB。

---

## 45. 允许短暂 remote mixed window

Google Drive/rclone 没有跨多个文件的 directory transaction。

因此 Phase 1 期间可能短暂出现：

```text
old MANIFEST = B
some detail files = C
```

V1 接受这个窗口，不为了 strict transaction 重新暴露 `generations/<uuid>/` remote hierarchy。

Consistency contract：

1. remote `MANIFEST.json` 是 commit marker；
2. JSONL row / aggregate / Markdown header 携带 `compile_generation_id`；
3. full MANIFEST 记录每个 detail artifact hash；manifest 自身由 local root pointer hash 和 remote manifest-last commit 覆盖；
4. consistency-aware consumer 发现 detail generation 与 MANIFEST 不一致时必须 reject/retry，不能跨代拼数据；
5. 一旦 remote MANIFEST 声明 C，worker 在此之前已经成功上传并验证 C details。

远端 presentation simplicity 优先于多目录 transaction。

---

# Part K：Remote namespace ownership

## 46. Derived ownership metadata 不放在 Chat-visible subtree

普通 profile ownership 继续使用现有：

```text
remote:.knowledge-sync/profiles/<profile_uuid>.json
```

Derived namespace 增加：

```text
remote:.knowledge-sync/derived/<profile_uuid>.json
```

而不是：

```text
.knowledge-derived/.knowledge-sync-derived.json
```

这样 `.knowledge-derived/` 只包含 Chat/human 有用的 artifacts。

---

## 47. Derived sidecar schema

至少：

```text
schema_version
profile_id
profile_uuid
remote_name
remote_folder_id
remote_binding_fingerprint
derived_path = ".knowledge-derived"
created_at
```

所有 destructive DerivedSync operation 前必须：

1. 验证普通 profile ownership sidecar；
2. 验证 derived sidecar。

Mismatch / malformed：fail closed。

Derived sidecar 的 `remote_binding_fingerprint` 必须按 §39 的 canonical algorithm 计算，并与当前 profile binding 完全一致。V1 的 marker path 仍按 `<profile_uuid>.json` 固定，因此 `profile migrate` 只支持迁移到不同的 rclone remote alias；同一 alias 内换目录直接拒绝。两个 alias 若实际指向同一 backing account 属于 V1 明确不支持的配置，不能静默覆盖旧 marker。

Remote alias 进入 sidecar / fingerprint 前先 canonicalize：只移除 rclone remote 名尾部可选的一个 `:`，不 lower-case，不改其他 spelling。比较、DB persistence 与 fingerprint 必须使用同一 canonical form。

Sidecar 字段一致还不足以证明 rclone display path 正在指向该 folder。每次 claim，以及每个 destructive publish/purge/migration commit 前，都必须执行 live root binding validation：

```text
resolve <canonical remote>:<remote_display_path>
require exactly one directory match
require live folder id == profile.remote_folder_id
require ordinary sidecar matches the same binding
require derived sidecar matches when namespace is claimed
```

零匹配、多个同名 folder、rename/replacement 后 ID 变化均 fail closed。不能使用“取第一个匹配”的 folder resolver 作为 destructive ownership proof。

Ordinary 与 derived marker claim 必须是 non-overwriting create：marker 已存在时先读取并严格验证；不存在时用 immutable/create-if-absent 语义写入，再 read-back 验证。不同 alias 若实际暴露同一 backing metadata root，会因为已有 marker/binding mismatch 而 fail closed，不能 unconditional `copyto` 覆盖旧 marker。

---

## 48. 第一次 claim

第一次 publication：

```text
remote .knowledge-derived absent
  -> create derived sidecar
  -> create/publish namespace

remote .knowledge-derived empty
  -> create derived sidecar
  -> publish

remote .knowledge-derived non-empty
AND no valid derived sidecar
  -> fail closed
  -> no overwrite
  -> no delete
```

V1 不提供：

```text
--force-adopt
```

Claim 成功后整个 `.knowledge-derived/` 是 compiler-owned exact mirror；用户手工塞入 unknown file，下一次 DerivedSync 会删除它。

`README.md` 必须明确：

> `.knowledge-derived/` is managed by knowledge-sync. Do not place user-authored files here.

---

## 49. Clean 后 ownership 保留

`compiler clean` 的语义是清 artifacts，不是 relinquish namespace。

因此 remote purge 成功后：

```text
.knowledge-derived/ absent/empty
```

但：

```text
.knowledge-sync/derived/<profile_uuid>.json
```

继续保留。

下一次 compile 可以直接重新创建 current view。

---

# Part L：Worker scheduling、retry 与 telemetry

## 50. 同一个 worker，是唯一持续 mirror/derived data-plane owner

不启动：

```text
derived-worker daemon
compiler uploader daemon
```

现有 worker 继续是已有 profile 的 ordinary reconcile / fast / prune、DerivedSync、migration、root-content purge 的唯一持续 data-plane runtime owner。这个不变量覆盖当前仍在 CLI 直接执行 rclone 的 `profile migrate` 与 `purge-remote`：在启用 DerivedSync 前，它们必须改成 durable intent + worker execution；CLI 只负责记录、wake 和 observe。

`profile add` 的 remote-root/sidecar bootstrap 与显式 `probe` 是现有 bounded control/diagnostic exception，不在本计划中强制改成 worker operation；它们不得读取 compiler generation、不得写入/删除 `.knowledge-derived/**`，也不能被当作 ordinary/derived attempt framework 的先例。该 exception 仍必须使用 canonical remote alias、strict unique live display-path-to-folder-ID resolution、resolved source/app-state/other-profile overlap validation，以及 non-overwriting ordinary sidecar claim；不能保留当前“重复目录取第一个”的行为。

Derived operation 复用：

```text
worker singleton
profile lock
remote lease
rclone progress
status socket/activity plumbing
error classification
```

Compiler local scan 本身不在 worker 中执行；`compile` CLI 只做本地 compiler work + 写 desired state + wake worker。

Local-only compiler command 使用 state-only App initialization，不调用 `exec.LookPath("rclone")`、`DiscoverConfigPath`、remote probe 或任何 rclone subprocess。缺少 rclone 只能使 worker 远端 attempt 失败，不能反判已经成功的 local compile 失败。

---

## 51. Scheduler ordering

保留 ordinary lane 当前既有优先级与 destructive-safety 语义，不为 compiler 改写它。

每个 active profile 的 worker pass：

```text
profile deletion lifecycle gate
        |
        v
migration lifecycle gate
  if pending/runnable -> execute migration attempt and end this profile pass
        |
        v
ordinary scheduler
  full/policy/prune/fast 等按现有规则处理一次 eligible work
        |
        v
derived scheduler
  if desired != remote_published
     -> publish or purge attempt
```

此外 worker 每个 pass 有独立的 disabled-profile root-content-purge scheduler，只扫描 durable purge requests；它不把普通 disabled profile 加回 ordinary/derived scheduling。

关键规则：

- ordinary work 优先尝试；
- ordinary failure **不阻止**同一 pass 后续 DerivedSync 尝试；
- derived failure 不把 ordinary profile mirror health 改成 failed；
- profile deletion request 优先级仍最高，deleting/tombstoned profile 不开始新 derived attempt。
- migration claim 后本 pass 不再执行 ordinary/derived work；cutover 完成后下一 pass 按新 binding 收敛；
- remote lease 在 operation 已识别后按该 operation 的 current/target remote 获取，不能在 migration selection 前固定绑定旧 remote。

当前 `runWorkerPass` 在 full-claim 与 no-full 分支中存在 early return。接入 derived scheduler 时必须把 ordinary result 收集后再统一进入 derived check，不能简单把 derived call 追加在现有 closure 尾部。重构必须保留当前 fast-event snapshot + exact-version clearing：不得为了接入 derived lane 改回 broad `ClearPending`。

正常情况下 source files 倾向先于新 derived index 收敛到 Drive；但普通 lane 被 delete budget / policy terminal gate 卡住时，不会永久拖死独立 derived view。

---

## 52. Derived error state 与 ordinary health 分开

允许：

```text
ordinary sync: ready
derived sync: failed
```

也允许：

```text
ordinary sync: failed
derived sync: current
```

`profile status` 可以显示摘要：

```text
compiler derived sync: failed
```

但不得把 derived error 覆盖普通 `profile_sync_state.last_error` 的真实语义。

`compiler status` 是 derived/local compiler 的详细 observer command。

---

## 53. Retry classification

Derived operation 接入现有 shared `classifyError` / retry framework，不复制一份“derived retry system”。

至少保持：

```text
context canceled / timeout -> retryable
transport-level temporary rclone failures -> follow existing retry classification
ownership mismatch / malformed derived sidecar -> terminal/fail-closed
pre-existing unclaimed non-empty derived namespace -> terminal until user resolves
```

具体 rclone exit-code mapping 复用当前 worker 逻辑。

Derived claim 必须同时检查：target key 仍 current、`next_retry_at` 已到期、无 active attempt、profile lifecycle 可运行。Terminal 只阻塞同一个 target key；新 compile/clean revision 或 migration binding cutover 自动形成新 target。Explicit retry 不创建 event queue，只通过 accepted command mutation 增加 desired revision。

---

# Part M：CLI contract

## 54. Compile

```bash
knowledge-sync compile <profile>
```

默认行为：

```text
initialize state-only App (no rclone discovery/probe)
load non-tombstoned, non-deleting profile
acquire compiler-profile lock
resolve committed eligibility snapshot
full deterministic compile
stable-source validation (one retry max)
publish local immutable generation
set desired derived generation = C
wake worker
return
```

成功输出概念上：

```text
Compiled generation C locally.
Derived sync queued.

Check progress:
  knowledge-sync compiler status <profile>
```

默认 **不等待 Drive upload**。

### 54.1 `--wait`

```bash
knowledge-sync compile <profile> --wait
```

先完成相同 local compile，然后等待：

```text
remote_published_generation_id == C
AND remote_published_binding_fingerprint == current_remote_binding_fingerprint
```

如果 profile disabled：

- local compile 仍可成功；
- derived state = `blocked_disabled`；
- `--wait` 不无限等待，应直接返回 actionable blocked state，提示先 enable profile。

`--wait` 不依赖实时 Drive query，只观察 SQLite / worker socket。若等待期间 profile 进入 deletion-requested 或 tombstoned，立即返回 deletion-specific terminal result，不能误报为 `blocked_disabled`。

若等待期间另一条 compile/clean 改变了 desired，使 C 在发布前被 supersede，`--wait` 返回 actionable `superseded`，不能无限等待，也不能把较新的 generation 当作 C 已发布。

---

## 55. Disabled profile

对 non-tombstoned disabled profile：

```text
compile -> allowed
compiler status -> allowed
compiler clean -> allowed
```

但 worker 当前只调度 enabled profile，因此：

```text
derived desired state retained
derived_state = blocked_disabled
```

`profile enable` 后自动继续收敛最新 desired state。

无需为 disabled profile 创建另一个 uploader。

---

## 56. `compiler status`

```bash
knowledge-sync compiler status <profile>
```

默认是轻量 observer：

```text
Local:
  generation
  compiled_at
  source_snapshot
  policy_hash
  eligibility_contract_hash
  eligible files / notes / warning count

Derived:
  desired
  remote_published
  current / published binding fingerprint
  state
  last_success
  last_error
```

默认：

- 使用 state-only App initialization，不要求 rclone 可执行文件或可达 remote；
- 读 SQLite；
- 读 local root pointer / generation MANIFEST；
- 检查必要文件存在；
- **不重新 SHA-256 全部 artifacts**；
- 不实时查询 Drive。

### 56.1 `--verify`

```bash
knowledge-sync compiler status <profile> --verify
```

只验证 local current generation integrity：

```text
generation MANIFEST exists and hash matches local root pointer
every detail artifact exists
detail bytes/hash match immutable generation MANIFEST
```

不把 `--verify` 变成远端 Drive check。

---

## 57. `compiler clean`

```bash
knowledge-sync compiler clean <profile>
```

本地逻辑立即：

```text
initialize state-only App (no rclone discovery/probe)
reject tombstoned or deletion-requested profile
acquire compiler-profile lock
DB transaction:
  set desired_derived_mode = absent
  set local_clean_state = committing
  set local_clean_operation_id
fsync + atomically remove local root pointer (local clean commit)
DB transaction:
  set local_mode = absent
  clear local_clean_state / operation_id
remove unpinned generations and staging best-effort
preserve compiler run/history rows
wake worker
```

DB clean intent 必须先于 pointer removal。Crash repair：

```text
clean_state=committing + pointer exists
  -> finish pointer removal

clean_state=committing + pointer absent
  -> repair local_mode=absent and clear committing

pointer absent + no committing marker
  -> treat as corruption unless profile has accepted local_mode=absent
```

若 pointer removal 已成功，后续 generation/staging 删除失败只形成 local GC debt，不把 local current 恢复，也不反判 clean commit 失败。新的 compile 遇到 `local_clean_state=committing` 时必须先完成 clean recovery，再开始新 generation。

如果有 in-flight pinned generation，物理目录允许暂时保留到 attempt 结束；从用户语义上 local current 已经 absent。

默认返回：

```text
Compiler artifacts cleaned locally.
Remote derived cleanup queued.
```

Worker 后续：

```text
validate profile ownership
if valid derived sidecar exists:
  validate derived ownership
  purge remote <profile root>/.knowledge-derived/
  mark remote derived state absent for current binding
else if derived sidecar missing AND namespace absent/empty:
  perform no destructive action
  record remote_state=absent_unclaimed for current binding
else:
  fail closed
```

因此 clean before first compile、clean after local compile but before first remote publish，以及 migration 后 desired 已经 absent 都能收敛；missing sidecar 只有和 live namespace absent/empty 证明同时成立时才表示安全 absence，不能单独当作成功。

不使用 ordinary `max_delete`，不走普通 prune authorization，因为该 subtree 已是 compiler-owned exact mirror。

### 57.1 `clean --wait`

等待：

```text
remote_state IN (absent, absent_unclaimed)
AND remote_state_binding_fingerprint == current_remote_binding_fingerprint
```

若等待期间新的 compile 把 desired 从 absent 改回 generation，返回 actionable `superseded`。

Disabled profile 下不无限等待，返回 `blocked_disabled` 并保留 desired absent，enable 后继续 purge。

### 57.2 compiler lock 与 lifecycle mutation

`compile` / `compiler clean` 持有完整 local operation 的 compiler lock。`profile ignore update`、disable/enable、remove/restore/forget、migrate cutover 等在现有 profile lock 内再取得同一 compiler lock，并按 §11.2 顺序执行。所有 mutation 在写 pointer / SQLite 前必须复查 profile UUID 与 lifecycle state。

---

# Part N：Profile lifecycle

## 58. `profile disable / enable`

Disable：

```text
stop normal jobs
keep local compiler generations
keep remote derived view
keep derived sidecar
keep desired state
DerivedSync blocked
```

Disable 不会丢弃 compiler generations、derived view 或 sidecar。尚未 claim 的 migration 进入 `blocked_disabled`，已有 desired derived state 保留；pending root-content purge 必须先完成或被显式取消，不能因 disable 自动获得 root-content delete 权限。

Enable：

```text
existing worker scheduling resumes
latest desired derived state converges automatically
```

如果存在 pending root-content purge，enable 必须拒绝并提示先完成或取消 purge；否则 enable 后恢复 ordinary + derived scheduler，并在 root-content purge 成功后把 ordinary mirror 标记为需要 full reconcile。

---

## 59. `profile remove / restore`

对齐现有“remove 保留 remote data”的语义。

### remove

```text
preserve local compiler generations/history
preserve remote .knowledge-derived
preserve remote derived sidecar
stop new DerivedSync attempts
```

`remove` 在写 `deletion_requested_at` 前必须取得 compiler lock；已经开始的 local compile 先完成，随后 deletion request 串行生效。进入 deletion-requested 后不允许新的 compile/clean mutation。

Deletion-requested profile 同时拒绝新的 policy commit、prune preview、`prune discover`、prune authorization、migration、root-content purge 与其他 ordinary mutation。Deletion finalization 必须按 profile lock -> compiler lock 执行，并在 tombstone/row deletion transaction 中将尚未 claim 的 migration、purge、prune request 标为 canceled/stale；running operation 由 profile lock 保证先结束。worker recovery/finalization 不能只沿用 enabled-profile 列表。

### restore

```text
restore compiler state visibility
enable 后继续收敛 existing desired state
```

`restore` 在 profile lock -> compiler lock 下恢复 visibility；只有显式 enable 后 worker 才恢复 ordinary/derived scheduling。

Restore 必须重新执行 source root 是非 symlink、source/app-state 不重叠、与所有 non-forgotten profile source 不重叠，以及 remote binding/sidecar 可验证等检查；不能因为 profile 曾经有效就跳过 overlap 和 ownership validation。

不因为 tombstone 自动清 remote derived data。

---

## 60. `profile forget`

Forget 是 permanent local identity deletion。

应删除：

```text
local compiler/<profile_uuid>/
compiler_runs for profile
compiler_profile_state
other compiler operational rows
```

Forget 必须在 profile lock -> compiler lock 下执行，并使用独立的 two-phase local marker：

```text
DB transaction: set forget_state=committing + operation id
remove local compiler root
DB transaction: delete compiler rows and profile identity
```

若本地删除失败，保留 tombstoned profile 和 DB marker/error 并返回错误；若 crash 或 DB failure 发生在两步之间，recovery 根据 `forget_state` 检查/重试 root removal，成功后再完成 identity deletion。不能先删除 profile row，也不能留下无 owner 的 generation 目录。

但不访问、不删除 remote：

```text
old remote .knowledge-derived remains
old remote .knowledge-sync/derived/<profile_uuid>.json remains
```

这与普通 profile remote data “forget 不做 remote destructive cleanup”保持一致，也避免留下“内容还在但 sidecar 被单独删掉”的 unowned subtree。

---

## 61. `profile migrate`

当前实现仍由 CLI 在 profile lock 内直接执行 rclone copy/verify，并在持锁时等待 worker reconciliation；这与 single-worker data-plane owner 冲突，也会阻塞 worker 获取同一个 profile lock。V1 必须先把 migration 改成 durable worker-owned operation：

```text
CLI validates request locally
  -> persist migration intent
  -> wake worker
  -> return after durable queueing by default, without holding profile/compiler lock
  -> optional --wait observes the durable terminal result

worker
   -> profile lock + remote lease
   -> validate source root and canonical target remote/path before any target mutation
   -> if target root exists, resolve it to exactly one live folder ID;
      if absent, resolve its parent strictly, create it once, then read back
      exactly one live folder ID
   -> reject a non-empty target root without a matching same-profile binding;
      never adopt unrelated remote content
   -> copy only strict shared eligible ordinary files
   -> never copy .knowledge-derived from source
  -> verify
  -> write/validate ownership for new binding
  -> atomic DB cutover
  -> queue ordinary reconcile + DerivedSync convergence
```

Request gate：profile 必须 non-tombstoned、non-deleting、enabled，且没有 active/pending root-content purge 或另一条 migration。Disable 使未 claim migration 进入 blocked 状态，enable 后恢复；remove/forget 必须取消尚未 claim 的 migration，running migration 因持有 profile lock 先完成后 remove 才能继续。

`profile migrate <id> <new-remote> <new-remote-path>` 默认只提交 intent 并返回；`--wait` 等待 `succeeded` / `failed` / `blocked` / `superseded`，遇到 deletion-requested、binding mismatch 或 ownership failure 返回 actionable terminal state。waiter 不持有 profile/compiler lock，也不在 CLI 中执行 copy/verify。

Migration 访问不同 remote，worker 不能在 operation selection 前无条件获取“当前 remote”的 lease。必须先在 profile lock 下识别 operation，再按目标获取 lease：old binding ownership read/validation 与 new target mutation 不同时持有两个 remote lease；target root create/copy/verify/sidecar claim/cutover 全程持有 target remote lease。若实现确需双 lease，必须按 canonical remote name 全局排序获取，禁止 deadlock。

V1 只接受 `new_remote != current remote_name`。同一 remote alias 内迁移直接拒绝，因为 ordinary/derived sidecar 都以 profile UUID 为固定 marker key，覆盖会破坏“旧 remote root + old sidecar 保留”语义。

目标 ordinary root 的创建/复用也必须 fail closed：目标路径不存在时，只能在严格解析且 ownership-safe 的父目录下创建；目标路径已存在时，必须解析到唯一 live folder，并且该 root 为空或已有同一 profile、同一新 binding 的可恢复 migration marker。非空且无匹配 owner 的目标 root 一律拒绝，不能把现有 remote 内容与本 profile 合并。

Migration 切换到新的 remote binding，并保留旧 remote root。

Compiler 行为：

```text
local current generation C 保持不变
old remote derived view + old derived sidecar 保留
DB remote binding cutover
current_remote_binding_fingerprint switches to new binding
remote_published generation/binding for new binding = none/unknown
remote_state/binding for new binding = unknown
desired generation remains C
queue DerivedSync to new binding
```

DerivedPublisher 自己在新 remote：

```text
validate new profile ownership
claim new derived namespace
publish C
```

不要让 ordinary migrate 特判“复制 compiler files”；compiler source 已经不在 Vault，重新 publish current generation 更简单、更可验证。

Migration 的 durable state 至少记录 source binding、target binding、target folder ID、当前 phase 与 operation ID。崩溃语义固定为：

```text
target root/copy/verify/marker 完成、DB cutover 尚未提交
  -> current binding 仍是 old；恢复时只能验证并继续同一 target operation，不能删除或静默接管

DB cutover 已提交、DerivedSync intent 尚未提交
  -> new binding 已是 current；恢复时补写 desired derived state 并重新发布 current generation

target root ownership/copy/verify 失败
  -> old binding 保持 current；target remains an uncommitted migration artifact
     and is never adopted by ordinary sync
```

`profile remove` / `forget` / retry recovery 必须能按上述 phase 处理 pending、running 和 orphaned migration；不能只依据 profile 当前是否 enabled 来决定是否恢复。

### 61.1 `purge-remote`

现有 `purge-remote` 在 CLI 中直接对整个 profile root 执行 rclone delete，因此会连同 `.knowledge-derived` 内容一起删除，且没有 worker lease/state commit。V1 把该命令重新定义为 durable worker-owned **root-content purge**：清空 managed profile root 内的内容和空子目录，但保留 profile root directory 本身、`remote_folder_id`、ordinary/derived sidecars 与 binding fingerprint。

删除 profile root directory 本身不属于 V1；那会使 folder ID 和全部 ownership/publication knowledge 失效。如需永久移除 folder object，只能在 forget 后由用户另行处理。

Root-content purge 固定以下 gate：

```text
profile is non-tombstoned and disabled
ordinary ownership valid
desired_derived_mode == absent
remote_state IN (absent, absent_unclaimed)
remote_state_binding_fingerprint == current_remote_binding_fingerprint
.knowledge-derived absent/empty
no active ordinary / derived / migration attempt
```

因为 disabled profile 的 DerivedSync 会进入 `blocked_disabled`，用户必须先在 profile enabled 时完成 derived cleanup，再 disable 后请求 root-content purge：

```bash
knowledge-sync compiler clean <profile> --wait
knowledge-sync profile disable <profile>
knowledge-sync purge-remote <profile> --confirm
```

未 claim、non-empty 的 `.knowledge-derived` 同样 fail closed，要求用户先手工移走或清空。这样 root-content purge 永远不成为 DerivedPublisher 之外的 derived subtree deleter。

从未创建 `compiler_profile_state` 的旧 profile 可走明确的 `never_claimed_absent` 分支：只有 derived sidecar 不存在且 `.knowledge-derived` absent/empty 时，root-content purge gate 才把 derived 视为 absent；不得把“state row 缺失”本身当作 absent 证明。

Worker 当前 ordinary/derived profile loop 只选择 enabled profiles，因此 root-content purge request 必须有独立的 durable scheduler，可选择“non-tombstoned + disabled + purge pending”的 profile；它仍复用 worker singleton、profile lock 和 remote lease，不能临时绕回 CLI data plane。

Root-content purge CLI 同样先提交 durable request，再等待 worker terminal result，等待期间不持 profile/compiler lock。Pending purge 存在时 `profile enable` 必须拒绝；`profile remove` 在 deletion intent commit 中取消尚未 claim 的 purge，保证 remove 的“保留 remote data”语义不会被旧 request 事后破坏。Running purge 持有 profile lock，完成后 lifecycle mutation 才能继续。

成功后：

```text
profile remains disabled
remote_folder_id / binding fingerprint unchanged
ownership sidecars remain
ordinary mirror state becomes uninitialized/dirty for next enable
next enable queues a full ordinary reconcile
derived remote_state remains absent for the same binding
```

---

# Part O：Crash / failure semantics

## 62. Local compile failure

以下任何 root-pointer commit 前 hard failure：

```text
eligible file unreadable
parser fatal error
serialization failure
fsync/rename failure
full generation manifest failure
root pointer commit failure
source unstable after retry
```

必须保证：

```text
old local current generation remains authoritative
new generation not selected
run recorded failed/interrupted as appropriate
```

Root pointer 已成功替换后的 SQLite failure 不属于上述 failed compile；按 §9 记录/返回 `local_published_state_pending_repair`，并由 pointer 驱动 repair。

Malformed per-file frontmatter 等已定义 nonfatal 情况进入 WARNINGS，不扩大成 whole compile failure。

---

## 63. Crash repair boundaries

至少覆盖：

```text
crash before generation directory atomic publish
crash after generation directory publish but before root pointer
crash after root pointer but before SQLite run success
crash during local GC
crash after clean intent before pointer removal
crash after clean pointer removal before SQLite clean completion
crash after migration target root/copy/verify/marker but before DB cutover
crash after migration DB cutover but before derived intent repair
crash during root-content purge before purge-state commit
crash during Derived Phase 1
crash during Derived Phase 2
crash after remote MANIFEST write but before DB remote success commit
```

Repair 原则：

- local root pointer + immutable manifest 是 local publication authority；
- `local_clean_state=committing` 是 clean recovery authority，intent 先于 pointer removal；
- remote MANIFEST commit + idempotent republish 处理 ambiguous remote crash；
- SQLite stale running state 可从 immutable facts / retry 逻辑修复；
- GC 永远不是成功 publication 的反向 transaction。

---

# Part P：Performance requirements

## 64. 20k+ corpus target

V1 correctness benchmark 至少覆盖 20,000+ files 的本地知识库。

Compiler 本地阶段性能主要来自：

```text
eligibility traversal
Markdown reads + SHA-256
Goldmark parse/extensions
YAML frontmatter parse
resolver
edge/node aggregation
JSONL serialization
local fsync/publication
```

Compiler local stage 不调用：

```text
rclone
Google Drive
LLM
embedding
```

DerivedSync benchmark 与 compiler benchmark 分开记录。

---

## 65. Memory / streaming invariants

1. 不把所有 Markdown body 长期保存在内存；
2. 单文件 parse 后只保留 graph/index 所需 metadata；
3. JSONL 尽量流式 serialization；
4. graph memory 与 node/edge metadata 规模相关，而不是 source byte 总量的重复 copy；
5. large attachment 不读内容，仅 metadata；
6. 不在 V1 为“可能更快”先做 persistent parse cache。

Benchmark 记录：

```text
eligible traversal duration
Markdown hashing duration
parse duration
resolver/graph build duration
serialization duration
publication duration
peak RSS
artifact sizes / row counts
```

不预设拍脑袋 latency SLO。

---

# Part Q：Test strategy

## 66. Parser characterization fixtures

必须有 exact expected parser facts / compiler records，至少覆盖：

```text
no frontmatter
valid YAML frontmatter
malformed complete frontmatter
unmatched opening ---
Unicode/custom YAML keys
frontmatter tags scalar
frontmatter tags sequence
frontmatter mapping/mixed warning
aliases scalar/sequence
inline tags
fenced code fake tags/links
inline code fake tags/links
wikilinks
wikilink alias
escaped \| in Markdown table wikilink
heading fragment
block fragment
note embed
attachment embed
Markdown relative links
Markdown <...> destinations
URL encoded local path
+ in path
external URL
Unicode filenames
spaces
case differences
.md / .MD note-kind classification
same basename in different directories
ambiguous basename
missing target
symlink
```

关键测试原则：

> fixture 的 syntax fact 先由第三方 parser/extension 产生；compiler 测试 resolver/aggregation，不通过自己扫 raw Markdown 造 expected parser behavior。

候选 dependency 只有在 mandatory parser fixtures 全部通过后才可进入生产 parser adapter；任何 mandatory fixture fail 都是 dependency rejection，不是“先实现、以后兼容”。

---

## 67. Resolver / graph fixtures

小型 hand-verifiable corpus：

```text
hard orphan
self-link-only hard orphan
no-backlink outbound note
mutual links
repeated A->B edge occurrences
note embed connectivity
attachment embed non-connectivity
unresolved local target
ambiguous target with sorted candidates
excluded candidate must not resolve
fragment retained but not validated
```

Exact compare：

```text
edge rows
unique neighbor counts
edge counts
orphan classifications
broken-link classifications
```

---

## 68. Security / eligibility fixtures

例如：

```text
Source/
  Public.md
  Private/Secret.md   // excluded by committed policy
```

断言：

- compiler 不打开 Secret；
- 所有 artifacts 不包含 private path/name/tag/frontmatter/hash；
- resolver 不把 Secret 当 candidate；
- missing policy row 与 valid empty committed policy 可区分，前者 fail closed；
- profile creation 不存在 enabled profile 可见但 initial policy 未提交的窗口；
- policy change 后 `policy_hash` 改变；
- max_file_size / reservation contract change 后 `eligibility_contract_hash` 改变；
- active local corpus 与 ordinary eligibility helper 一致。
- `.knowledge-derived/**` 即使被 `.gitignore` negation 也不被打开或泄漏；
- eligible regular-file metadata 读取失败是 hard failure；excluded/protected subtree 不读取；
- full traversal 与 fast single-path classifier 对 symlink / non-regular / oversize / protected path 结论一致。
- source root symlink、source/app-state overlap、active/tombstoned profile overlap 均被拒绝；

---

## 69. Source-stability tests

覆盖：

```text
Markdown content changes during compile
membership add/remove during compile
Markdown metadata-only size/mtime_unix_nano changes
non-Markdown size/mtime_unix_nano changes
policy hash changes
eligibility contract hash changes
```

预期：

```text
first instability -> one automatic retry, same run ID
second instability -> source_unstable
old current pointer unchanged
```

---

## 70. Local publication / crash tests

Fault injection：

```text
artifact write
artifact fsync
full manifest write
staging rename
root pointer replace
SQLite success commit
GC delete
clean intent commit
clean pointer removal
clean SQLite completion
```

验证：

- root pointer 永不指 partial generation；
- old current 保留；
- manifest-before-DB crash 可 repair；
- GC failure 不反判 compile；
- current + previous retention 收敛；
- in-flight pin 防止 generation 被删除。
- full generation MANIFEST inventory 不包含 MANIFEST 自身；root pointer hash 覆盖 generation MANIFEST。
- pointer commit 后 SQLite failure 被 repair 为 published success，不回滚 pointer；
- clean 每个 crash point 都由 committing marker 恢复，不出现“desired generation 仍指向已删除目录”。

---

## 71. Derived ownership tests

使用 local/file remote 或 test remote：

```text
namespace absent -> claim succeeds
namespace empty -> claim succeeds
namespace non-empty + no derived sidecar -> fail closed
valid derived sidecar -> publish
UUID mismatch -> fail closed
folder id mismatch -> fail closed
remote binding fingerprint mismatch -> fail closed
display path rename/replacement -> live folder-id mismatch -> fail closed
duplicate same-name root folders -> fail closed, never choose first
malformed sidecar -> fail closed
existing marker claim is non-overwriting
two aliases exposing the same backing marker -> mismatch, no overwrite
clean preserves derived sidecar
same-remote-alias migration -> rejected
non-empty unowned migration target root -> fail closed
missing migration target root -> strict parent resolution + one create + read-back
crash before migration cutover -> old binding remains current
crash after migration cutover -> new binding is repaired and derived republish is queued
clean before first publish + no sidecar + absent/empty namespace -> binding-scoped absent
clean + missing sidecar + non-empty namespace -> fail closed
clean after local compile before first publish + empty namespace -> absent_unclaimed
live display path resolves zero/multiple folders -> fail closed
profile add/restore bootstrap duplicate folder -> fail closed
```

---

## 72. Derived data-plane integration tests

默认 CI 不依赖真实 Google Drive。

用 rclone local/file remote 跑完整三阶段：

```text
Phase 1 detail sync
Phase 2 hash-level check
Phase 3 MANIFEST last
```

至少验证：

```text
obsolete detail file removed
old MANIFEST preserved during Phase 1
Phase 1 failure -> no manifest commit
Phase 2 failure -> no manifest commit
Phase 3 failure -> remote_published not advanced
success -> remote_published generation + binding pair advanced
in-flight C succeeds after desired becomes D -> remote C recorded, state pending
in-flight C succeeds after desired becomes absent -> remote C recorded, purge remains pending
crash after Phase 3 before DB -> idempotent retry converges
rapid B/C/D compile -> latest desired converges
in-flight older generation pin
clean -> desired absent -> remote purge
disabled -> blocked_disabled
enable -> resumes
prune suppressed-managed target and ignored-unmanaged target remain distinct
prune discover rejects reserved/malformed candidates before persistence
prune batch size <= 512 with transactional checkpoint after each batch
partial prune batch retry reclassifies absent targets as missing and never deletes active ownership
```

---

## 73. Ordinary lane protection tests

必须证明普通 sync 不会碰 derived namespace，尤其要覆盖 destructive path：

```text
ordinary full reconcile
policy change
max-delete path
prune path
fast upsert
sync-now catch-up
watcher startup catch-up
pre-upgrade frozen prune target
malformed persisted target (. / .. / absolute / backslash)
worker-owned migration copy
root-content purge gate
```

`sync-now`、watcher startup catch-up、manifest diff 等现有调用点必须只投影 ordinary `active` rows；`protected` row 不得被当作“本地消失”反复制造 destructive reconciliation debt。

这些 catch-up / fast paths 必须直接消费 shared eligibility API 的 metadata view 和 single-path classifier：禁止重新使用 second-resolution `os.Stat`、禁止对 eligible metadata error 静默 `continue`，并与 full compiler 一样拒绝 symlink、non-regular、oversize、reserved target。普通 manifest projection 必须明确调用 `ManifestAllState(profile, active)` 或等价的 active-only API；不得在新增 `protected` 状态后继续调用返回全部 state 的 `ManifestAll` 参与 deletion diff。

Remote `.knowledge-derived/**` 不得：

```text
upload from source
modify
delete
```

该测试是 system-reserved namespace 的 safety gate。

---

## 74. Lifecycle tests

覆盖：

```text
compile while disabled
compile --wait while disabled -> actionable blocked state
clean while disabled
clean --wait while disabled -> actionable blocked state
enable resumes latest desired
remove preserves local/remote derived data
restore resumes
forget deletes local compiler state but leaves remote
migrate re-publishes current generation to new remote
compile/status/clean local path works with no rclone executable/config
deletion-requested during wait -> deletion-specific terminal result
migrate is worker-owned and does not wait while holding the profile lock
purge-remote refuses until compiler clean --wait reaches remote absent
disable blocks pending migration; enable resumes it
remove cancels unclaimed migration/purge intents
migration recovery preserves old binding before cutover and repairs new binding after cutover
enable rejects pending purge
root-content purge preserves folder ID; next enable queues full ordinary reconcile
restore revalidates source/app-state/profile overlap
derived retry backoff persists and a new desired revision supersedes old terminal/retry debt
```

---

## 75. Determinism / schema golden tests

固定 corpus 连续 compile 多次：

- UUID/timestamp 可不同；
- normalized structural rows、ordering、counts、resolution outcomes 必须一致；
- JSONL stable sort；
- full manifest detail-artifact count/hash 与实际文件匹配，且不包含 MANIFEST 自身；
- local root pointer schema 与 remote full MANIFEST schema 明确分离。

Schema behavior change 必须：

```text
schema_version bump
or explicit compiler_version compatibility rule
```

不得 silent semantic change。

---

## 76. Scale fixture

生成 20k+ synthetic corpus，包含 realistic：

```text
folder depth
link density
duplicate basenames
frontmatter/tag distribution
attachments
broken links
Unicode paths
```

记录 performance，但 synthetic corpus 不替代真实知识库验证。

---

## 77. Release smoke：真实 Drive + ChatGPT

默认 CI 不连接用户 Google Drive。

将“核心 CI”和“真实 Drive canary”明确分开：

```text
core CI
  -> go test / build / synthetic local tests
  -> no real Drive credential or remote dependency

optional/repository-gated Drive canary
  -> explicitly configured real remote smoke
  -> failure is reported as remote canary failure, not a reason to make core CI
     require a live account
```

当前仓库已有独立的 Drive canary workflow；Knowledge Compiler 接入后，该 canary 才扩展为 compiler-specific smoke，不得把 real-Drive steps 偷塞进默认 test target。

Release acceptance 至少一次真实 smoke：

```text
knowledge-sync compile <profile> --wait
knowledge-sync compiler clean <profile> --wait
```

检查 Drive：

```text
<profile>/.knowledge-derived/
  README.md
  MANIFEST.json
  KNOWLEDGE_HEALTH.md
  FILE_INDEX.jsonl
  LINKS.jsonl
  ...
```

检查 control metadata：

```text
remote:.knowledge-sync/derived/<profile_uuid>.json
```

检查：

```text
remote MANIFEST generation == compiler status remote_published
remote MANIFEST binding == compiler status remote_published_binding
artifact hashes consistent
remote view has no generations/<uuid>/ hierarchy
```

再做 Chat smoke，例如：

```text
读取 .knowledge-derived/README.md，
根据 KNOWLEDGE_HEALTH.md、ORPHANS.md、BROKEN_LINKS.md
说明当前知识库结构问题。
```

验收重点：用户/ChatGPT 不需要知道 local generation UUID directory 或 compiler internal state。

Canary 还必须覆盖：

```text
derived sidecar claim/validation
manifest-last publication
clean before first publish and after publish
flat current view after a second generation
binding mismatch / duplicate-folder fail-closed behavior
```

---

# Part R：实施结构与代码改动建议

## 78. 新增 compiler package

先新增 compiler 与 ordinary 共用、且不依赖 sync package 的 leaf packages：

```text
internal/namespace/
  derived.go         // system-reserved path predicate

internal/source/
  eligibility.go     // strict traversal + single-path classification
```

再新增：

```text
internal/compiler/
  compiler.go          // orchestration
  corpus.go            // source eligibility adapter + snapshot
  parser.go            // Goldmark stack wiring
  frontmatter.go       // YAML metadata adapter
  resolver.go          // project-owned deterministic resolver
  graph.go             // counts/classifications
  schema.go            // stable artifact record types
  artifacts.go         // serializers
  generation.go        // immutable local generation store
  status.go            // local integrity/status helpers
```

具体拆分可以按代码规模调整，但必须保持：

```text
parser syntax extraction
resolver
aggregation
serialization
```

边界清晰，避免一个大文件混合所有语义。

---

## 79. Shared eligibility refactor

普通 sync 当前 `ScanActiveEntries` / fast-event eligibility 逻辑必须提取成 compiler 可复用的 authoritative helper。

目标：

```text
ordinary scan
ordinary fast single-path check
compiler corpus scan
```

调用同一 eligibility policy implementation。

需要修复任何“full / fast / compiler 对 committed policy、regular file、Oversize、Symlink、system reservation 判定不同”的潜在分叉；compiler 不允许通过 copy-paste 保持“看起来一样”。

实现约束：

```text
Lstat, never follow symlinks
non-regular files are ineligible
eligible-path metadata errors are fatal
excluded/protected subtrees are skipped before metadata/content reads
ActiveEntry carries size + mtime_unix_nano
all results sorted by canonical rel_path
```

在 `internal/sync` 保留薄 wrapper 可以降低调用方改动，但 eligibility implementation 的唯一 owner 必须是 `internal/source`。同一个 package 还提供 `ValidateCanonicalRelativeTarget`：在任何 trim/replace/clean 之前拒绝 malformed target，并由 prune discovery、state persistence、files-from construction、delete batch 和 ordinary targeted deletion 共同调用。Source root 本身在 profile add/restore/compile 前用 `Lstat` 验证为非 symlink directory，并按 resolved path 检查 app-state / all non-forgotten profile overlap。

---

## 80. Paths

扩展现有 `internal/paths`：

```text
CompilerRoot(profileUUID)
CompilerGenerationsDir(profileUUID)
CompilerStagingDir(profileUUID)
CompilerLockName(profileUUID)
```

基于现有：

```text
~/.local/share/knowledge-sync
```

不引入新的用户可配置 output path。

---

## 81. State / migration

新增 additive migration 和 state helpers：

```text
compiler_runs
compiler_derived_runs
compiler_profile_state
manifest protected state
durable profile migration intent/state
durable root-content purge intent/state
prune request kind (suppressed-managed | ignored-unmanaged)
prune batch checkpoint state
```

如果为了 shared attempt telemetry 需要 derived attempt history，可增加 compiler-specific/additive run table；不要为了复用现有 integer source-generation 字段而硬塞进普通 `sync_runs`。

真正要复用的是 worker execution/attempt framework，不是强迫两种 operation 共用不兼容的数据模型。

Migration 必须向后兼容已有 DB；旧 profile 默认没有 compiler state。已有 `.knowledge-derived` manifest paths 必须原子转换为 `protected`，不能先落入 suppressed/prune candidate。Compiler state 必须记录 current/published remote binding fingerprint。

同一 migration 必须 stale 所有含 reserved `prune_targets` 的非终态 request。`compiler_profile_state` schema 必须覆盖 local clean committing marker、desired revision、binding-scoped remote state 与 retry/backoff fields，不能把这些只留在内存。

---

## 82. CLI

新增/修改：

```text
internal/cli/compiler.go
root command registration
```

实现：

```text
compile <profile> [--wait]
compiler status <profile> [--verify]
compiler clean <profile> [--wait]
profile migrate <id> <new-remote> <new-remote-path> [--wait]
purge-remote <profile> --confirm [--wait]
```

拆分 App initialization：

```text
NewLocalApp / equivalent
  -> paths + DB + app config + locks + local status dependencies
  -> no rclone lookup/config discovery/probe

NewApp / remote extension
  -> builds on local dependencies
  -> rclone + Remote + Sync + Reconciler
```

`compile` / `compiler status` / local `compiler clean` 使用 local constructor；`--wait` 只观察 DB / worker socket。Read-only status 不应因为 rclone 缺失或 derived remote 暂时不可达而失效。

Local constructor 的使用范围还包括所有不执行远端 data-plane mutation 的命令：

```text
profile show/list/status
profile wait
sync-now / reconcile-now intent submission
profile ignore update
profile disable/enable/remove/restore/forget
compiler compile/status/clean
prune preview/authorize/status
```

只有显式需要远端读写的操作才使用 remote extension：profile add bootstrap、prune discover 的远端 listing、verify、duplicate inspection、probe，以及 worker 自身。Migration/purge 的 CLI 仍只提交 intent；copy/verify/delete 全部在 worker 中执行。

---

## 83. Derived ownership helper

沿用 `internal/sidecar` 的整体 pattern，新增 derived metadata helper，例如：

```text
DerivedSidecarPath(profileUUID)
ReadDerived
WriteDerived
ValidateDerived
ClaimDerived
CanonicalRemoteName
ResolveFolderIDStrict
ValidateLiveBinding
ValidateCanonicalRelativeTarget
```

普通 profile ownership validation 仍先执行。

不要把 derived marker 写进 `.knowledge-derived/`。
`ClaimDerived` 和 ordinary bootstrap marker claim 都必须 non-overwriting + read-back validated；`ResolveFolderIDStrict` 必须对每个 path component 要求唯一目录匹配，不能沿用当前 resolver 的 first-match 行为。

---

## 84. Derived publisher

推荐独立 package 或明确模块：

```text
internal/derived/
  publisher.go
  remoteops.go / reuse shared remoteops
```

职责：

```text
validate/claim ownership
pin generation
Phase 1 sync
Phase 2 check
Phase 3 manifest commit
purge for desired absent
```

它不负责 source scan / Markdown parse。

---

## 85. Worker refactor

`internal/cli/worker.go` 保持唯一 data-plane owner。

需要：

1. 将已有 common attempt shell 抽到可供 ordinary/derived 复用的 helper；
2. 保持 ordinary claim/retry/deletion priority 不变；
3. ordinary attempt/batch 后检查 derived desired debt；
4. derived activity 接入现有 live telemetry；
5. derived completion 写 compiler state，不覆盖 ordinary sync state；
6. recovery 清理 stale active pin / interrupted derived attempt，并让 level-triggered desired state重新 claim。
7. 消除 ordinary 分支 early return 对 derived scheduler 的遮挡，同时保留 fast-event exact-version clearing；
8. 把 `profile migrate` 与 `purge-remote` 改为 durable worker-owned operation，不允许 CLI 直接执行 rclone。
9. 恢复 stale running attempts / pins 时扫描全部 compiler identities，包括 disabled、deleting、tombstoned profiles；
10. 派生 retry gate 按 `(desired mode, generation, desired revision, binding)` 持久化，不能 busy-loop。

不要复制出第二套：

```text
lease renewal
progress checkpoint
live socket publish
rclone error wrapping
retry scheduler
```

---

## 86. Ordinary system-reserved protection

将 `.knowledge-derived/**` 加入 ordinary lane 的 system protected namespace。

这不是 user exclude rule；不要允许用户 negate。

实现必须与当前 exclusion/prune architecture 对齐：

```text
app owns desired/protected/delete decisions
rclone remains transport
```

当前 ordinary reconcile 已采用 files-from active set + separately proven targeted deletes，而不是 `--delete-excluded`。实现应利用这一结构并在每层补 hard guard：

```text
shared source eligibility
manifest protected state
disappearance classifier
prune target creation/execution
DeleteRemotePaths final check
fast event exact consume without upload
migration files-from
root-content purge precondition
prune discover remote listing
batched `rclone delete --files-from-raw` final guard
```

任何一层发现 `.knowledge-derived` target 都必须 fail closed 或安全 no-op，不能依赖“上游应该已经过滤”。

---

## 87. Dependency addition

`go.mod` 当前只包含已有 CLI/SQLite dependencies；compiler implementation 增加 parser direct dependencies。

要求：

- 精确版本 pin；
- mandatory parser characterization 全通过后才接受 dependency 组合；
- parser behavior fixture 锁定；
- 不引入 Node/CGO parser runtime；
- 不引入 model SDK。

---

# Part S：推荐实施顺序

## 88. Phase 0 — current architecture hardening

先修复 compiler 依赖的现有边界，不写 parser / graph：

1. 抽取 `internal/source` strict authoritative eligibility helper；
2. 增加 committed policy bundle API，原子创建 profile + initial policy，missing policy fail closed；
3. 加强 source-root validation，拒绝 symlink root、app-state overlap 与 tombstoned-profile overlap；
4. 增加 fast single-path classifier，保持 exact-version event clearing；
5. 增加 `internal/namespace` 与 `.knowledge-derived` hard reservation；
6. additive migration 将历史 ordinary ledger row 转为 `protected`，并 stale 已冻结 reserved prune targets；
7. 在 scan / catch-up / disappearance / prune / canonical target / delete execution 加最终保护；
8. 拆分 state-only App initialization；
9. 增加 compiler paths / blocking-wait lock wrapper / lifecycle lock ordering；
10. 把 profile migrate 改成 durable worker-owned operation；
11. 把 purge-remote 改成 preserve-root-binding 的 durable worker-owned root-content purge；
12. 增加 canonical remote alias + strict live display-path-to-folder-ID validator 与 non-overwriting marker claim。

Gate：普通 sync 全量 regression tests 通过；profile 不存在 runnable-without-policy 窗口；metadata-aware active fingerprint 行为不回退；derived path protection 覆盖 full / fast / catch-up / policy / frozen prune / malformed target / delete destructive paths；local App 在无 rclone 环境可打开 DB 并执行只读路径；migrate/purge CLI 已不再直接执行 rclone。

---

## 89. Phase 1 — parser characterization gate

实现 candidate parser spike 与 exact fixtures，不生成正式 artifacts：

```text
pinned candidate dependencies
frontmatter characterization
wikilink/embed characterization
escaped table-pipe characterization
tag/code-region characterization
.md/.MD note-kind contract
```

Gate：全部 mandatory parser fixtures 通过。任一 mandatory fixture fail 就拒绝该 dependency 组合；不得开始 resolver/graph，更不得增加 raw-text fallback lexer。

---

## 90. Phase 2 — standalone deterministic compiler

实现：

```text
full corpus scan
stable snapshot with policy + eligibility hashes
accepted parser adapters
resolver
graph aggregation
artifact schemas
local immutable generation publication
root pointer and manifest-self exclusion
local retention
compiler_runs / profile state
state-only compile/status/clean --verify
```

这个阶段可以完全不做 DerivedSync。

Gate：

- parser/graph/security/source-stability golden tests；
- local crash/failure tests；
- 20k synthetic benchmark；
- local output 完整且 deterministic；
- rclone 缺失不影响 local compile/status/clean correctness。

---

## 91. Phase 3 — dedicated DerivedSync

实现：

```text
derived sidecar + binding fingerprint
remote ownership claim
compiler_derived_runs
shared RemoteOps / worker attempt refactor
ordinary-result then derived scheduling (no early-return shadowing)
three-phase DerivedPublisher
level-triggered desired state
generation pin
remote purge for desired absent
compile --wait
clean --wait
derived status
```

Gate：rclone local/file remote integration matrix 全过；ordinary failure 不阻止 derived attempt；generation + binding pair 才能满足 current predicate。

---

## 92. Phase 4 — lifecycle and remaining data-plane integration

实现/验证：

```text
disable/enable
remove/restore/forget with profile -> compiler lock ordering
worker-owned profile migrate
worker-owned purge-remote
same-remote migration rejection
pending/running migration-purge lifecycle transitions
enable-after-purge reinitialization on the same folder binding
profile status summary
worker recovery
```

Gate：现有 ordinary lifecycle regression 不变；compiler lifecycle tests 全过；remove 不会执行遗留 purge/migration intent；restore/enable 不会绕过 overlap 或 pending-purge gate。

---

## 93. Phase 5 — real release smoke

执行：

```text
go test ./...
20k benchmark
real Drive compile --wait
real clean --wait
worker-owned migration smoke on a distinct remote
real ChatGPT consumption smoke
```

通过后才把 Knowledge Compiler V1 视为 ready。

---

# Part T：V1 acceptance checklist

## 94. Compiler correctness

- [ ] `knowledge-sync compile <profile>` 对完整 eligible local corpus 做 deterministic full scan。
- [ ] obsidian/generic 共用同一 parser/schema contract。
- [ ] path policy 只来自 committed snapshot；`Profile.Excludes` 不被二次叠加。
- [ ] valid empty committed policy 与 missing/inconsistent policy 可区分，后者 fail closed。
- [ ] profile + initial committed policy 无 runnable transaction gap。
- [ ] `policy_hash` 与 `eligibility_contract_hash` 分开记录并参与 source snapshot。
- [ ] excluded/suppressed source 不被读取、不泄漏。
- [ ] eligible metadata/read failure 是 hard failure，不静默跳过。
- [ ] 只有 regular files 进入 corpus；`.md` note-kind 使用 ASCII case-insensitive contract。
- [ ] symlink 永不跟随。
- [ ] source root 不是 symlink，且不与 app state 或任何 non-forgotten profile source overlap。
- [ ] stable snapshot 有 pre-publication validation，最多自动重试一次。
- [ ] Markdown content SHA-256 正确。
- [ ] 所有 eligible file 的 size/mtime_unix_nano 参与 snapshot；Markdown mtime-only change 可检测。
- [ ] parser syntax facts 只来自 pinned third-party parser/extensions。
- [ ] escaped table wikilink、Unicode、frontmatter、embed、local Markdown links 有 characterization fixtures。
- [ ] fenced/inline code 内 fake tags/links 均被 parser structure 排除。
- [ ] ambiguous link 永不猜。
- [ ] aliases 不参与 resolver。
- [ ] self-link / note embed / attachment embed connectivity 定义有 exact tests。
- [ ] hard orphan / no-backlink / outbound-only 使用 unique-note counts。
- [ ] edge occurrence counts 与 unique-neighbor counts 分离。

## 95. Local publication

- [ ] output 只位于 app state，不写 Vault。
- [ ] local root MANIFEST 是小 current pointer。
- [ ] generation MANIFEST 是 immutable full manifest。
- [ ] generation manifest 记录 compiler_run_id / source snapshot / policy hash / eligibility contract hash / detail artifact hashes。
- [ ] generation manifest inventory 不包含 MANIFEST 自身；root pointer 记录其 hash。
- [ ] failed compile 保留 previous current。
- [ ] manifest-before-DB crash 可 repair。
- [ ] clean intent/pointer/SQLite 各 crash point 可恢复，不留下 dangling desired generation。
- [ ] steady-state retention current + previous。
- [ ] in-flight generation 有 pin，GC 不会删除 upload source。
- [ ] compiler lock 不在网络上传期间持有；compile 与 DerivedSync upload 可以按 pin 规则并行。
- [ ] GC failure 不反判 compile failure。

## 96. Artifact contract

- [ ] 生成 README.md。
- [ ] 生成 MANIFEST.json。
- [ ] 生成 FILE_INDEX.jsonl。
- [ ] 生成 LINKS.jsonl。
- [ ] 生成 ORPHANS.jsonl / ORPHANS.md。
- [ ] 生成 BROKEN_LINKS.jsonl / BROKEN_LINKS.md。
- [ ] 生成 TAG_INDEX.jsonl。
- [ ] 生成 FRONTMATTER_STATS.json。
- [ ] 生成 WARNINGS.jsonl。
- [ ] 生成 KNOWLEDGE_HEALTH.md。
- [ ] FILE_INDEX kind 只有 note / attachment。
- [ ] FILE_INDEX path_id 为完整 SHA-256 path digest。
- [ ] JSONL row 携带 generation id。
- [ ] aggregate/Markdown 携带 generation metadata。
- [ ] ordering deterministic。

## 97. DerivedSync

- [ ] ordinary sync 永不管理 `.knowledge-derived/**`。
- [ ] ordinary manifest 有 protected state，历史 derived paths 经 additive migration 转入 protected。
- [ ] pre-upgrade frozen reserved prune targets 被 stale，final delete 仍重验 canonical/reserved path。
- [ ] DerivedSync 不经过普通 `ArgsFor()`，不继承 global/operation args。
- [ ] V1 无 `derived_sync_args` arbitrary flag surface。
- [ ] derived ownership metadata 在 `.knowledge-sync/derived/<uuid>.json`。
- [ ] destructive operation 前 live display path 唯一解析到 stored folder ID；rename/replacement/duplicate fail closed。
- [ ] ordinary/derived marker claim non-overwriting，alias collision 不覆盖旧 marker。
- [ ] non-empty unclaimed namespace fail closed。
- [ ] claimed namespace exact mirror，unknown files 会被删除。
- [ ] Phase 1 detail sync 排除 MANIFEST。
- [ ] Phase 2 hash-level `rclone check`，不用 size-only / one-way。
- [ ] Phase 3 generation MANIFEST 最后 copyto。
- [ ] 只有三阶段全部成功才 commit remote_published。
- [ ] remote current predicate 同时匹配 generation id 与 binding fingerprint。
- [ ] remote absent/current state 都携带 matching binding fingerprint；migration 后旧 state 不可复用。
- [ ] remote mixed window 有 generation/hash consistency contract。
- [ ] crash after MANIFEST before DB 可通过 idempotent retry 收敛。
- [ ] rapid compile 只追 latest desired state。
- [ ] ordinary failure 不阻止 derived attempt；derived failure 不污染 ordinary health。
- [ ] derived retry/terminal gate 按 desired revision + binding scoped，持久化 backoff 且不会 busy-loop。

## 98. CLI / lifecycle

- [ ] compile 默认 local success 后即返回，DerivedSync 后台进行。
- [ ] compile/status/local clean 以及 profile/status/wait/intent-only lifecycle commands 不要求 rclone executable/config，也不运行 rclone probe。
- [ ] compile `--wait` 可等远端 current。
- [ ] compile/clean wait 在 desired 被替换时返回 superseded，不无限等待。
- [ ] status 默认 quick，不全量 rehash。
- [ ] status `--verify` 做 local artifact integrity hash。
- [ ] clean 默认 local clean + queue remote purge。
- [ ] clean `--wait` 可等 remote absent。
- [ ] disabled profile 可 compile/status/clean，DerivedSync blocked_disabled。
- [ ] enable 后自动继续 latest desired。
- [ ] remove/restore 保留 compiler local/remote data。
- [ ] forget 删 local compiler state，不删 remote。
- [ ] deleting/tombstoned profile 不开始新的 compile/clean mutation，wait 返回 deletion-specific state。
- [ ] lifecycle 双锁顺序固定为 profile -> compiler。
- [ ] migrate/purge-remote 由 worker 执行，CLI 不直接运行 rclone。
- [ ] migrate 只接受不同 remote alias，并在新 binding 重新 publish current generation。
- [ ] purge-remote 要求 derived clean --wait 已收敛为 absent。
- [ ] root-content purge 保留 folder ID/binding，enable 后 full reconcile；remove 会取消未 claim 的 purge/migration。

## 99. Testing / performance / release

- [ ] `go test ./...` 全过。
- [ ] parser/graph/security/crash/ownership/derived/lifecycle integration matrix 全覆盖。
- [ ] Phase 0 ordinary regression 证明 latest fast-event exact clearing 与 metadata fingerprint 行为不回退。
- [ ] 20k+ synthetic corpus 有时间、RSS、artifact size 数据。
- [ ] default CI 不依赖真实 Google Drive。
- [ ] real-Drive canary 与 core CI 分离，并覆盖 compiler publish/clean/ownership，而非只覆盖 ordinary sync。
- [ ] release smoke 在真实 Drive 验证 flat current view + ownership sidecar + manifest consistency。
- [ ] ChatGPT 可以从 README / KNOWLEDGE_HEALTH / compact reports 使用结果，无需理解 internal generation directory。

---

# Part U：明确延后，不在 V1 顺手实现

## 100. Measurement-dependent deferred decisions

只有 V1 数据出来后再决定：

- full compile 是否足够快，能长期保持 manual/full-scan only；
- 是否在 quiet window 自动 compile；
- 是否与 full reconciliation 绑定 cadence；
- 是否需要 JSONL folder partition；
- persistent parsed-file cache 是否真的改善 20k+ workload；
- incremental graph update 是否值得复杂度；
- semantic embeddings / model layer 是否有足够价值；
- 是否需要进一步追求更精细的 Obsidian resolver compatibility。

这些不是 V1 correctness prerequisite。

---

## 101. Optional future semantic layer

如果未来增加：

```text
POSSIBLE_LINKS
TOPIC_CLUSTERS
semantic orphan hints
conceptual duplicates
```

必须：

- 独立 schema / artifact namespace；
- 明确标记 inferential/model-derived；
- 以 deterministic compiler output + source note 为输入；
- 永不覆盖 deterministic facts。

例如模型不能重新定义：

```text
actual backlink count
actual eligible file count
actual broken local link
frontmatter field presence
```

---

# 102. 最终不变量

实现 review 时优先检查以下不变量，而不是只检查命令“能跑”：

```text
1. Input authority:
   compiler corpus == strict shared ordinary committed eligible local corpus;
   committed snapshot is the sole path-policy authority

2. Syntax authority:
   characterization-accepted pinned third-party parser output
   == syntax fact source

3. Local publication authority:
   root pointer hash -> immutable generation manifest;
   generation manifest inventories details, never itself

4. Derived desired state:
   level-triggered current generation or absent

5. Remote ownership:
   .knowledge-sync/derived/<profile_uuid>.json
   bound to remote name + folder id fingerprint

6. Remote presentation:
   flat .knowledge-derived current view only

7. Remote commit marker:
   generation MANIFEST copied last

8. Ordinary protection:
   ordinary lane never owns .knowledge-derived/**;
   historical ledger rows are protected, never suppressed/pruned/deleted

9. Data-plane ownership:
   existing single worker is sole ongoing mirror/derived mutation owner
   for reconcile/fast/prune/DerivedSync/migration/root-content purge;
   bootstrap/probe exceptions never touch .knowledge-derived/**

10. Framework reuse:
    ordinary/derived share execution-attempt infrastructure,
    not correctness algorithms

11. Failure isolation:
    local compile, ordinary sync, derived sync have separate durable health

12. No model dependency:
    deterministic structural correctness never requires LLM/embedding/network AI

13. Local command independence:
    compile/status/local clean never require or execute rclone

14. Lifecycle serialization:
    compiler-local lock is exclusive for local mutation;
    dual-lock order is always profile -> compiler

15. Binding-scoped publication knowledge:
    remote current/absent requires matching binding fingerprint;
    generation publication additionally requires matching generation id
```

如果未来修改破坏其中任何一条，必须作为显式 architecture/schema change 重新 review，而不是在 implementation 中静默偏离。
