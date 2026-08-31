# knowledge-sync ignored churn / redundant full reconcile 修复计划

## 0. 文档状态

- 状态：Implementation Plan
- 目标仓库：`gd-sync-harness`
- 基线：`master`，诊断时 HEAD `197df7c622609608794dda9e554603edf2b26173`
- 主要影响模块：
  - `internal/state/events.go`
  - `internal/cli/fastupsert.go`
  - `internal/sync/policy.go`
  - `internal/sync/reconcile.go`
  - 对应 state / cli / sync 测试
- 不要求数据库 schema migration。

---

## 1. 问题摘要

当前 watcher 的设计是“durable fact producer”：它故意不在 watcher 边界根据 `.gitignore` 丢事件，因为 watcher 看到的 policy 可能过期；如果在这里过滤，一个今天被 ignore 的路径在 policy 更新后重新 eligible 时，历史事件可能永久丢失。

这个原则本身是正确的。

问题出在 watcher 之后的 durable scheduling：`RecordEvent()` 目前只要发现以下任一条件成立，就会把事件折叠为 full-reconcile debt：

```go
if full || !last.Valid || desired > last.Int64 || current.Valid {
    // desired_generation = MAX(desired_generation, generation)
    // reconcile_requested = 1
    // DELETE pending_events
}
```

因此，一个本来属于 safe fast path 的普通 `create` / `modify`，只要恰好发生在 full reconcile 执行期间（`current_run_id != NULL`），就会：

1. 增加 `source_generation`；
2. 推高 `desired_generation`；
3. 丢掉 path-level `pending_events` evidence；
4. 让当前 full run 只能提交到旧 target；
5. 当前 run 成功后仍留下 `desired_generation > last_success_generation`；
6. worker 立刻 claim 下一轮 full reconcile。

这会让 committed policy 已经明确 ignore 的临时目录 churn 触发昂贵但没有业务价值的完整 reconcile。

---

## 2. 已验证现场

现场 profile：`example-profile`。

Committed ignore policy：

```text
policy_source:          gitignore
policy_hash:            a2ec73ae121e358a966896dabd921455137bc236f52eb1f97400a56c3dc3615c
committed_generation:   355324
current disk snapshot:  clean
matcher warnings:       0
refresh state:          ready
refreshed hash:         a2ec73ae121e358a966896dabd921455137bc236f52eb1f97400a56c3dc3615c
active managed:         18106
suppressed managed:     0
```

根 `.gitignore` 已包含：

```gitignore
.tmp_scripts/
```

针对 `.tmp_scripts/...` 的 manifest 查询返回 0 行，说明 ignore matcher、active scan、manifest refresh 都正常工作；`.tmp_scripts` 文件没有进入 managed active set，也没有被作为 active 文件上传。

实际 run 时间线（本地时区）：

```text
4b99a586... | target=409468 | failed    | 01:05:32 -> 01:53:33 | files=18063 | worker_interrupted
ba2b60ac... | target=409468 | succeeded | 01:53:33 -> 02:51:42 | files=18104
3c1eaa9c... | target=409469 | succeeded | 02:51:47 -> 03:14:01 | files=18106
```

`409468` 成功后仅 5 秒就 claim `409469`。

TMU 后台任务在约 02:51 写入 `.tmp_scripts` 文件，与 generation 从 `409468` 到 `409469` 的时间高度吻合。即使具体是哪一个 fs event 触发 409469 已因当前 promotion 分支清空 `pending_events` 而无法事后证明，状态机缺陷本身已可独立复现和验证。

`18104 -> 18106` 与 `.tmp_scripts` 无关：两个 ignore 文件没有进入 active manifest；这一小时内源目录存在正常 active 文件变化，因此下一轮 scan 的净 active 数量增加 2 是合理现象。

本次额外 full reconcile 从 02:51:47 运行到 03:14:01，约 22 分钟。即使 rclone 最终不重传所有 18106 个文件，也仍付出了完整 local scan、preflight、remote comparison、reconcile 和相关 I/O 成本。

---

## 3. 根因

### 3.1 根因 A：safe event 被 active full run 错误提升为 full debt

`internal/state/events.go` 当前把“是否已有 current run / full debt”和“当前事件本身是否必须 full reconcile”混成了一个决策。

事件本身是：

- `create` / `modify`：通常可由 worker fast-upsert 处理；
- `delete` / `rename` / `other`：具有 destructive / uncertain 语义，需要 full reconcile。

但当前代码会因为：

- `current.Valid`
- `desired > last`
- `!last.Valid`

把 safe event 也转换成 full debt。

这导致“当前系统正在做 full”被错误地当成“新事件也必须 full”。

### 3.2 根因 B：promotion 会删除 path-level evidence

promotion 分支执行：

```sql
DELETE FROM pending_events WHERE profile_id = ?
```

因此一旦 safe event 被错误提升，后续 worker 不再知道 debt 是由哪个 path 造成的，也无法用 committed policy 判断：

```text
.tmp_scripts/foo
    -> excluded
    -> consume as no-op
```

这也是为什么事后无法从数据库精确反查 generation 409469 的路径来源。

### 3.3 根因 C：excluded-only fast batch 存在过宽清理窗口

`runFastUpsertBatchAt()` 当前在整批 pending 经过 owned committed policy recheck 后都变成 stale / excluded 时执行：

```go
return app.DB.ClearPending(p.ID)
```

这是 profile-wide delete。

worker 在 `ListPending()` 后到 `ClearPending()` 前，watcher 仍可能写入更新的 pending event；这样新事件可能被错误一起删除。

已有 `ClearPendingEvents(profileID, events)` 支持：

```sql
DELETE ...
WHERE profile_id = ?
  AND path = ?
  AND source_generation <= ?
```

因此 excluded/stale no-op 也必须使用 exact-version clearing，而不是 profile-wide clearing。

### 3.4 独立回归：policy-aware preflight fingerprint 只比较 path

当前 `PreflightProtected()`：

```go
active1 := ScanActivePaths(...)
fp1 := fingerprintPaths(active1)
...
active2 := ScanActivePaths(...)
dry.SourceStable = fp1 == fingerprintPaths(active2)
```

`fingerprintPaths()` 只包含相对路径。

结果：

- 新增 / 删除 active path：能检测；
- 同一路径内容变化但 path 不变：不能检测；
- size / mtime 变化：不能检测。

旧的 `ScanResult.ChangedFingerprint()` 至少包含 `RelPath + Size + ModTime`。

这个回归不是 ignored churn 的直接根因，但与本次修改处于同一 correctness boundary：修复 safe-event promotion 后，更不能继续依赖 path-only fingerprint 作为 source-stability 保护。

---

## 4. 修复目标

### 4.1 必须实现

1. watcher 继续记录 ignored path 的 durable fact，不在 watcher 侧重新引入 policy drop gate。
2. 普通 `create` / `modify` 不再仅因为 `current_run_id` 存在而产生新的 full-reconcile debt。
3. 普通 `create` / `modify` 在已有 full debt 时也保留 path-level durable pending evidence，而不是被 blanket collapse。
4. full run 完成后，worker 使用自己持有的 committed policy snapshot recheck pending safe events：
   - excluded / stale：精确消费，不上传；
   - eligible：走 targeted fast upsert。
5. excluded-only / stale-only fast batch 不允许 profile-wide 清空后来新到的事件。
6. destructive / uncertain 事件仍 fail-safe 地触发 full reconcile。
7. 恢复 policy-aware active scan 的 metadata fingerprint，使 active same-path change 能重新触发 source-unstable / dirty detection。
8. 不改变 `.gitignore` committed snapshot 作为唯一 path authority 的原则。

### 4.2 非目标

本次不做：

- watcher 直接读取并缓存 committed policy 后丢 ignored event；
- 为 ignored `delete` / `rename` / `other` 做 aggressive short-circuit；
- 试图在 full run claim 后再“猜这一轮 debt 是否 ignored-only”并跳过整个 run；
- 改变 delete budget / prune / suppression 的现有安全模型；
- 引入新的数据库 schema；
- 用内容 hash 扫描所有 18k+ 文件作为稳定性 fingerprint（成本过高）。

---

## 5. 关键安全不变量

实现过程中必须保持以下不变量。

### I1. watcher 不做最终 eligibility authority

watcher 记录事实，不因为当前 ignore snapshot 就永久丢事件。

### I2. destructive / uncertain 优先 full

以下事件继续进入 full reconcile：

- delete
- rename
- other / overflow / platform uncertainty
- caller 显式 `full=true`
- 未识别 event kind（防御性 fail closed）

### I3. safe event 永远保留 path evidence

对于已初始化或已有初始化/full debt 的 profile：

```text
create / modify
    -> source_generation++
    -> durable pending_events upsert
    -> 不因 current_run_id 或 desired>last 自动推进 desired_generation
```

### I4. source_generation 与 desired_generation 允许暂时分离

`source_generation` 表示观察到的 filesystem fact 顺序；
`desired_generation` 表示必须通过 full reconcile 才能偿还的 debt watermark。

因此允许：

```text
source_generation = 409469
desired_generation = 409468
last_success_generation = 409468
pending_events contains generation 409469
```

这不是不一致，而是“没有 full debt，但还有 fast-event work”。

### I5. full success 只清理自己 target 以内的 pending event

现有：

```sql
DELETE FROM pending_events
WHERE profile_id = ?
  AND source_generation <= target_generation
```

继续保留。

因此 full run target=G 执行期间产生的 safe event G+1 会在 run 成功后继续存在，并由 fast path 处理。

### I6. exact-version clearing

任何基于先前 `ListPending()` snapshot 做出的 no-op / success 清理，都必须使用 path + source_generation 边界，不能 blanket `ClearPending(profile)`。

### I7. policy refresh / destructive safety 不降级

policy change 产生的 full refresh debt、suppression、prune 和 proven delete 逻辑保持原语义。

---

## 6. 新的事件决策模型

### 6.1 分类函数

建议在 state 层把决策显式化，不继续用一个大 boolean 条件隐式表达。

伪代码：

```go
func isSafeFastKind(kind string) bool {
    return kind == EventCreate || kind == EventModify
}

func shouldPromoteEvent(full bool, kind string, lastValid bool, desired int64, currentValid bool) bool {
    // destructive / uncertain 永远 full
    if full || !isSafeFastKind(kind) {
        return true
    }

    // bootstrap fallback：完全没有成功基线，也没有任何已经存在的
    // initial/full intent/run 时，不能只留下 fast event。
    if !lastValid && desired == 0 && !currentValid {
        return true
    }

    return false
}
```

注意：`currentValid` 不再是 safe event promotion 的理由；`desired > last` 也不再是 safe event promotion 的理由。

### 6.2 为什么已有 full debt 时 safe event 仍应保留 pending

场景：

```text
full debt target = G
safe modify arrives at source generation G+1
```

不把 target 推成 G+1，意味着 upcoming full run 的审计 target 仍是 G。

实际 full scan 会读取执行时的最新 active tree，通常已经包含 G+1 的内容；但为了不依赖“它一定在 scan 前发生”，G+1 pending event 仍保留。

full 成功后：

- 如果 full 已经覆盖它：后续 fast-upsert 最多做一次幂等 targeted copy；
- 如果发生在 full scan / upload 之后：fast-upsert 正好补齐；
- 如果路径被 ignore：worker 直接消费 no-op。

这里选择“可能多一次 targeted upsert”，换取“不把整个 profile 再跑一次 full reconcile”。

### 6.3 bootstrap 行为

如果 profile 从未成功初始化，并且：

```text
last_success_generation IS NULL
desired_generation == 0
current_run_id IS NULL
```

此时第一个 safe event 仍应建立 initial/full debt，防止 profile 永远只做局部 upsert 而没有建立 authoritative baseline。

正常 `profile add` 已经会创建 initial reconciliation intent；这个分支主要是 defensive fallback。

---

## 7. `internal/state/events.go` 修改

### 7.1 拆分 promotion 与 detailed-event 路径

把当前：

```go
if full || !last.Valid || desired > last.Int64 || current.Valid {
    ...
} else {
    ...
}
```

替换成显式的 event-kind 决策。

建议抽 helper：

```go
safe := kind == EventCreate || kind == EventModify
bootstrapNeedsFull := !last.Valid && desired == 0 && !current.Valid
promote := full || !safe || bootstrapNeedsFull
```

### 7.2 safe detailed path 保持原子性

仍在与 `source_generation++` 同一个 transaction 内 upsert：

- path
- kind
- first_seen / last_seen
- source_generation
- observed_policy_hash
- observed_policy_generation
- policy_context_known

不能拆成两个事务，否则重新打开 generation 已增加但 event 未持久化的 crash window。

### 7.3 safe path 不更新 full-debt 字段

safe event detailed 分支不得修改：

- `desired_generation`
- `reconcile_not_before_at`
- `reconcile_deadline_at`
- `profile_runtime.reconcile_requested`

worker wake 仍由 watcher / socket invalidation 负责；durable pending event 是 fast-work authority。

### 7.4 destructive promotion 继续可 collapse 已有 pending

当真正 destructive / uncertain event 到达时，full run 将成为 authoritative repair barrier。

此时允许现有 promotion 行为继续：

- 推进 `desired_generation` 到当前 generation；
- 设置 debounce；
- `reconcile_requested=1`；
- collapse 之前的 pending events。

事务串行保证 promotion 后新到达的更高 generation 事件不会被这个 transaction 的 delete 吃掉。

---

## 8. `internal/cli/fastupsert.go` 修改

### 8.1 分类为 eligible 与 discardable

当前只构造 `eligible`，建议改成：

```go
var eligible []state.PendingEvent
var discardable []state.PendingEvent
```

对于 safe create/modify：

- `os.Stat` 不存在：按当前语义视为 stale，加入 `discardable`；
- committed matcher excluded：加入 `discardable`；
- 仍存在且 eligible：加入 `eligible`。

如果出现 destructive kind，继续走现有 full promotion，不进入 fast copy。

### 8.2 禁止 excluded-only 使用 `ClearPending(profile)`

把：

```go
if len(eligible) == 0 {
    return app.DB.ClearPending(p.ID)
}
```

改为 exact-version clear：

```go
if len(eligible) == 0 {
    return app.DB.ClearPendingEvents(p.ID, pending)
}
```

这样如果 `ListPending()` 之后 watcher 又写了同一路径 generation 更高的新事件：

```sql
source_generation > snapshot.source_generation
```

它会保留下来。

### 8.3 mixed batch 也消费 discardable snapshot

当前 mixed batch 会上传 eligible，但 ignored/stale 项可能继续留到下一 pass。

建议：

1. 在 owned snapshot 下完成 classification；
2. 对 `discardable` 调用 `ClearPendingEvents()`；
3. 对 eligible 执行 FastUpsert；
4. ledger barrier 成功后，对 eligible 调用 `ClearPendingEvents()`。

如果担心 upload 前清 discardable 与 policy 并发变化，必须依赖 worker 已持有的 profile ownership / committed snapshot 不变量；policy refresh 会产生独立 full debt，不应靠旧 ignored event 修复新 policy。

更保守实现也可在 eligible upload 成功后再清两类，但无论顺序如何都必须 exact-version。

### 8.4 fast success generation 语义

本次不要求 fast-upsert 推进 `last_success_generation`。

`last_success_generation` 继续表示 authoritative full reconciliation 已提交到的 target；safe pending event 由自己的 `source_generation` 和 exact clearing 表示完成情况。

因此合法状态是：

```text
source_generation > last_success_generation
source_generation > desired_generation
pending_events = 0
HasDebt() = false
```

后续 destructive / manual / scheduled full intent会以新的 source generation 建立新的 full target。

若现有 `MarkFastSuccess()` 内部存在与此相冲突的 generation 更新，实施前必须先调整或加测试固定语义；不要为了让数字“看起来相等”而人为推进 full-reconcile watermark。

---

## 9. policy-aware metadata fingerprint 修复

### 9.1 新增 metadata-aware active scan

不要继续让 `ScanActivePaths()` 同时承担“files-from 列表”和“source-stability fingerprint”两个不同职责。

建议在 `internal/sync/policy.go` 新增：

```go
type ActiveEntry struct {
    RelPath    string
    Size       int64
    ModTimeNS  int64
}

func ScanActiveEntries(sourcePath string, maxFileSize int64, snap *policy.Snapshot) ([]ActiveEntry, error)
```

walker 仍使用同一个 committed matcher：

- ignored dir：不 descend；
- ignored file：skip；
- symlink：skip；
- oversize：skip；
- active file：记录 path + size + high-resolution mtime。

建议 fingerprint 使用 `UnixNano()`；不要复用 manifest 的 Unix seconds 精度作为 preflight 稳定性判定。

### 9.2 `ScanActivePaths()` 变成 projection

可实现为：

```go
entries, err := ScanActiveEntries(...)
paths := make([]string, len(entries))
for i := range entries {
    paths[i] = entries[i].RelPath
}
```

这样 rclone `--files-from`、manifest active set、preflight fingerprint 共享一套 eligibility traversal，避免 matcher 语义漂移。

如果性能上需要避免一次调用内重复 stat，可让调用者直接保留 entries 并 projection。

### 9.3 metadata fingerprint

新增：

```go
func fingerprintActiveEntries(entries []ActiveEntry) string
```

至少包含：

- sorted `RelPath`
- `Size`
- `ModTimeNS`

不要求内容 hash。

### 9.4 更新 `PreflightProtected()`

改为：

```text
active1 metadata scan
    -> fp1
    -> dry-run using same committed policy
active2 metadata scan
    -> fp2
SourceStable = fp1 == fp2
```

ignored `.tmp_scripts` churn 不进入 active entries，因此不会导致 `source_unstable`。

active 文件即使路径不变，只要 size / mtime 变化，也能重新检测到 source instability。

### 9.5 更新 `markDirtyIfChangedProtected()`

live sync 后再次做 metadata-aware active scan，并与 preflight fingerprint 比较。

- ignored-only change：不产生 full debt；
- active create/delete：产生 full debt；
- active same-path modification：产生 full debt。

这保留当前 conservative post-sync safety barrier。

---

## 10. 状态机预期变化

### 10.1 当前错误流程

```text
full run target G is active
        |
        +-- ignored .tmp_scripts/x create/modify
                 |
                 +-- source_generation = G+1
                 +-- current_run_id != NULL
                 +-- desired_generation = G+1
                 +-- pending evidence deleted

full run G succeeds
last_success_generation = G
desired_generation = G+1
        |
        +-- worker claims another full run G+1
```

### 10.2 修复后 ignored safe event

```text
full run target G is active
        |
        +-- ignored .tmp_scripts/x create/modify
                 |
                 +-- source_generation = G+1
                 +-- desired_generation remains G
                 +-- pending_event(path=x, generation=G+1)

full run G succeeds
last_success_generation = G
desired_generation = G
pending G+1 survives target-bound clear
        |
        +-- no full debt
        +-- worker fast-event pass
                 |
                 +-- committed policy says excluded
                 +-- exact-version clear
                 +-- no upload
                 +-- no new full run
```

### 10.3 修复后 active safe event

```text
full run target G is active
        |
        +-- active notes/a.md modify at G+1
                 |
                 +-- durable pending G+1

full run completes
        |
        +-- if metadata final scan sees change:
        |      conservative full dirty debt may be created
        |
        +-- otherwise pending survives and targeted fast-upsert repairs it
```

本计划不要求进一步优化 active-change-during-full 的 conservative follow-up full；优先消除 ignored-only churn 的无效 full。

---

## 11. 测试计划

### 11.1 `internal/state/events_test.go`

新增以下矩阵测试。

#### T1 initialized + idle + safe modify

初始：

```text
last_success = 100
desired = 100
current_run = NULL
```

记录 safe modify：

预期：

```text
source_generation = 101
desired = 100
pending(path, modify, 101) exists
no full debt
```

#### T2 active full + ignored-or-unknown-policy safe modify

state 层不判断 ignore，只验证 scheduling：

```text
last_success = 100
desired = 100
current_run = run-100
```

safe modify -> generation 101。

预期：

- desired 仍 100；
- pending 101 存在；
- current run 不变；
- 不删除已有 detailed pending evidence。

#### T3 full debt pending + safe modify

```text
last_success = 100
desired = 105
current_run = NULL
```

safe modify -> source 106。

预期：

- desired 仍 105；
- pending generation 106 存在。

#### T4 destructive event

对 delete / rename / other：

预期：

- desired 推到最新 source generation；
- reconcile debounce 设置；
- full debt 存在；
- 旧 pending 可被 collapse。

#### T5 unknown kind fail closed

`kind="mystery"`, `full=false` 仍必须 promotion。

#### T6 bootstrap fallback

```text
last_success = NULL
desired = 0
current = NULL
```

safe create 必须建立 initial/full debt，而不是只留下 fast event。

#### T7 initial run active + safe modify

```text
last_success = NULL
desired = G
current = initial-run
```

safe modify G+1：

- 不推进 desired；
- pending G+1 保留；
- initial run 成功后可由 fast path 补齐。

### 11.2 `internal/cli/fastupsert_test.go`

#### T8 excluded-only exact clear

1. pending snapshot 含 generation 101 ignored path；
2. classification 后模拟同 path 新事件 generation 102；
3. 清理 snapshot；
4. generation 102 必须仍存在。

禁止再用 profile-wide `ClearPending()` 通过测试。

#### T9 mixed eligible + ignored

pending：

```text
a.md               eligible
.tmp_scripts/x     excluded
```

预期：

- 只 FastUpsert `a.md`；
- `a.md` ledger 更新；
- `.tmp_scripts/x` exact consumed；
- batch 后两条 snapshot event 都不残留。

#### T10 ignored safe event after full target

完整状态机测试：

1. claim full target G；
2. `RecordEvent(.tmp_scripts/x, modify, full=false)` -> G+1；
3. assert desired 仍 G；
4. CommitRunSuccess(G)；
5. assert no `HasDebt()`；
6. pending G+1 仍存在；
7. fast batch with committed ignore snapshot；
8. assert no `FastUpsert` remote call；
9. pending cleared；
10. subsequent `ClaimRun()` == `ClaimNoDebt`。

这是本次 bug 的核心 regression test。

#### T11 active safe event after full target

同上，但 path eligible：

- full success 后无由 RecordEvent 单独制造的 full debt；
- fast batch targeted upload 该 path；
- manifest upsert；
- pending exact clear。

### 11.3 `internal/sync` tests

#### T12 `.tmp_scripts/` directory exclusion

用真实 committed snapshot：

```gitignore
.tmp_scripts/
```

source：

```text
notes/a.md
.tmp_scripts/a.py
.tmp_scripts/result.json
```

`ScanActiveEntries/Paths` 必须只返回 `notes/a.md`。

#### T13 metadata fingerprint same-path size change

同一 path 内容从 1 byte -> 2 bytes，fingerprint 必须变化。

#### T14 metadata fingerprint same-size mtime change

同一路径、同 size，但 mtime 改变，fingerprint 必须变化。

#### T15 ignored churn fingerprint stable

仅修改 `.tmp_scripts/result.json`：active fingerprint 必须不变。

#### T16 active create/delete fingerprint changes

新增或删除 active path，fingerprint 必须变化。

### 11.4 concurrency / race tests

至少增加一个 DB-level deterministic test 验证 exact clear：

```text
worker snapshot event G
watcher upsert same path G+1
worker ClearPendingEvents(snapshot G)
=> G+1 survives
```

若现有测试框架适合，`go test -race ./...` 也应纳入验收。

---

## 12. 实施顺序

### Phase 1 — 固定 state event semantics

修改 `internal/state/events.go`：

- event kind 显式分类；
- safe create/modify 不再因 `current.Valid` / `desired>last` 自动 promotion；
- 保留 bootstrap fallback；
- destructive/unknown fail closed；
- 增加 state regression tests。

完成标准：核心 T1-T7 全过。

### Phase 2 — 修复 fast consumer exact clearing

修改 `internal/cli/fastupsert.go`：

- eligible / discardable 分类；
- excluded-only 不再 `ClearPending(profile)`；
- mixed batch 同步清理 discardable snapshot；
- 增加 T8-T11。

完成标准：ignored event 能在 full 成功后无远端 I/O 地被消费，新到事件不会被旧 snapshot 清掉。

### Phase 3 — 恢复 metadata source-stability fingerprint

修改：

- `internal/sync/policy.go`
- `internal/sync/reconcile.go`
- 必要时复用 / 扩展 `ScanEntry`

增加 T12-T16。

完成标准：ignored churn 不影响 fingerprint，active same-path metadata change 能被检测。

### Phase 4 — integration / full suite

执行至少：

```bash
go test ./...
go test -race ./...
```

如果 repo 有既有 lint / build / e2e 命令，一并执行。

重点确认：

- async profile add 行为不回归；
- worker ownership / lease 不回归；
- policy refresh / prune 不回归；
- delete budget 不回归；
- watcher restart durable pending 不回归；
- worker restart orphan run recovery 不回归。

---

## 13. 现场复现与验收脚本

实现后在专用测试 profile `example-profile` 上做一次最小验证。

### 13.1 前置条件

确认：

```bash
./bin/knowledge-sync profile ignore status example-profile
```

应显示：

```text
current disk snapshot: clean
refresh state: ready
```

并确认 `.tmp_scripts/` 在 committed `.gitignore`。

### 13.2 在 full run 期间制造 ignored churn

在一个 full reconcile 正在 planning/uploading 时：

```bash
mkdir -p <source>/.tmp_scripts
printf 'x' > <source>/.tmp_scripts/ignored-churn-test.tmpdata
printf 'y' >> <source>/.tmp_scripts/ignored-churn-test.tmpdata
```

注意测试文件扩展名本身不要依赖其他 ignore rule，确保是由 `.tmp_scripts/` 目录规则排除。

### 13.3 预期数据库状态

在事件发生后、full run 尚未结束时，应允许看到：

```text
source_generation > desired_generation
current_run_id != NULL
pending_events contains .tmp_scripts/ignored-churn-test.tmpdata
```

当前 full run 成功后：

```text
desired_generation == last_success_generation
current_run_id = NULL
```

随后 worker fast pass 消费 ignored event：

```text
pending_events no matching row
no follow-up full run solely for that generation
```

### 13.4 run history 验收

错误版本会出现：

```text
full G succeeds
5s later full G+1 claimed
```

修复版本在只有 ignored safe churn 时必须变成：

```text
full G succeeds
no redundant full G+1
ignored pending consumed by fast-event pass
```

---

## 14. 可观测性建议

本次不要求 schema 变更，但建议补 worker debug log，方便以后快速区分：

```text
fast event consume: profile=... path=... generation=... reason=excluded
fast event consume: profile=... path=... generation=... reason=stale
fast event upload:  profile=... path=... generation=...
event promote full: profile=... path=... kind=rename generation=...
```

不要默认逐文件 INFO 打爆日志；可以：

- batch summary 默认 INFO；
- per-path 放 DEBUG / verbose。

batch summary 示例：

```text
fast event batch example-profile: pending=7 eligible=2 excluded=4 stale=1
```

这样下一次看到 generation 变化时，不必靠 manifest mtime 反推事件来源。

---

## 15. 不建议的替代方案

### 15.1 不要在 watcher 侧直接 drop ignored event

原因：

- watcher policy cache 可能 stale；
- policy change 后路径可能重新 eligible；
- 会破坏 durable fact producer 原则。

### 15.2 不要先 claim full 再判断“是不是 ignored-only”

当前 promotion 已经删除 path evidence；即使额外保留 evidence，claim 后才取消 full 也让调度模型更复杂：

- 需要证明所有 debt 都来自 ignored path；
- 需要处理 policy generation 变化；
- 需要处理 destructive / rename / overflow；
- 容易与 delete/suppression chronology 冲突。

更好的 cut point 是：safe event 一开始就不要错误进入 full-debt lane。

### 15.3 不要让 `files_discovered` 包含 ignored path 后再靠 upload 过滤

当前正确模型是 `ScanActivePaths` 同时作为 files-from active authority；保持这一点。

现场已经验证 `.tmp_scripts` 不在 manifest，说明这条链路无需重写。

---

## 16. 后续可选优化：ignored destructive churn

本次 V1 仍让 `delete` / `rename` / `other` conservative full。

某些应用通过 atomic rename 写文件，即使在 ignored 目录内，也可能产生 rename/delete 类型事件，因此 V1 不能保证消除所有 ignored-directory churn。

如后续仍观察到 `.tmp_scripts` 因 atomic rename 产生 full reconcile，可单独设计 V2：

1. 保留 destructive event 的 path + observed policy hash/generation evidence；
2. worker 在 owned committed policy 下判断事件路径是否“可证明处于 ignored namespace”；
3. 同时检查 managed ledger 状态，确保不是 active managed object 的真实删除；
4. policy hash 不一致或 evidence 不完整时 fail closed -> full；
5. 只有证明安全时才 consume destructive ignored event。

这个优化必须单独 review，不能与本次 safe-event 修复一起偷渡，因为它会接触 delete/suppression safety boundary。

---

## 17. Definition of Done

以下条件全部满足才算完成：

- [ ] watcher 不新增 ignore drop gate。
- [ ] safe `create/modify` 在 active full run 期间只增加 source generation + durable pending event，不推进 full desired generation。
- [ ] safe `create/modify` 在已有 full debt 时仍保留 path evidence。
- [ ] destructive / uncertain / unknown event 仍 fail closed 到 full reconcile。
- [ ] bootstrap profile 不会因为只收到 safe event 而跳过 initial full baseline。
- [ ] excluded-only fast batch 使用 exact-version clear，不再 profile-wide `ClearPending()`。
- [ ] mixed fast batch 不永久遗留 ignored/stale snapshot events。
- [ ] 同路径更高 generation 新事件不会被旧 fast batch 清掉。
- [ ] `.tmp_scripts/` safe churn 在 full run 中不会单独制造下一轮 full run。
- [ ] ignored churn 不改变 active metadata fingerprint。
- [ ] active same-path size/mtime change 能改变 fingerprint。
- [ ] full test suite 通过。
- [ ] race tests 通过。
- [ ] 专用测试 profile `example-profile` 验证不再复现“full 成功 5 秒后仅因 ignored safe churn 再 claim full”的模式。

---

## 18. 推荐 commit 拆分

建议分成 3 个逻辑 commit，便于 review / bisect：

1. `fix(state): keep safe watcher events out of full reconcile debt`
   - `events.go`
   - state tests

2. `fix(worker): consume ignored fast events with exact-version clearing`
   - `fastupsert.go`
   - fastupsert tests

3. `fix(sync): restore metadata-aware active source fingerprints`
   - `policy.go`
   - `reconcile.go`
   - sync tests

不要把 V2 ignored destructive-event optimization 混入以上 commit。

---

## 19. 最终目标状态

修复后的核心设计应该明确区分三件事：

```text
filesystem fact order       = source_generation
full authoritative debt     = desired_generation > last_success_generation
safe targeted work          = pending_events
```

这三个概念不再被强制绑成同一个 generation lane。

因此：

```text
ignored safe churn
    -> durable fact
    -> worker policy recheck
    -> no-op consume
```

而不是：

```text
ignored safe churn
    -> full debt
    -> 18k-file reconcile
```

这既保留 watcher 不丢事实的原始安全设计，也消除了当前状态机把 ignored transient work 放大成 full reconciliation 的主要性能问题。
