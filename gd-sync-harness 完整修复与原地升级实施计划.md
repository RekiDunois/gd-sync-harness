# gd-sync-harness 完整修复与原地升级实施计划

## 0. 文档目的

这是一个 **implementation-ready plan**。

它针对当前 `gd-sync-harness` 的实际代码和已经存在的真实部署，目标不是重新设计系统，而是修复已经确认的问题，并安全地把现有机器从旧实现升级到新实现。

本计划已经锁定的核心行为包括：

```text
1. 正常 rclone 同步不再有 10 分钟 / 30 分钟总运行时限。

2. knowledge-sync 不因为“看起来很久没动”主动 kill rclone。

3. 30 分钟没有 measurable progress 只显示 possible stall。

4. status 要能解释同步到底在 scan / compare / transfer / finalize 哪个阶段。

5. rclone 无法明确分类的错误：
   最多自动 retry 3 次：
   1m -> 5m -> 15m
   再失败则 terminal。

6. rclone 明确 temporary error 不受上述 3 次限制，
   继续使用 durable retry/backoff。

7. watcher 不再把 directory path 当普通文件传给 --files-from。

8. fast debounce：
   quiet 3s
   max delay 30s

9. destructive debounce：
   quiet 10s
   max delay 60s
   由 worker 统一调度，
   watcher 不再每个 event 起 goroutine sleep。

10. 一旦升级成 full reconcile，
    不再保存几万条 detailed pending paths。

11. full reconcile 运行期间的新变化只推进 generation，
    不启动 competing fast upload。

12. watcher 启动要做 local catch-up，
    恢复停机期间可能漏掉的文件变化。

13. launchd reconcile job 的身份仍叫 reconcile，
    但实际 CLI argv 必须是 reconcile-scheduled。

14. launchd install/update 必须真的 reload 已加载 job。

15. 现有机器已经有真实 profile、真实 SQLite 和已加载 launchd，
    本次是原地升级，不是 fresh install。
```

除非实际代码证明存在不可兼容的硬冲突，否则 agent 不要重新讨论这些决定。

---

# 1. 开始前必须阅读

在修改代码前阅读：

```text
AGENTS.md

harness-plan(2).md

knowledge-sync-async-initial-sync-modification-plan(1).md
```

特别遵守：

```text
generation state
    = 唯一 durable full-reconcile intent

sync_runs
    = execution history / attempts
    != 第二套 durable queue

V1
    = 单 worker runtime owner

每个 profile
    = 最多一个 active reconciliation run

普通 filesystem events
    = 不能清 terminal error gate

knowledge-sync sync <profile>
    = 显式 reopen / retry request
    = 不在 CLI 进程里启动 competing transfer
```

---

# 2. 安全规则

仓库可能公开。

不要把当前真实环境中的任何内容提交进仓库，包括：

```text
真实 profile 名
真实 rclone remote 名
真实本地目录
真实用户名
真实 Google 账号
真实 folder ID
真实 profile UUID
真实 OAuth 信息
真实日志
真实文件名
真实知识库内容
真实 pending 数量/incident 内容
```

所有测试使用：

```text
t.TempDir()
synthetic profile
synthetic remote
synthetic file names
fake rclone
fake fswatch
fake launchctl
temporary SQLite
```

不要把真实部署数据复制成“稍微改一点”的 fixture。

---

# 3. 这是已有部署的原地升级

## 3.1 必须假设当前机器已经存在

当前机器可能已经有：

```text
真实 profile
真实 profile_sync_state
真实 manifest
真实 pending_events
真实 terminal error
真实 retry state
真实 SQLite migration history
```

同时可能已经 loaded：

```text
global worker LaunchAgent

每个 active profile：
    watcher LaunchAgent
    scheduled reconcile LaunchAgent
```

还可能有正在运行的：

```text
旧 knowledge-sync worker
旧 watcher
旧 rclone subprocess
```

不能把系统当成空环境。

---

# 4. 开工前先 inventory，禁止修改

agent 第一阶段先做只读检查。

## 4.1 Git

```bash
git status --short
```

不要：

```text
reset
checkout .
clean
```

覆盖用户已有修改。

---

## 4.2 当前 profile / DB 状态

只读检查：

```text
profile 数量
enabled 状态
initialized 状态
current sync state
current phase
current run
pending 数量
desired generation
last success generation
retry classification
terminal gate
schema migration version
```

真实值只用于本机诊断。

不要写进：

```text
source
test
docs
commit
```

---

## 4.3 launchd inventory

检查：

```text
实际存在多少 LaunchAgent
哪些 loaded
哪些 running
```

不要假设“只有一个 launchd”。

当前设计可能存在：

```text
1 global worker

N profile watcher

N scheduled reconcile
```

记录本机临时信息：

```text
label
loaded status
PID
ProgramArguments
binary path
plist path
```

这些记录不要提交 Git。

---

# 5. 单元测试必须与真实部署完全隔离

在开发阶段：

```text
go test ./...
go test -race ./...
```

不能碰：

```text
真实 knowledge-sync.sqlite
真实 rclone.conf
真实 Drive
真实 LaunchAgents
真实 HOME state
```

测试需要：

```text
temporary DB
temporary HOME/state dirs
fake subprocesses
dependency injection
```

如果任何现有测试会通过默认路径打开真实 DB，先修 test isolation。

---

# 6. rclone 总运行时间问题

主要文件：

```text
internal/exec/backend.go
internal/exec/rclone.go
internal/exec/rclone_test.go
internal/cli/root.go
internal/cli/worker.go
```

当前存在两个错误的 wall-clock kill limit：

```text
Rclone wrapper:
    10 minutes

App.Context:
    30 minutes
```

必须处理。

---

# 7. 删除正常同步的固定 wall-clock timeout

## 7.1 rclone wrapper

当前 `NewRclone()` 默认有一个 10 分钟 timeout。

删除这种语义。

正常：

```text
full reconciliation
initial synchronization
large transfer
large verification
```

不能因为总运行时间：

```text
10m
30m
6h
```

而由 knowledge-sync 主动停止。

---

## 7.2 short control operation 仍可 timeout

例如：

```text
rclone config file discovery
small health probe
```

仍然可以由 caller 显式：

```go
context.WithTimeout(...)
```

例如现有 config discovery 的 15 秒 timeout 可以保留。

原则：

```text
short control-plane operation
    -> explicit short timeout

long data-plane operation
    -> no arbitrary total-duration timeout
```

---

# 8. App.Context 重构

当前通用：

```go
App.Context()
```

统一返回 30 分钟 timeout。

不要继续让所有操作共享一个 global 30m cap。

推荐：

```text
long operation:
    context.WithCancel(...)
    或继承 worker lifecycle context

short operation:
    显式 context.WithTimeout(...)
```

审计所有调用点。

确认：

```text
full reconcile
initial sync
verify
large rclone command
```

不会再被 30m 杀掉。

---

# 9. rclone 仍然负责 transport timeout

不要重新实现 rclone 的网络 watchdog。

rclone 自己已经有：

```text
connection timeout
I/O idle timeout
low-level retries
retries
backend retry behavior
```

knowledge-sync 只负责：

```text
执行
状态观测
结果分类
durable retry orchestration
```

特别注意：

```text
rclone --timeout
```

不是整个命令 wall-clock runtime limit。

它是 I/O idle timeout 概念。

---

# 10. rclone progress 改成机器可读 JSON

主要文件：

```text
internal/exec/rclone.go
internal/exec/rclone_test.go
```

当前解析：

```text
--progress
Transferred:
```

这种人类可读输出。

必须停止作为 progress authority。

---

## 10.1 使用 JSON logging

长 rclone operation 使用：

```text
--use-json-log
--stats 10s
--stats-log-level NOTICE
```

逐行读取 JSON Lines / NDJSON。

普通 JSON log：

```text
保留到日志/错误缓冲
```

包含 structured stats 的记录：

```text
转换成 ProgressStats
```

---

# 11. ProgressStats

至少支持：

```text
bytes
totalBytes

checks
totalChecks

transfers
totalTransfers

listed

deletes

errors

speed

current item
current item bytes
current item total size
```

rclone 某些 command / backend / version 不一定给全部字段。

因此：

```text
unknown != 0
```

需要有清晰 presence / unknown 语义。

不要 fabricate total。

---

# 12. heartbeat 与 progress 必须分开

新增概念：

```text
last_heartbeat_at
last_progress_at
```

## heartbeat

每次收到合法 structured stats：

```text
last_heartbeat_at = now
```

表示：

```text
rclone 还在输出 telemetry
```

---

## progress

只有 measurable work 前进才更新：

```text
bytes 增加
checks 增加
transfers 增加
listed 增加
delete / rename 工作增加
current item 明显推进或切换
```

以下不能算 progress：

```text
elapsed time 增加
同一个 stats frame 重复
error 数增加
```

否则错误循环会一直假装“正在进展”。

---

# 13. possible stall

锁定规则：

```text
连续 30 分钟没有 measurable progress
```

显示：

```text
Warning: possible stall
```

但：

```text
不 cancel
不 kill
不自动 retry
```

如果：

```text
heartbeat 仍 fresh
progress 很旧
```

显示：

```text
rclone responsive
possible stall
```

如果 heartbeat 也很旧：

```text
last heartbeat 31m ago
```

如实显示。

同样：

```text
不 kill
```

---

# 14. context cause 必须保留

当前 `exec.CommandContext` kill 后可能最后只看到：

```text
signal: killed
```

不能靠这个字符串分类。

如果 subprocess 是因为：

```text
context.Canceled
context.DeadlineExceeded
```

被停止，上层必须能：

```go
errors.Is(err, context.Canceled)
errors.Is(err, context.DeadlineExceeded)
```

识别。

可以使用：

```text
typed wrapper
errors.Join
WithCancelCause
```

或等价方案。

禁止：

```go
strings.Contains(err.Error(), "signal: killed")
```

作为 correctness logic。

---

# 15. progress DB migration

主要文件：

```text
internal/state/migrate.go
internal/state/runs.go
internal/state/syncstate.go
```

只追加 migration。

不要重写已经发布的 migration。

建议扩展 `sync_runs`：

```text
last_heartbeat_at TEXT

checks_completed INTEGER NOT NULL DEFAULT 0
checks_total INTEGER NOT NULL DEFAULT 0

items_listed INTEGER NOT NULL DEFAULT 0

errors_count INTEGER NOT NULL DEFAULT 0

speed_bytes_per_second REAL NOT NULL DEFAULT 0

current_item TEXT

current_item_bytes INTEGER NOT NULL DEFAULT 0
current_item_size INTEGER NOT NULL DEFAULT 0
```

现有：

```text
files_discovered
files_completed
bytes_total
bytes_completed
last_progress_at
```

继续使用。

不要建立另一套 progress queue/table，除非实际 migration 需求证明必要。

---

# 16. local scan 也必须可观察

full reconcile 的长时间阶段不一定都在 rclone。

例如：

```text
扫描 4 万个文件
```

本身可能很慢。

给 scanner 增加 optional progress callback，例如概念上：

```text
ScanLocalProgress(...)
```

原：

```text
ScanLocal(...)
```

可以作为 wrapper，传 nil callback。

扫描 progress 要：

```text
按时间节流
```

例如约 1 秒更新一次。

不要每扫一个文件都写 SQLite。

---

# 17. reconciliation phase 必须真实

主要文件：

```text
internal/sync/reconcile.go
internal/cli/worker.go
```

当前 worker 会在真正 preflight 还没结束时就显示：

```text
uploading
```

必须修。

建议实际 phase：

```text
scanning
    local scan

planning
    rclone dry-run / remote comparison

scanning / validating
    second local scan / stability check

uploading / reconciling
    live rclone sync

finalizing
    final scan
    manifest update
    success commit
```

尽量复用现有：

```text
PhaseScanning
PhasePlanning
PhaseUploading
PhaseFinalizing
```

不要为了 UI 增加没有必要的复杂 state。

---

# 18. status UX

主要文件：

```text
internal/cli/profilestatus.go
```

运行时应该能显示类似：

```text
Profile: example
Initialized: no
State: initializing
Phase: planning

Runtime: 18m32s

Local:
  Discovered: 38421

Remote comparison:
  Listed: 25012
  Checked: 13240

Transfer:
  Files: 81
  Data: 20.4 MiB / 4.8 GiB
  Speed: 41 KiB/s

Current:
  docs/example.pdf
  4.1 MiB / 12 MiB

Heartbeat:
  8s ago

Last progress:
  17m ago

Errors:
  0
```

超过 30m 没 progress：

```text
Warning: possible stall
No measurable progress for 30m
```

unknown total：

```text
calculating
```

不要显示虚构百分比。

---

# 19. retry / error 分类

主要文件：

```text
internal/cli/worker.go
internal/state/retry.go
internal/state/syncstate.go
```

优先使用：

```text
typed project error
context cause
rclone exit code
```

禁止通过错误字符串猜类别。

---

# 20. rclone exit code policy

至少：

```text
0
    success

2
    syntax / usage
    terminal

5
    temporary error
    retryable

6
    NoRetry-class error
    terminal / no automatic retry

7
    fatal
    terminal

1
    uncategorized
    limited automatic retry
```

exit 3 / 4：

```text
根据 operation context 分类
```

例如 ownership marker 确认不存在：

```text
terminal safety failure
```

但临时 API 故障导致无法读取：

```text
retryable
```

exit 8 / 10：

```text
harness 本身不应设置 max-transfer/max-duration
```

如果外部配置导致出现：

```text
明确 configuration/limit failure
```

不要无限 retry。

---

# 21. unknown error 有限 retry

锁定：

```text
original failure

retry #1 after 1m

retry #2 after 5m

retry #3 after 15m

still fails
    -> terminal
```

最大连续失败：

```text
1 original + 3 automatic retry
```

之后：

```bash
knowledge-sync sync <profile>
```

重新开放一轮。

---

# 22. temporary retry 不受 3 次限制

如果 rclone 明确：

```text
exit code 5
```

继续使用 normal durable exponential backoff。

不要把：

```text
temporary
```

和：

```text
unknown limited retry
```

塞进同一个简单 bool。

推荐 structured classification：

```text
retryable
retryable_limited
terminal
```

或者等价独立 policy 字段。

---

# 23. ownership validation 分类

主要文件：

```text
internal/sidecar/*
internal/cli/worker.go
```

必须拆开两类：

## 真正 ownership invalid

例如：

```text
sidecar 确认不存在
UUID mismatch
folder ID mismatch
remote mismatch
metadata malformed
```

处理：

```text
terminal
fail closed
绝不执行 destructive sync
```

---

## ownership temporarily unverifiable

例如：

```text
API temporary failure
rclone exit 5
transport temporary failure
```

处理：

```text
本次仍然绝不 destructive
但允许 retry
```

使用 typed errors。

不要让 worker parse：

```text
"ownership failed"
```

字符串。

---

# 24. generation 模型修复

这是核心 correctness fix。

主要文件：

```text
internal/state/runtime.go
internal/state/syncstate.go
internal/watch/watcher.go
internal/sync/reconcile.go
internal/cli/sync.go
```

当前会：

```text
filesystem event
    -> source_generation + 1

RequestReconcile()
    -> desired_generation + 1
```

这两个不是同一个坐标系。

重复 scheduling 会制造：

```text
不存在的 generation debt
```

必须修。

---

# 25. filesystem generation 成为统一版本

事件：

```text
source_generation += 1
```

得到：

```text
gen = 27
```

如果该事件需要 full reconciliation：

```text
desired_generation =
    max(desired_generation, gen)
```

不要：

```text
desired_generation++
```

新增概念 API，例如：

```text
EnsureReconcileGeneration(profileID, generation)
```

必须：

```text
idempotent
```

同一 generation 请求 10 次：

```text
只欠一次
```

---

# 26. manual sync 是特殊 control-plane 操作

用户：

```bash
knowledge-sync sync <profile>
```

即使没有 filesystem event，也要制造新的 reconciliation debt。

因此 manual path：

```text
reopen terminal/retry gate

desired_generation
    至少 last_success_generation + 1

clear/bypass automatic destructive debounce
```

不要继续复用一个：

```text
RequestReconcile() = unconditional +1
```

API 来表达所有情况。

---

# 27. full run 期间发生事件

例如：

```text
run target = 20
```

运行期间：

```text
source generation -> 27
desired generation -> 27
```

当前 run 成功：

```text
last success generation = 20
```

然后：

```text
下一 run target 直接 = 27
```

不能因为发生 7 个 event：

```text
跑 7 次 full reconcile
```

---

# 28. pending event race 修复

主要文件：

```text
internal/state/events.go
internal/watch/watcher.go
internal/cli/watch.go
```

当前 path-based clear 存在 race：

```text
a.md generation 10
进入 fast batch

上传期间
a.md generation 11 到达

上传 generation 10 成功

ClearPendingPaths("a.md")
```

这样可能把 generation 11 一起删掉。

必须 generation-aware clear。

---

# 29. fast clear 必须带 generation

例如：

```text
ClearPendingEvent(
    profile,
    path,
    throughGeneration,
)
```

SQL：

```text
DELETE pending_events
WHERE profile_id = ?
  AND path = ?
  AND source_generation <= ?
```

batch snapshot 必须保存：

```text
path
generation
```

不能只保存 path string。

---

# 30. full success 也不能 ClearPending(profile)

当前 full success 后全清 pending 会删除运行期间来的新 event。

改为：

```text
ClearPendingThroughGeneration(
    profile,
    run.TargetGeneration,
)
```

任何：

```text
event generation > run target
```

必须留下。

---

# 31. full success transaction 顺序

正确顺序：

```text
remote sync success

final local scan success

manifest refresh success

DB transaction:
    commit run success(target_generation)
    clear pending <= target_generation

如果 desired > target
    state remains syncing
else
    ready
```

不要：

```text
先 clear pending
后 commit success
```

---

# 32. fswatch 改为 structured numeric event

主要文件：

```text
internal/watch/watcher.go
internal/watch/watcher_test.go
```

当前只：

```text
fswatch -0 -r
```

输出 path。

然后 `os.Stat()` 猜 event type。

必须换成 machine-readable path + numeric flags。

---

# 33. 推荐 fswatch record format

根据当前安装版本文档确认后，实现类似：

```text
-0
-r
-n
--format=%p%0%f
```

目标输出：

```text
path \0 flag \0
path \0 flag \0
```

不要用空格分割：

```text
path flags
```

因为 filename 可以有空格。

---

# 34. fswatch event constants

以当前安装 fswatch 的官方文档为最终依据。

本计划预期要处理：

```text
Created
Updated
Removed
Renamed
OwnerModified
AttributeModified
MovedFrom
MovedTo
IsFile
IsDir
IsSymLink
Link
Overflow
```

不要散落 magic numbers。

定义 named constants。

---

# 35. event 分类规则

## regular file create/update/moved-to

如果：

```text
当前是 regular file
filter eligible
```

进入 fast path。

---

## file delete/rename/moved-from

```text
full reconcile
```

fast path 永远不 remote delete。

---

## directory structural changes

以下目录事件：

```text
Created
Removed
Renamed
MovedFrom
MovedTo
```

全部：

```text
full reconcile
```

原因：

```text
用户可能一次拖入一个装着几千文件的目录
```

不能赌 child event 全部到齐。

---

## directory Updated / AttributeModified only

不要直接 full reconcile。

否则普通文件修改可能伴随 parent directory metadata event，导致 reconcile storm。

---

## Overflow

```text
event stream unreliable
-> full reconcile
```

---

## symlink

遵守已有 policy：

```text
do not follow symlink
```

不要 fast upload target。

---

## uncertain event

只要不能安全证明：

```text
这是普通 existing regular file create/update
```

就：

```text
full reconcile
```

---

# 36. fast debounce

锁定：

```text
quiet = 3s
max delay = 30s
```

当前算法错误。

必须保存：

```text
first_event_at
last_event_at
```

触发：

```text
now - last_event_at >= 3s

OR

now - first_event_at >= 30s
```

不能拿：

```text
last batch fire time
```

判断 quiet。

---

# 37. batch 执行期间不能丢新 event

当前不能：

```text
取 map reference
unlock
fire batch
重新 map = empty
```

正确做法：

```text
lock

判断当前 trigger window due

原子切换 / snapshot 当前 trigger window

reset new trigger window

unlock

处理旧 snapshot
```

期间来的新 event 会进入新 window。

DB pending 才是 durable authority。

内存 debounce 只是 timing。

---

# 38. backlog promotion

保留原设计：

```text
pending > 500

OR

pending > 5% known manifest

OR

destructive event

OR

overflow / uncertain

-> full reconciliation
```

当前代码缺 5% relative threshold，要补。

---

# 39. promotion 必须原子

新增 DB operation，例如：

```text
PromoteToFullReconcile(...)
```

一个短 transaction 完成：

```text
desired generation 提升到 source generation

legacy reconcile_requested = 1

collapse 不再需要的 detailed pending

设置 destructive debounce timing（如适用）

更新 public sync state

绝不清 terminal gate
```

禁止：

```text
ClearPending()
进程 crash
RequestReconcile()
```

这种非原子顺序。

---

# 40. full mode 后不再积累几万 pending

一旦：

```text
authoritative full debt 已存在
```

普通 event：

```text
source_generation +1
desired_generation = max(...)
```

不再为了 correctness 保存每个 detailed path。

尤其：

```text
full reconcile active
```

期间不能继续形成 fast queue storm。

---

# 41. destructive debounce

锁定：

```text
quiet = 10s
max delay = 60s
```

删除当前：

```text
每个 event:
    goroutine
    sleep 10s
    runReconcile()
```

watcher 不再直接执行 delayed reconciliation。

---

# 42. durable destructive timing

建议 `profile_sync_state` 增加：

```text
reconcile_not_before_at
reconcile_deadline_at
```

第一 destructive event：

```text
not_before = now + 10s
deadline = now + 60s
```

之后 destructive event：

```text
not_before =
    min(now + 10s, deadline)

deadline 不变
```

结果：

```text
安静 10 秒开始

如果一直变化
最迟 60 秒开始
```

---

# 43. worker claim gate

automatic worker：

```text
debt exists

but now < not_before

and deadline not reached
```

返回：

```text
deferred
```

不是：

```text
failure
```

推荐明确：

```text
ClaimDeferred
```

不要滥用 retry/error gate。

---

# 44. manual bypass debounce

明确用户操作：

```text
knowledge-sync sync <profile>

knowledge-sync reconcile-now <profile>
```

绕过：

```text
10s / 60s automatic debounce
```

automatic：

```text
watch-triggered
worker normal
scheduled reconciliation
```

尊重 debounce。

不要让单一：

```go
force bool
```

同时表达：

```text
ignore debt
bypass debounce
```

两个不同概念。

必要时改成 options struct。

---

# 45. watcher startup catch-up

watcher 每次启动：

```text
先启动 fswatch 并开始 durable 接收 event

然后执行 local catch-up scan
```

不要：

```text
先 scan
后启动 watcher
```

否则 scan 和 watcher startup 之间存在漏 event 窗口。

重复 event 没关系。

丢 event 才是 correctness 问题。

---

# 46. startup catch-up decision

本地 scan 与 manifest 比较：

## 少量 create/modify

```text
恢复 fast pending
```

## delete

```text
full reconcile
```

## 大量变化

```text
full reconcile
```

## uninitialized profile / 已有 full debt

不要重新生成几万 detailed pending。

只确保 full debt 覆盖当前 source generation。

## scan 无法完成

```text
保留已有 durable intent
不要假装 catch-up success
```

---

# 47. profile cross-process writer lock

主要文件：

```text
internal/flock/*
internal/cli/root.go
internal/cli/watch.go
internal/cli/worker.go
internal/cli/syncnow.go
internal/cli/reconcile.go
```

项目已有 PID-aware recoverable cross-process profile lock。

必须真正把它放到：

```text
remote mutation boundary
```

---

# 48. 必须受同一把锁保护的入口

至少：

```text
watcher fast upsert

manual sync-now

manual reconcile-now

scheduled reconciliation

worker reconciliation

migration remote operation
```

同一个 profile 的 Drive writer 不能互相竞争。

---

# 49. worker 获取锁顺序

不要：

```text
先 ClaimRun()
再发现 profile lock 被 fast upload 占着
```

这样会制造：

```text
running run
```

但实际没运行。

应该：

```text
try acquire profile writer lock

如果拿不到：
    本轮 skip
    下次 worker poll 再试

拿到：
    ClaimRun
    execute
    commit
    release
```

`ErrLocked`：

```text
不是 sync error
不是 retry failure
```

---

# 50. watcher fast path 获取同一锁

fast batch：

```text
acquire profile writer lock

重新检查：
    当前是否已有 full debt
    当前是否 full run active

如果 full mode:
    不启动 fast rclone
    ensure desired generation
    保留正确 durable intent

否则:
    rclone copy
    generation-aware pending clear

release
```

如果锁被 worker 占用：

```text
不丢 pending
不 terminal
```

---

# 51. remote-level scheduler 当前必须修

当前 `internal/sched.Scheduler` 只是：

```text
每个 process 自己内存里一份 queue
```

但 watcher 是独立 process。

因此多个 profile 共用同一个 remote 时：

```text
它们彼此看不见
```

不满足原设计。

---

# 52. remote-level scheduler 必须 cross-process

目标：

```text
same remote:
    bounded concurrency
    initial default ~2

waiting fast operation:
    higher priority

waiting full reconciliation:
    lower priority

different independent remotes:
    can run concurrently
```

---

# 53. 推荐 SQLite-backed remote lease

不要把它塞进 `sync_runs`。

可以增加独立资源调度表，例如：

```text
remote_operation_leases

id
remote_name
priority
owner_pid
state
created_at
lease_until
```

流程：

```text
insert waiting operation

短 transaction:
    检查 same remote running count
    检查 priority / FIFO eligibility

claim running lease

transaction commit

执行 rclone

周期性续 lease

完成:
    release/delete
```

crash：

```text
lease expiry
-> recoverable
```

绝不：

```text
持 SQLite transaction 等网络 I/O
```

如果 agent 有更简单、正确且经过测试的 cross-process 方案，可以替换。

但必须满足：

```text
cross-process
bounded same-remote concurrency
fast priority
crash recovery
```

不能继续使用 process-local scheduler 冒充 global scheduler。

---

# 54. launchd command bug

主要文件：

```text
internal/launchd/launchd.go
internal/launchd/launchd_test.go
internal/cli/jobs.go
internal/cli/install.go
```

保留：

```text
JobKind = reconcile
label = ...reconcile
log name = ...reconcile.log
calendar condition uses reconcile
```

实际 ProgramArguments：

```text
knowledge-sync reconcile-scheduled <profile>
```

---

# 55. job identity 与 CLI command 分离

增加 mapping：

```text
JobWatch
    -> watch

JobReconcile
    -> reconcile-scheduled

JobWorker
    -> worker
```

plist 不再直接：

```text
argv[1] = Kind
```

而是：

```text
argv[1] = Command()
```

---

# 56. launchd install/update 必须 reload

当前：

```text
write plist
bootstrap
already loaded -> success
```

不能保证 loaded configuration 更新。

新逻辑：

```text
bootout existing job

write new plist

bootstrap
```

旧 job 不存在：

```text
bootout no-op
```

profile job 和 global worker 都走一致 reload 语义。

---

# 57. 现有 global worker 必须单独考虑

当前：

```text
knowledge-sync install <profile>
```

只处理指定 profile jobs。

不会自动处理：

```text
global worker
```

所以 rollout 时不能只执行：

```text
install <profile>
```

然后以为全系统都升级了。

升级流程必须显式 reload：

```text
global worker
profile watcher
profile reconcile
```

---

# 58. launchd test

必须精确断言：

```text
label 仍然是 .reconcile

ProgramArguments 中：
    command = reconcile-scheduled

calendar interval 仍存在
```

不要只 assert：

```text
plist contains "reconcile"
```

因为 label 自己就含这个词。

---

# 59. launchctl dependency injection

unit tests 不实际调用系统：

```text
launchctl
```

抽象 command runner。

测试：

```text
bootout
write plist
bootstrap
```

顺序。

---

# 60. 旧数据库 migration

新 schema 只能追加 migration。

必须兼容已经存在：

```text
profiles
manifest
pending
sync state
runs
terminal error
```

不能要求：

```text
删除 ~/.local/share/...sqlite
重新 add profile
```

---

# 61. migration rehearsal

真正升级真实 DB 前：

```text
复制真实 SQLite 到 repository 外的临时安全位置
```

在副本运行新版 migration。

验证：

```text
profile count 不变
profile identity 不变
remote identity 不变

initialized_at 不丢

last_success_generation 不倒退

terminal gate 不被清

manifest 不被意外清空

current run recovery 合理

old pending 能正确 promotion/collapse
```

然后在副本运行：

```text
knowledge-sync profile status <profile>
```

确认新代码能读取旧 state。

---

# 62. old large pending upgrade

如果旧 DB 已经有：

```text
大量 pending
```

新版 recovery 不能逐条 fast upload。

如果超过 promotion threshold：

```text
原子 promotion full

collapse detailed pending

desired generation 覆盖 source generation
```

但：

```text
terminal error gate 继续保留
```

migration 不自动 reopen。

---

# 63. orphan run recovery

如果真实旧 DB 启动时残留：

```text
current running run
```

但旧 worker 已经不存在：

使用已有 orphan recovery 语义：

```text
worker_interrupted
retryable
preserve debt
```

不要把 run 直接当 success。

---

# 64. 实施顺序

## Phase A — test isolation / inventory

完成：

```text
真实环境 inventory

确认 tests 不碰真实 deployment

建立 synthetic subprocess test harness
```

---

## Phase B — durable DB correctness

先修：

```text
migrations

generation coalescing

generation-aware pending clear

full-success pending clear

destructive timing fields

ClaimDeferred

limited retry state
```

先把数据库状态机做正确。

---

## Phase C — rclone wrapper

完成：

```text
删除 10m timeout

删除 long-operation 30m timeout

JSON stats

heartbeat/progress

context cause
```

---

## Phase D — reconciliation / errors

完成：

```text
准确 phase

ownership typed errors

exit code classifier

unknown limited retry
```

---

## Phase E — watcher

完成：

```text
numeric fswatch input

event classification

3s / 30s debounce

generation-safe batch clear

promotion/collapse

10s / 60s durable debounce

startup catch-up
```

彻底删除：

```text
watcher delayed reconcile goroutine
```

---

## Phase F — concurrency

完成：

```text
profile cross-process writer lock

cross-process remote scheduler
```

---

## Phase G — launchd

完成：

```text
Command mapping

reconcile-scheduled argv

reliable reload

global worker upgrade semantics
```

---

## Phase H — status

完成：

```text
phase

stats

heartbeat

last progress

possible stall
```

---

## Phase I — regression

完成全部测试。

---

# 65. 单元测试要求

不要通过增加 sleep 修 flaky test。

尽量：

```text
injectable clock
pure timing state
fake subprocess
temporary DB
```

---

# 66. rclone tests

覆盖：

```text
JSON stats parser

bytes
checks
transfers
listed
errors
speed
current item

普通 JSON log 不当 stats

invalid JSON 不让 reader 崩溃

context cancel cause preserved

long command 无默认 10m limit
```

删除旧：

```text
Transferred:
```

人类文本 parser 作为 progress authority 的测试。

---

# 67. retry tests

覆盖：

```text
exit 5 -> retryable

exit 7 -> terminal

exit 2 -> terminal

unknown failure 1:
    next 1m

unknown failure 2:
    next 5m

unknown failure 3:
    next 15m

unknown failure 4:
    terminal

success:
    reset limited failure count

manual sync:
    reopen

filesystem event:
    terminal gate remains closed
```

使用 fake clock。

不要 sleep 真正 15 分钟。

---

# 68. progress/stall tests

覆盖：

```text
valid stats -> heartbeat update

identical stats -> no progress update

bytes increase -> progress

checks increase -> progress

listed increase -> progress

errors only -> not progress

30m no progress -> warning

warning -> does not cancel process
```

---

# 69. fswatch tests

synthetic numeric masks：

```text
file Created
file Updated
file Removed
file Renamed
file MovedFrom
file MovedTo

dir Created
dir Updated
dir Removed
dir Renamed

symlink

Overflow

unknown
```

确认：

```text
directory path 永远不进入 --files-from
```

---

# 70. debounce tests

确定性验证：

```text
last event <3s
    no fire

quiet >=3s
    fire

continuous events <30s
    no force

first event >=30s
    force
```

---

# 71. event-during-batch race test

构造：

```text
batch executing

new event arrives

batch completes
```

断言新 event：

```text
仍然在下一 trigger window / DB pending
```

---

# 72. same-path generation race test

```text
a.md generation 10
fast batch starts

a.md generation 11 arrives

batch 10 success
```

断言：

```text
generation 10 cleared
generation 11 remains
```

---

# 73. promotion tests

覆盖：

```text
501 pending -> full

>5% manifest -> full

delete -> full

overflow -> full

directory structural change -> full
```

promotion：

```text
collapse detailed pending

desired generation correct

repeated promotion idempotent
```

---

# 74. full run generation race test

```text
run target = 10

event -> 11
event -> 12

run 10 success
```

必须：

```text
last success = 10
desired = 12

下一 run target = 12
```

而不是：

```text
run 11
run 12
```

两次。

---

# 75. startup catch-up tests

覆盖：

```text
watcher down:
    small create -> fast
    small modify -> fast
    delete -> full
    large change -> full

uninitialized profile:
    不生成几万 pending
```

---

# 76. profile lock tests

至少证明：

```text
fast writer holds profile lock
worker does not run

worker full writer holds lock
fast writer does not run
```

`ErrLocked`：

```text
不能被记录成 terminal sync failure
```

---

# 77. remote scheduler tests

证明：

```text
same remote active <= configured max

different remote can run concurrently

waiting fast outranks waiting full

expired lease recoverable
```

最好有跨 process integration test。

---

# 78. launchd tests

证明：

```text
label remains reconcile

argv is reconcile-scheduled

calendar unchanged

reload order:
    bootout
    write
    bootstrap
```

---

# 79. build/test commands

至少：

```bash
gofmt -w <changed files>

go vet ./...

go test ./...

go test -race ./...
```

过去 flaky watcher 相关测试：

```bash
go test ./internal/watch -count=50
```

如果测试被移动，执行等价新 package。

最后：

```bash
git diff --check
git diff --stat
git diff
```

再按照 `AGENTS.md` 检查敏感信息。

---

# 80. 在纯代码测试通过前，不碰真实 launchd

开发阶段：

```text
修改
test
race
fake integration
```

不需要停止真实服务。

前提：

```text
测试完全 isolated
```

---

# 81. 第一次真实 rollout：maintenance window

当准备：

```text
新版 binary
+
真实 DB migration
+
真实 profile test
```

时，必须避免旧新 writer 同时工作。

---

# 82. 检查 active run

如果旧系统当前有：

```text
正在正常工作的 reconciliation/rclone
```

不要为了升级随意 SIGKILL。

优先：

```text
等待当前 run 结束
```

除非用户明确要求停止。

---

# 83. 停止所有旧自动 writer

进入 maintenance window：

```text
bootout global worker

bootout target profile watcher

bootout target profile scheduled reconcile
```

如果还有其他 profile 共用同一个 remote，并且本次测试可能影响 global scheduler / shared remote：

```text
考虑把所有相关 remote writers 一并停止
```

之后确认：

```text
没有旧 knowledge-sync writer

没有旧 rclone transfer
```

---

# 84. rollout backup

备份：

```text
旧 knowledge-sync binary

真实 SQLite DB

当前 plist
```

备份放 repository 外。

---

# 85. migration rehearsal 必须先过

用真实 DB 副本跑新版。

通过后才碰真实 DB。

---

# 86. 真实 DB migration

运行新版 binary，让正常 migration framework 升级 DB。

之后先只运行：

```text
knowledge-sync profile status <profile>
```

确认：

```text
profile 正常
initialized 正常
error gate 正常
generation 正常
pending promotion 正常
```

---

# 87. 第一次真实同步不要直接恢复所有 launchd

不要一上来：

```text
install
全部自动启动
```

先手动控制。

---

# 88. explicit sync

如果旧 profile 当前 terminal blocked：

```bash
knowledge-sync sync <profile>
```

作为用户明确 reopen。

不要 migration 自动清 gate。

---

# 89. one-shot / manual worker 验证

在 watcher 仍停止的情况下：

```text
只运行新版 worker
```

最好使用：

```text
worker --once
```

或等价一次性路径。

观察：

```text
status phase
heartbeat
last progress
rclone activity
retry classification
```

---

# 90. 验证旧 10m / 30m bug 已不存在

第一次真实 sync 如果自然运行超过：

```text
10m
```

确认没有被 kill。

如果运行超过：

```text
30m
```

同样确认没有被 knowledge-sync wall-clock kill。

不要求为了测试故意等 30 分钟。

如果真实同步自然超过则观察即可。

unit tests 仍负责严格证明无 cap。

---

# 91. 第一次 full success 后 DB 检查

确认：

```text
manifest > 0

initialized success

last success generation correct

desired generation not artificially inflated

pending does not contain stale huge backlog

newer-generation pending not accidentally cleared
```

---

# 92. 恢复 global worker

使用新版 reload/install path。

之后检查：

```text
launchctl loaded service

binary path

PID

ProgramArguments
```

确认内存里的 service 已经是新版。

不能只看 plist 文件。

---

# 93. 恢复 watcher

再启动 target profile watcher。

用 synthetic 本地文件做测试：

```text
create

modify

delete
```

不要拿真实私人文件做 regression fixture。

验证：

```text
create/modify -> fast

delete -> destructive debounce -> full
```

---

# 94. 验证 10s / 60s

删除测试文件以后：

```text
不要立即 full reconcile
```

应该进入：

```text
10 秒 settle
```

如果持续 destructive burst：

```text
最迟 60 秒 eligible
```

测试可以用 fake clock/unit test严格验证。

真实 rollout 只做 sanity check。

---

# 95. 恢复 scheduled reconcile

最后恢复：

```text
scheduled reconcile LaunchAgent
```

检查：

```text
label:
    ...reconcile

ProgramArguments:
    knowledge-sync
    reconcile-scheduled
    <profile>
```

---

# 96. launchd 必须检查 loaded state

升级成功不能只证明：

```text
plist on disk 正确
```

必须：

```text
launchctl print ...
```

或当前 macOS 等价检查 loaded service。

确认：

```text
ProgramArguments
binary
PID
```

已经变成新版。

---

# 97. rollback

第一次真实 rollout 前必须有 rollback。

如果 migration / binary / launchd 出现严重问题：

```text
停止所有新版 jobs

恢复旧 binary

恢复 migration 前 SQLite backup

恢复旧 plist

重新 bootstrap 旧 jobs
```

不要在已经被新 schema 改过的真实 DB 上手工删列硬退版本。

---

# 98. 官方文档：使用原则

遇到工具行为不确定时：

```text
当前机器 --help / man
    优先

官方文档
    第二

项目设计文档
    结合判断

第三方博客
    只作为最后补充
```

不要凭记忆猜。

---

# 99. rclone 官方文档

## 总文档

[rclone Documentation](https://rclone.org/docs/?utm_source=chatgpt.com)

用于：

```text
global options
logging
stats
timeout
retry
exit codes
```

---

## Global flags

[rclone Global Flags](https://rclone.org/flags/?utm_source=chatgpt.com)

重点：

```text
--use-json-log
--stats
--stats-log-level
--timeout
--contimeout
--retries
--low-level-retries
--max-duration
```

---

## Logging

[rclone Logging Documentation](https://rclone.org/docs/?utm_source=chatgpt.com#logging)

实现 JSON progress 前必须查。

---

## core/stats

[rclone RC core/stats](https://rclone.org/rc/?utm_source=chatgpt.com#core-stats)

设计 `ProgressStats` 的主要官方依据。

---

## Exit codes

[rclone Exit Codes](https://rclone.org/docs/?utm_source=chatgpt.com#list-of-exit-codes)

修改 retry classifier 前重新核对。

---

## Google Drive backend

[rclone Google Drive backend](https://rclone.org/drive/?utm_source=chatgpt.com)

涉及：

```text
Drive API
OAuth
drive.file
Google-specific behavior
```

时查。

---

## rclone sync

[rclone sync command](https://rclone.org/commands/rclone_sync/?utm_source=chatgpt.com)

重点：

```text
sync deletion semantics
dry-run
track-renames
max-delete
```

---

## rclone copy

[rclone copy command](https://rclone.org/commands/rclone_copy/?utm_source=chatgpt.com)

fast upsert 使用。

---

## rclone filtering / files-from

[rclone Filtering](https://rclone.org/filtering/?utm_source=chatgpt.com)

涉及：

```text
--files-from
filters
path semantics
```

时查。

---

# 100. Go 官方文档

## os/exec

[Go os/exec](https://pkg.go.dev/os/exec?utm_source=chatgpt.com)

重点：

```text
CommandContext
Cmd.Start
Cmd.Wait
ExitError
```

特别核对 subprocess cancellation 行为。

---

## context

[Go context](https://pkg.go.dev/context?utm_source=chatgpt.com)

重点：

```text
WithCancel
WithCancelCause
Cause
WithTimeout
Canceled
DeadlineExceeded
```

---

## errors

[Go errors package](https://pkg.go.dev/errors?utm_source=chatgpt.com)

重点：

```text
errors.Is
errors.As
errors.Join
```

---

## sync

[Go sync package](https://pkg.go.dev/sync?utm_source=chatgpt.com)

watcher goroutine / Mutex / Cond 参考。

---

## time

[Go time package](https://pkg.go.dev/time?utm_source=chatgpt.com)

debounce / retry / timestamp 参考。

---

## database/sql

[Go database/sql](https://pkg.go.dev/database/sql?utm_source=chatgpt.com)

transaction correctness 参考。

---

## Race Detector

[Go Race Detector](https://go.dev/doc/articles/race_detector?utm_source=chatgpt.com)

本次必须运行：

```bash
go test -race ./...
```

---

## testing

[Go testing package](https://pkg.go.dev/testing?utm_source=chatgpt.com)

测试隔离、TempDir 等。

---

# 101. launchd / Apple 官方参考

Apple 的 archived launchd guide 用于理解 plist。

当前 macOS `launchctl` 具体行为优先看本机 man page。

---

## Apple launchd job guide

[Apple Creating Launch Daemons and Agents](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html?utm_source=chatgpt.com)

重点：

```text
Label
ProgramArguments
RunAtLoad
KeepAlive
StartCalendarInterval
LaunchAgents
```

---

## Apple Service Management

[Apple Service Management](https://developer.apple.com/documentation/servicemanagement?utm_source=chatgpt.com)

仅作为现代 macOS service-management 背景参考。

本次不要顺便重构到新 framework。

---

# 102. 当前机器 launchctl 文档

必须执行：

```bash
man launchctl
```

重点查：

```text
bootstrap
bootout
print
kickstart
gui/<uid>
domain target
service target
```

不要用老教程中的：

```text
launchctl load
launchctl unload
```

替代当前设计。

---

# 103. 当前机器 launchd.plist

必须执行：

```bash
man 5 launchd.plist
```

重点：

```text
Label
ProgramArguments
RunAtLoad
KeepAlive
StartCalendarInterval
StandardOutPath
StandardErrorPath
```

---

# 104. fswatch 官方文档

## 首页

[fswatch documentation](https://emcrisostomo.github.io/fswatch/doc/?utm_source=chatgpt.com)

先检查本机：

```bash
fswatch --version
fswatch --help
```

---

## Invocation / formatting

[fswatch Invoking fswatch](https://emcrisostomo.github.io/fswatch/doc/1.17.1/fswatch.html/Invoking-fswatch.html?utm_source=chatgpt.com)

重点确认：

```text
-0
--print0
-n
--numeric
--format
%p
%f
%0
```

以及 event flag 行为。

numeric mask 以当前实际安装版本文档为最终依据。

---

# 105. SQLite 官方文档

## Transactions

[SQLite Transactions](https://www.sqlite.org/lang_transaction.html?utm_source=chatgpt.com)

原则：

```text
短 durable transaction
网络 I/O transaction 外
```

---

## ALTER TABLE

[SQLite ALTER TABLE](https://www.sqlite.org/lang_altertable.html?utm_source=chatgpt.com)

migration 时查。

---

## WAL

[SQLite WAL](https://www.sqlite.org/wal.html?utm_source=chatgpt.com)

并发 DB behavior 参考。

---

## UPSERT

[SQLite UPSERT](https://www.sqlite.org/lang_upsert.html?utm_source=chatgpt.com)

pending / generation coalescing 参考。

---

# 106. agent 遇到文档不确定时的规则

agent 完成报告中必须具体写：

```text
哪个设计点

查了哪个官方 URL / man page

最终采用了什么行为
```

例如：

```text
rclone JSON progress:
    verified against:
    https://rclone.org/docs/
    https://rclone.org/rc/#core-stats

Go cancellation:
    verified against:
    https://pkg.go.dev/os/exec
    https://pkg.go.dev/context

launchd reload:
    verified against:
    man launchctl
    man 5 launchd.plist
```

不要只说：

```text
according to documentation
```

---

# 107. 明确禁止的修法

不要：

```text
增加固定 6h total timeout
```

不要：

```text
possible stall -> kill rclone
```

不要：

```text
parse "signal: killed"
```

不要：

```text
继续解析人类 Transferred: progress
```

不要：

```text
让 sync_runs 成为第二套 queue
```

不要：

```text
每个 full request desired_generation++
```

不要：

```text
full success ClearPending(profile) 全删
```

不要：

```text
fast success 只按 path 清 pending
```

不要：

```text
directory path 放 --files-from
```

不要：

```text
directory Updated 一律 full reconcile
```

不要：

```text
每个 destructive event 起 sleep goroutine
```

不要：

```text
filesystem event 清 terminal error
```

不要：

```text
只写新 plist 不 reload loaded launchd service
```

不要：

```text
只 reload profile jobs 忘记 global worker
```

不要：

```text
单进程内存 scheduler 冒充 cross-process scheduler
```

不要：

```text
用更多 time.Sleep 修 watcher flaky test
```

---

# 108. 最终验收场景

## 大型首次同步

运行：

```text
>10m
>30m
甚至数小时
```

knowledge-sync 不因 wall clock kill。

---

## 超慢传输

例如长时间只上传很少数据：

```text
不是 failure
```

只要：

```text
checks
listed
files
bytes
current item
```

还在推进，就正常显示。

---

## 真正无 measurable progress 30m

```text
possible stall
```

但 rclone 继续。

---

## temporary rclone failure

```text
durable retry
```

---

## unknown rclone failure

```text
1m
5m
15m
```

三次自动 retry。

再失败：

```text
terminal
```

手动：

```bash
knowledge-sync sync <profile>
```

可重新开始。

---

## 大目录 move / rename

不会生成：

```text
几千个 directory --files-from entries
```

而是：

```text
一次 durable full debt
10s settle
max 60s
```

---

## 38k backlog

自动：

```text
promote full
collapse detailed pending
```

不会长期保存 38k 路径继续逐条 fast。

---

## full run 期间继续修改

不会启动 competing fast writer。

run 成功到 target generation 后：

```text
如果 desired > target
下一次直接覆盖最新 generation
```

---

## watcher restart

停机期间：

```text
small create/modify
    recovered fast

delete / large / uncertain
    recovered full
```

---

## launchd upgrade

新 install 后真实 loaded job：

```text
reconcile label remains

argv:
    reconcile-scheduled
```

global worker 与 profile watcher 都实际运行新版 binary。

---

# 109. 最终完成报告格式

agent 不要只回复：

```text
done
```

必须报告：

```text
1. 修改文件列表

2. 新 DB migration 版本

3. 新增/修改字段

4. migration rehearsal 结果

5. rclone progress 最终数据源

6. 查询过的 rclone 官方 URL

7. timeout/cancellation 最终语义

8. retry classifier 表

9. unknown retry 如何计数

10. ownership error 如何区分

11. watcher numeric event parser

12. watcher event 分类表

13. 3s/30s debounce 如何实现

14. 10s/60s destructive debounce 如何持久化

15. generation 如何避免虚假 debt

16. pending clear 如何避免 generation race

17. old huge pending 如何 upgrade

18. startup catch-up 如何实现

19. profile lock 覆盖哪些 writer

20. remote scheduler 如何 cross-process

21. launchd command mapping

22. launchd reload 实现

23. 是否确认 global worker 也 reload

24. go vet ./... 结果

25. go test ./... 结果

26. go test -race ./... 结果

27. repeated watcher test 结果

28. 真实 rollout 前 inventory 结果
    只写 job 类型和数量，
    不泄露真实名称

29. 真实 DB migration 是否成功

30. terminal gate 是否正确保留

31. loaded launchd 是否确认使用新版 binary

32. 第一次真实 sync 结果

33. status heartbeat/progress 是否正常

34. 是否执行 rollback

35. 是否有任何偏离本计划的实现
```

如果存在偏离：

```text
原计划要求

实际代码

为什么必须偏离

代码证据

查阅的官方文档
```

不能静默改变设计。