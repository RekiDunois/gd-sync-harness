# Knowledge Compiler V1 最终实施计划

> 状态：已完成 design grill；本文中的 V1 决策视为锁定实现约束。  
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
2. compiler 输入严格复用 profile 的 committed source eligibility policy；
3. compiler 输出位于 app-owned state dir，**不写入 Vault**；
4. `.knowledge-derived/**` 是 system-reserved namespace，普通 sync 永远不上传、修改或删除它；
5. DerivedPublisher 是该 namespace 的唯一 data-plane owner；
6. DerivedSync 与普通 sync 共享 rclone execution、worker、profile lock / remote lease、progress / error / attempt 框架，但不共享 correctness algorithm；
7. 远端只暴露平铺的 current view，不暴露 generation UUID 目录；
8. local immutable generation + manifest-last remote commit 共同提供 crash-safe、可验证的 publication 语义；
9. syntax facts 必须来自固定版本第三方 parser/extension 的结构化结果，compiler 不再手写第二套 Markdown lexer；
10. full compile 是 V1 correctness authority；自动化、增量 cache、partition、semantic layer 全部后置。

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

---

## 4. Compiler corpus 必须严格复用 committed eligibility policy

Compiler input corpus 定义为：

> 当前本地 source 中，按照 profile 已提交的普通同步 eligibility policy 判定为 active + eligible 的文件集合。

必须复用普通同步的 authoritative eligibility helper；不得在 `internal/compiler` 里复制一套 exclude / gitignore / max-file-size / symlink 判定逻辑。

至少共享：

```text
structured exclude policy
.gitignore / committed ignore policy（若当前版本已启用）
max_file_size
symlink rule
system-reserved paths
```

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
policy_hash
sorted eligible membership
Markdown content SHA-256
non-Markdown size + mtime metadata
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
  "profile_id": "obsidian-main",
  "profile_uuid": "...",
  "current_generation_id": "uuid-C",
  "generation_manifest_path": "generations/uuid-C/MANIFEST.json",
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

任何失败都不能修改上一代 root pointer。

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

Compiler lock 只用于短时间 state mutation / pin / publication / clean，不允许在整个网络上传期间持有，从而不阻塞下一次 compile。

---

# Part D：source stability 与 corpus correctness

## 12. Stable source snapshot

Compiler 必须保证发布的 artifacts 对应一个稳定 source snapshot。

流程：

```text
scan 1
  -> determine eligible membership
  -> Markdown SHA-256
  -> non-Markdown size/mtime
  -> policy_hash
  -> parse/build artifacts

pre-publication validation
  -> recompute eligible membership
  -> rehash Markdown
  -> re-read non-Markdown size/mtime
  -> re-read committed policy hash
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
```

非 Markdown attachment：

```text
content_sha256 = null / omitted
size + mtime retained
```

V1 不为了 FILE_INDEX 对 PDF、图片、视频等全部做 content hash；Derived artifact 自身仍由 generation manifest 做完整 SHA-256。

---

# Part E：Parser 与 syntax contract

## 14. Parser architecture

V1 使用纯 Go parser stack，不依赖 Obsidian、Node、Electron 或 plugin runtime。

推荐 stack：

```text
github.com/yuin/goldmark
go.abhg.dev/goldmark/wikilink
go.abhg.dev/goldmark/hashtag   (ObsidianVariant)
go.abhg.dev/goldmark/frontmatter
go.yaml.in/yaml/v3
```

实施时将这些依赖作为 direct dependencies 写入 `go.mod` 并 pin 精确版本；禁止运行时依赖 floating/latest 行为。

Parser behavior 由 characterization fixtures 固定。

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

Inline tag syntax 以 `goldmark/hashtag` 的 Obsidian-compatible variant 解析结果为准。

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
eligible Markdown -> note
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
mtime
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
mtime
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
last_compile_error

desired_derived_mode       // generation | absent
desired_derived_generation_id
remote_published_generation_id
remote_state                // absent | generation | unknown as needed
derived_state               // pending | syncing | current | failed | blocked_disabled
active_publish_generation_id
last_derived_error
last_derived_success_at
```

重点：

- desired state 是 **level-triggered current desire**，不是 per-generation event queue；
- `desired_derived_mode=absent` 用于 clean/purge，不和“从未编译”混淆；
- `remote_published_generation_id` 是 worker 在成功 MANIFEST commit 后写入的 durable knowledge；
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
derived_state = current
```

如果 Phase 1/2/3 任一步失败：

```text
desired_generation remains C
remote_published remains previous known value
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
3. full MANIFEST 记录每个 artifact hash；
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
remote_folder_id
derived_path = ".knowledge-derived"
created_at
```

所有 destructive DerivedSync operation 前必须：

1. 验证普通 profile ownership sidecar；
2. 验证 derived sidecar。

Mismatch / malformed：fail closed。

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

## 50. 同一个 worker，是唯一 rclone data-plane owner

不启动：

```text
derived-worker daemon
compiler uploader daemon
```

现有 worker 继续是唯一 data-plane runtime owner。

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

---

## 51. Scheduler ordering

保留 ordinary lane 当前既有优先级与 destructive-safety 语义，不为 compiler 改写它。

每个 active profile 的 worker pass：

```text
profile deletion lifecycle gate
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

关键规则：

- ordinary work 优先尝试；
- ordinary failure **不阻止**同一 pass 后续 DerivedSync 尝试；
- derived failure 不把 ordinary profile mirror health 改成 failed；
- profile deletion request 优先级仍最高，deleting/tombstoned profile 不开始新 derived attempt。

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

---

# Part M：CLI contract

## 54. Compile

```bash
knowledge-sync compile <profile>
```

默认行为：

```text
load non-tombstoned profile
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
```

如果 profile disabled：

- local compile 仍可成功；
- derived state = `blocked_disabled`；
- `--wait` 不无限等待，应直接返回 actionable blocked state，提示先 enable profile。

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
  eligible files / notes / warning count

Derived:
  desired
  remote_published
  state
  last_success
  last_error
```

默认：

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
artifact exists
bytes/hash match immutable MANIFEST
```

不把 `--verify` 变成远端 Drive check。

---

## 57. `compiler clean`

```bash
knowledge-sync compiler clean <profile>
```

本地逻辑立即：

```text
remove local current pointer
remove unpinned generations
remove staging
preserve compiler run/history rows
set desired_derived_mode = absent
wake worker
```

如果有 in-flight pinned generation，物理目录允许暂时保留到 attempt 结束；从用户语义上 local current 已经 absent。

默认返回：

```text
Compiler artifacts cleaned locally.
Remote derived cleanup queued.
```

Worker 后续：

```text
validate profile ownership
validate derived ownership
purge remote <profile root>/.knowledge-derived/
mark remote derived state absent
```

不使用 ordinary `max_delete`，不走普通 prune authorization，因为该 subtree 已是 compiler-owned exact mirror。

### 57.1 `clean --wait`

等待 remote derived state 变为 absent。

Disabled profile 下不无限等待，返回 `blocked_disabled` 并保留 desired absent，enable 后继续 purge。

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

Enable：

```text
existing worker scheduling resumes
latest desired derived state converges automatically
```

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

### restore

```text
restore compiler state visibility
enable 后继续收敛 existing desired state
```

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

但不访问、不删除 remote：

```text
old remote .knowledge-derived remains
old remote .knowledge-sync/derived/<profile_uuid>.json remains
```

这与普通 profile remote data “forget 不做 remote destructive cleanup”保持一致，也避免留下“内容还在但 sidecar 被单独删掉”的 unowned subtree。

---

## 61. `profile migrate`

当前 ordinary migration 切换到新的 remote binding，并保留旧 remote root。

Compiler 行为：

```text
local current generation C 保持不变
old remote derived view + old derived sidecar 保留
DB remote binding cutover
remote_published_generation_id for new binding = none/unknown
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

---

# Part O：Crash / failure semantics

## 62. Local compile failure

以下任何 hard failure：

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

Malformed per-file frontmatter 等已定义 nonfatal 情况进入 WARNINGS，不扩大成 whole compile failure。

---

## 63. Crash repair boundaries

至少覆盖：

```text
crash before generation directory atomic publish
crash after generation directory publish but before root pointer
crash after root pointer but before SQLite run success
crash during local GC
crash during Derived Phase 1
crash during Derived Phase 2
crash after remote MANIFEST write but before DB remote success commit
```

Repair 原则：

- local root pointer + immutable manifest 是 local publication authority；
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
inline code fake tags/links（若 parser extension 支持可靠排除）
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
same basename in different directories
ambiguous basename
missing target
symlink
```

关键测试原则：

> fixture 的 syntax fact 先由第三方 parser/extension 产生；compiler 测试 resolver/aggregation，不通过自己扫 raw Markdown 造 expected parser behavior。

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
- policy change 后 `policy_hash` 改变；
- active local corpus 与 ordinary eligibility helper 一致。

---

## 69. Source-stability tests

覆盖：

```text
Markdown content changes during compile
membership add/remove during compile
non-Markdown size/mtime changes
policy hash changes
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
```

验证：

- root pointer 永不指 partial generation；
- old current 保留；
- manifest-before-DB crash 可 repair；
- GC failure 不反判 compile；
- current + previous retention 收敛；
- in-flight pin 防止 generation 被删除。

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
malformed sidecar -> fail closed
clean preserves derived sidecar
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
success -> remote_published advanced
crash after Phase 3 before DB -> idempotent retry converges
rapid B/C/D compile -> latest desired converges
in-flight older generation pin
clean -> desired absent -> remote purge
disabled -> blocked_disabled
enable -> resumes
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
```

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
```

---

## 75. Determinism / schema golden tests

固定 corpus 连续 compile 多次：

- UUID/timestamp 可不同；
- normalized structural rows、ordering、counts、resolution outcomes 必须一致；
- JSONL stable sort；
- full manifest artifact count/hash 与实际文件匹配；
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

Release acceptance 至少一次真实 smoke：

```text
knowledge-sync compile <profile> --wait
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

---

# Part R：实施结构与代码改动建议

## 78. 新增 compiler package

推荐新增：

```text
internal/compiler/
  compiler.go          // orchestration
  corpus.go            // shared eligibility adapter + snapshot
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

普通 sync 当前 source scanning/eligibility 逻辑必须提取成 compiler 可复用的 authoritative helper。

目标：

```text
ordinary scan
compiler corpus scan
```

调用同一 eligibility policy implementation。

需要修复任何“sync 扫描和 compiler 扫描对 Oversize/Exclude/Symlink 判定不同”的潜在分叉；compiler 不允许通过 copy-paste 保持“看起来一样”。

---

## 80. Paths

扩展现有 `internal/paths`：

```text
CompilerRoot(profileUUID)
CompilerGenerationsDir(profileUUID)
CompilerStagingDir(profileUUID)
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
compiler_profile_state
```

如果为了 shared attempt telemetry 需要 derived attempt history，可增加 compiler-specific/additive run table；不要为了复用现有 integer source-generation 字段而硬塞进普通 `sync_runs`。

真正要复用的是 worker execution/attempt framework，不是强迫两种 operation 共用不兼容的数据模型。

Migration 必须向后兼容已有 DB；旧 profile 默认没有 compiler state。

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
```

复用现有 App lifecycle / DB / rclone discovery 行为；read-only status 不应因为 derived remote 暂时不可达而失效。

---

## 83. Derived ownership helper

沿用 `internal/sidecar` 的整体 pattern，新增 derived metadata helper，例如：

```text
DerivedSidecarPath(profileUUID)
ReadDerived
WriteDerived
ValidateDerived
ClaimDerived
```

普通 profile ownership validation 仍先执行。

不要把 derived marker 写进 `.knowledge-derived/`。

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

如果当前实现仍有 `--delete-excluded` 等会将 excluded object 变成 deletion target 的路径，必须先保证 protected derived subtree 不会进入该 deletion semantics，再启用 DerivedPublisher。

---

## 87. Dependency addition

`go.mod` 当前只包含已有 CLI/SQLite dependencies；compiler implementation 增加 parser direct dependencies。

要求：

- 精确版本 pin；
- parser behavior fixture 锁定；
- 不引入 Node/CGO parser runtime；
- 不引入 model SDK。

---

# Part S：推荐实施顺序

## 88. Phase 1 — shared corpus contract

先做：

1. shared authoritative eligibility helper；
2. system-reserved `.knowledge-derived` path protection；
3. compiler paths / lock skeleton；
4. parser dependencies + characterization fixtures。

Gate：普通 sync regression tests 全过，derived path protection 有 destructive test。

---

## 89. Phase 2 — standalone deterministic compiler

实现：

```text
full corpus scan
stable snapshot
parser adapters
resolver
graph aggregation
artifact schemas
local immutable generation publication
root pointer
local retention
compiler_runs / profile state
compile/status --verify
```

这个阶段可以完全不做 DerivedSync。

Gate：

- parser/graph/security/source-stability golden tests；
- local crash/failure tests；
- 20k synthetic benchmark；
- local output完整且 deterministic。

---

## 90. Phase 3 — dedicated DerivedSync

实现：

```text
derived sidecar
remote ownership claim
shared RemoteOps / worker attempt refactor
three-phase DerivedPublisher
level-triggered desired state
generation pin
remote purge
compile --wait
clean --wait
derived status
```

Gate：rclone local/file remote integration matrix 全过。

---

## 91. Phase 4 — lifecycle integration

实现/验证：

```text
disable/enable
remove/restore/forget
migrate
profile status summary
worker recovery
```

Gate：现有 ordinary lifecycle regression 不变，compiler lifecycle tests 全过。

---

## 92. Phase 5 — real release smoke

执行：

```text
go test ./...
20k benchmark
real Drive compile --wait
real clean --wait
real ChatGPT consumption smoke
```

通过后才把 Knowledge Compiler V1 视为 ready。

---

# Part T：V1 acceptance checklist

## 93. Compiler correctness

- [ ] `knowledge-sync compile <profile>` 对完整 eligible local corpus 做 deterministic full scan。
- [ ] obsidian/generic 共用同一 parser/schema contract。
- [ ] excluded/suppressed source 不被读取、不泄漏。
- [ ] symlink 永不跟随。
- [ ] stable snapshot 有 pre-publication validation，最多自动重试一次。
- [ ] Markdown content SHA-256 正确。
- [ ] parser syntax facts 只来自 pinned third-party parser/extensions。
- [ ] escaped table wikilink、Unicode、frontmatter、embed、local Markdown links 有 characterization fixtures。
- [ ] ambiguous link 永不猜。
- [ ] aliases 不参与 resolver。
- [ ] self-link / note embed / attachment embed connectivity 定义有 exact tests。
- [ ] hard orphan / no-backlink / outbound-only 使用 unique-note counts。
- [ ] edge occurrence counts 与 unique-neighbor counts 分离。

## 94. Local publication

- [ ] output 只位于 app state，不写 Vault。
- [ ] local root MANIFEST 是小 current pointer。
- [ ] generation MANIFEST 是 immutable full manifest。
- [ ] generation manifest 记录 compiler_run_id / source snapshot / policy hash / artifact hashes。
- [ ] failed compile 保留 previous current。
- [ ] manifest-before-DB crash 可 repair。
- [ ] steady-state retention current + previous。
- [ ] in-flight generation 有 pin，GC 不会删除 upload source。
- [ ] GC failure 不反判 compile failure。

## 95. Artifact contract

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

## 96. DerivedSync

- [ ] ordinary sync 永不管理 `.knowledge-derived/**`。
- [ ] DerivedSync 不经过普通 `ArgsFor()`，不继承 global/operation args。
- [ ] V1 无 `derived_sync_args` arbitrary flag surface。
- [ ] derived ownership metadata 在 `.knowledge-sync/derived/<uuid>.json`。
- [ ] non-empty unclaimed namespace fail closed。
- [ ] claimed namespace exact mirror，unknown files 会被删除。
- [ ] Phase 1 detail sync 排除 MANIFEST。
- [ ] Phase 2 hash-level `rclone check`，不用 size-only / one-way。
- [ ] Phase 3 generation MANIFEST 最后 copyto。
- [ ] 只有三阶段全部成功才 commit remote_published。
- [ ] remote mixed window 有 generation/hash consistency contract。
- [ ] crash after MANIFEST before DB 可通过 idempotent retry 收敛。
- [ ] rapid compile 只追 latest desired state。
- [ ] ordinary failure 不阻止 derived attempt；derived failure 不污染 ordinary health。

## 97. CLI / lifecycle

- [ ] compile 默认 local success 后即返回，DerivedSync 后台进行。
- [ ] compile `--wait` 可等远端 current。
- [ ] status 默认 quick，不全量 rehash。
- [ ] status `--verify` 做 local artifact integrity hash。
- [ ] clean 默认 local clean + queue remote purge。
- [ ] clean `--wait` 可等 remote absent。
- [ ] disabled profile 可 compile/status/clean，DerivedSync blocked_disabled。
- [ ] enable 后自动继续 latest desired。
- [ ] remove/restore 保留 compiler local/remote data。
- [ ] forget 删 local compiler state，不删 remote。
- [ ] migrate 在新 remote 重新 publish current generation。

## 98. Testing / performance / release

- [ ] `go test ./...` 全过。
- [ ] parser/graph/security/crash/ownership/derived/lifecycle integration matrix 全覆盖。
- [ ] 20k+ synthetic corpus 有时间、RSS、artifact size 数据。
- [ ] default CI 不依赖真实 Google Drive。
- [ ] release smoke 在真实 Drive 验证 flat current view + ownership sidecar + manifest consistency。
- [ ] ChatGPT 可以从 README / KNOWLEDGE_HEALTH / compact reports 使用结果，无需理解 internal generation directory。

---

# Part U：明确延后，不在 V1 顺手实现

## 99. Measurement-dependent deferred decisions

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

## 100. Optional future semantic layer

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

# 101. 最终不变量

实现 review 时优先检查以下不变量，而不是只检查命令“能跑”：

```text
1. Input authority:
   compiler corpus == ordinary committed eligible local corpus

2. Syntax authority:
   third-party parser output == syntax fact source

3. Local publication authority:
   root pointer -> immutable generation manifest

4. Derived desired state:
   level-triggered current generation or absent

5. Remote ownership:
   .knowledge-sync/derived/<profile_uuid>.json

6. Remote presentation:
   flat .knowledge-derived current view only

7. Remote commit marker:
   generation MANIFEST copied last

8. Ordinary protection:
   ordinary lane never owns .knowledge-derived/**

9. Data-plane ownership:
   existing single worker is sole rclone runtime owner

10. Framework reuse:
    ordinary/derived share execution-attempt infrastructure,
    not correctness algorithms

11. Failure isolation:
    local compile, ordinary sync, derived sync have separate durable health

12. No model dependency:
    deterministic structural correctness never requires LLM/embedding/network AI
```

如果未来修改破坏其中任何一条，必须作为显式 architecture/schema change 重新 review，而不是在 implementation 中静默偏离。
