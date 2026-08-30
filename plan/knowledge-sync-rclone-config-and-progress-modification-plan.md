# knowledge-sync：同步进度可观测性与 rclone 参数配置化修改计划

## 1. 目标

本次修改包含两个相互独立、但可以一起落地的功能：

1. **改进 `profile status --watch` 的同步吞吐可观测性**
   - 增加 `Files/min`
   - 增加 `Active transfers`
   - 保留现有 `Speed`、`Transferred files`、`Current` 等字段
   - 避免用户只看单个 `Current` 或瞬时字节速度而误判多并发同步是否卡住

2. **将 rclone 性能/调优参数从代码中解耦**
   - 不再在 `internal/sync/service.go` 中写死 `--transfers`
   - 允许用户通过应用配置文件调整 rclone 参数，无需重新编译
   - 保留 harness 自己必须控制的安全/语义参数
   - 默认配置继续提供已验证的合理值，例如 FullSync `--transfers=12`

本计划不改变 profile 的同步语义、ownership sidecar、generation/debt、retry gate、delete budget 等现有核心模型。

---

## 2. 当前实现与问题

### 2.1 当前进度链路

当前进度链路为：

```text
rclone --use-json-log --stats 10s
        ↓
internal/exec/rclone.go
ProgressStats
        ↓
internal/cli/worker.go
progressSnapshot()
        ↓
internal/state/syncstate.go
UpdateRunStats()
        ↓
sync_runs SQLite
        ↓
internal/cli/profilestatus.go
profile status / --watch
```

现有 `ProgressStats` 已解析：

- bytes / totalBytes
- checks / totalChecks
- transfers / totalTransfers
- listed
- errors
- speed
- `transferring` / `currentTransfers`

但对 `transferring` 数组当前只取第一个元素作为 `CurrentItem`，没有记录数组长度。因此 12 路并发时，CLI 仍只显示一个 `Current`。

### 2.2 当前 `Files/min` 缺失

当前 `sync_runs` 已持久化：

- `files_completed`
- `started_at`
- `phase`
- bytes/checks/listed/speed/current item

但没有“进入 uploading 阶段的时间”。

直接用：

```text
files_completed / (now - run.started_at)
```

会把 scanning/planning/preflight 的时间算进上传吞吐，因此对 FullSync 不准确。

### 2.3 当前 rclone 调优参数分散在代码里

当前 `internal/sync/service.go` 中：

- `FastUpsert` 明确写死 `--transfers 4`
- FullSync 原本依赖 rclone 默认 transfers
- 本轮性能实验临时修改 FullSync 为 `--transfers 12`

这意味着以后想从 12 调到 8、16，或增加 `--checkers`、`--drive-chunk-size` 等参数，都需要改源码并重新编译。

---

# Part A：`Files/min` 与 `Active transfers`

## 3. 输出语义

建议最终 status 输出：

```text
Profile: obsidian-main
State:   syncing
Phase:   uploading
Run:     ...
Local discovered: 37563
Transferred files: 240
Files/min:          78.4
Active transfers:   12
Listed: 53226
Checked: 3206 / 3206
Transferred:        75.4 MiB / 2.7 GiB
Speed:              1.0 MiB/s
Current:            ...
Started: 3m ago
Last progress: 2s ago
Last heartbeat: 2s ago
```

### 3.1 `Active transfers`

定义：

```text
Active transfers = len(rclone stats.transferring)
```

兼容 rclone 的两个字段：

```text
transferring
currentTransfers
```

规则：

- stats 中存在 transfer 数组时，值是“已知”
- 空数组表示 `0`
- 字段不存在表示 unknown
- 不把 ActiveTransfers 的变化视为 `MeasurableProgress`
  - 否则 transfer 数量抖动可能错误刷新 stall timer

### 3.2 `Files/min`

建议定义为：

```text
Files/min = files_completed / uploading_elapsed_minutes
```

其中：

```text
uploading_elapsed = now - upload_started_at
```

不用 `run.started_at`，避免 planning/scanning 时间污染吞吐率。

建议：

- 仅在 `upload_started_at != NULL` 且 `files_completed > 0` 时显示
- 保留 1 位小数
- 这是“本 run 自进入 uploading 后的平均 files/min”
- 暂不实现 10 秒瞬时 rate / EWMA，避免输出过度抖动

未来若需要可以追加：

```text
Files/min (recent)
Files/min (avg)
```

但本次只实现稳定的平均值。

---

## 4. 数据模型修改

### 4.1 SQLite schema migration v7

修改：

```text
internal/state/migrate.go
```

新增 migration：

```sql
ALTER TABLE sync_runs
    ADD COLUMN active_transfers INTEGER NOT NULL DEFAULT 0;

ALTER TABLE sync_runs
    ADD COLUMN upload_started_at TEXT;
```

不需要修改 `profile_sync_state`。

### 4.2 `state.SyncRun`

修改：

```text
internal/state/runs.go
```

新增：

```go
ActiveTransfers int64   `json:"active_transfers"`
UploadStartedAt *string `json:"upload_started_at,omitempty"`
```

同步修改：

- `runCols`
- `scanRun`
- `scanRuns`

旧数据库通过 migration 自动补齐：

```text
active_transfers = 0
upload_started_at = NULL
```

---

## 5. rclone stats parser 修改

修改：

```text
internal/exec/rclone.go
```

### 5.1 `ProgressStats`

增加：

```go
ActiveTransfers      int64
ActiveTransfersKnown bool
```

### 5.2 `parseJSONProgressLine`

当前代码：

```go
if json.Unmarshal(rawTransfers, &transfers) == nil && len(transfers) > 0 {
    item := transfers[0]
    ...
}
```

改成逻辑：

```go
if json.Unmarshal(rawTransfers, &transfers) == nil {
    s.ActiveTransfers = int64(len(transfers))
    s.ActiveTransfersKnown = true

    if len(transfers) > 0 {
        item := transfers[0]
        ...
    }
}
```

`CurrentItem` 仍暂时展示第一个 active transfer，避免一次性把 12 个路径打印到 watch 中造成过多噪音。

### 5.3 `MeasurableProgress`

不要加入：

```go
ActiveTransfers
```

因为：

```text
12 → 11 → 12
```

不能代表真实文件/字节进度。

---

## 6. worker progress persistence 修改

修改：

```text
internal/cli/worker.go
```

`progressSnapshot()` 增加：

```go
ActiveTransfers: s.ActiveTransfers,
```

修改：

```text
internal/state/syncstate.go
```

`ProgressSnapshot` 增加：

```go
ActiveTransfers int64
```

`UpdateRunStats()` SQL 增加：

```sql
active_transfers = ?
```

---

## 7. 上传阶段开始时间

修改：

```text
internal/state/syncstate.go
```

当前：

```go
UpdateRunPhase(profileID, runID, phase)
```

在 phase 第一次切换到：

```text
uploading
```

时写入：

```text
upload_started_at = now
```

但只允许第一次写：

```sql
upload_started_at =
    CASE
        WHEN ? = 'uploading'
        THEN COALESCE(upload_started_at, ?)
        ELSE upload_started_at
    END
```

这样即使 phase callback 重复发送 `uploading`，也不会重置计时。

---

## 8. status renderer 修改

修改：

```text
internal/cli/profilestatus.go
```

在：

```text
Transferred files
```

之后增加：

```go
if run.UploadStartedAt != nil && run.FilesCompleted > 0 {
    ...
    fmt.Printf("Files/min:          %.1f\n", rate)
}
```

再显示：

```go
fmt.Printf("Active transfers:   %d\n", run.ActiveTransfers)
```

建议 Active transfers 仅在：

```text
phase == uploading
```

时显示。

### 输出兼容性

已有字段不删除、不改名：

- `Transferred files`
- `Speed`
- `Current`
- `Last progress`
- `Last heartbeat`

因此不会破坏已有脚本对原字段的依赖。

---

# Part B：rclone 参数配置化

## 9. 配置设计原则

核心要求：

> rclone 的性能调优参数可以修改，但 harness 自己负责正确性和安全性的参数不能被配置文件覆盖。

因此配置系统分成两类：

### 用户可配置

例如：

```text
--transfers
--checkers
--drive-chunk-size
--tpslimit
--tpslimit-burst
--buffer-size
--multi-thread-streams
--low-level-retries
```

### harness-owned / 禁止覆盖

至少包括：

```text
--config
--files-from
--files-from-raw
--filter
--include
--exclude
--delete-excluded
--max-delete
--track-renames
--dry-run
--use-json-log
--stats
--stats-log-level
```

原因：

- 文件选择规则属于 mirror correctness
- delete budget 属于 destructive safety
- progress JSON/stats 属于 worker heartbeat/status correctness
- `--config` 由 launchd-safe rclone wrapper 管理

---

## 10. 配置文件位置

新增：

```text
~/.config/knowledge-sync/config.json
```

修改：

```text
internal/paths/paths.go
```

新增：

```go
ConfigDir()
AppConfigPath()
```

建议遵循：

```text
~/.config/knowledge-sync
```

而数据库继续保持：

```text
~/.local/share/knowledge-sync
```

配置和运行状态分离。

---

## 11. 配置格式

V1 建议直接使用 Go 标准库支持的 JSON，不新增 YAML/TOML dependency。

示例：

```json
{
  "rclone": {
    "global_args": [
      "--checkers=8"
    ],
    "full_sync_args": [
      "--transfers=12"
    ],
    "fast_upsert_args": [
      "--transfers=4"
    ],
    "dry_run_args": [],
    "verify_args": []
  }
}
```

以后想测试：

```text
12 → 8
```

只需要：

```json
"full_sync_args": [
  "--transfers=8"
]
```

重启 worker 后生效，不需要重新编译。

也可以：

```json
"full_sync_args": [
  "--transfers=12",
  "--drive-chunk-size=64M"
]
```

---

## 12. 配置加载模块

新增：

```text
internal/config/config.go
internal/config/config_test.go
```

建议类型：

```go
type Config struct {
    Rclone RcloneConfig `json:"rclone"`
}

type RcloneConfig struct {
    GlobalArgs     []string `json:"global_args"`
    FullSyncArgs   []string `json:"full_sync_args"`
    FastUpsertArgs []string `json:"fast_upsert_args"`
    DryRunArgs     []string `json:"dry_run_args"`
    VerifyArgs     []string `json:"verify_args"`
}
```

暴露：

```go
func Default() Config
func Load(path string) (Config, error)
func Validate(cfg Config) error
func ArgsFor(cfg Config, operation Operation) []string
```

---

## 13. 默认值

默认配置建议：

```json
{
  "rclone": {
    "global_args": [],
    "full_sync_args": [
      "--transfers=12"
    ],
    "fast_upsert_args": [
      "--transfers=4"
    ],
    "dry_run_args": [],
    "verify_args": []
  }
}
```

原因：

- FullSync `12` 已通过真实目录和 FullSync-like benchmark 验证
- FastUpsert 暂时保持当前 `4`
- `checkers` 尚未经过独立 benchmark，不主动修改

这里的目标不是“程序没有任何默认值”，而是：

> `service.go` 不再决定具体调优参数；参数有集中默认值并且可以从配置文件覆盖。

---

## 14. 参数格式与验证

为了简化 merge 和避免：

```text
["--transfers", "12"]
```

这种跨数组元素配对问题，配置参数统一要求单参数形式：

```text
--name=value
```

或无值 boolean flag：

```text
--some-flag
```

不允许：

```json
[
  "sync",
  "/path",
  "remote:path"
]
```

每个 entry 必须以：

```text
--
```

开头。

### 14.1 reserved flag 检查

normalize flag name：

```text
--transfers=12
→ --transfers
```

如果命中 reserved set：

```text
config error:
rclone.full_sync_args contains reserved flag "--max-delete";
this flag is owned by knowledge-sync
```

启动时 fail-fast。

不要静默忽略错误配置。

---

## 15. global 与 operation 参数优先级

优先级：

```text
operation-specific
    >
global
    >
Default()
```

示例：

```json
{
  "rclone": {
    "global_args": [
      "--transfers=8",
      "--checkers=8"
    ],
    "full_sync_args": [
      "--transfers=12"
    ]
  }
}
```

FullSync 最终：

```text
--transfers=12
--checkers=8
```

FastUpsert：

```text
--transfers=8
--checkers=8
```

如果 FastUpsert 自己配置 `--transfers=4`，则它再覆盖 global。

实现时不要简单 append 导致重复 flag；应按 normalized flag name merge。

---

## 16. App 初始化修改

修改：

```text
internal/cli/root.go
```

`NewApp()` 中：

```text
paths.Ensure()
↓
open DB
↓
resolve rclone
↓
load knowledge-sync config
↓
construct sync service
```

`App` 增加：

```go
Config config.Config
```

或至少：

```go
RcloneConfig config.RcloneConfig
```

构造：

```go
svc := sync.New(rclone, db, cfg.Rclone)
```

不要把配置解析逻辑放到 `service.go`。

---

## 17. sync.Service 修改

修改：

```text
internal/sync/service.go
```

当前：

```go
type Service struct {
    Rclone *exec.Rclone
    DB     *state.DB
}
```

改成：

```go
type Service struct {
    Rclone      *exec.Rclone
    DB          *state.DB
    RcloneConfig config.RcloneConfig
}
```

或者封装为：

```go
Options ServiceOptions
```

避免未来构造函数继续膨胀。

---

## 18. 去掉写死参数

### FastUpsert

删除：

```go
"--transfers", "4",
```

改为从：

```text
FastUpsertArgs
```

注入。

### FullSync

删除临时写死：

```go
"--transfers", "12",
```

改为：

```text
FullSyncArgs
```

注入。

### DryRun / Verify

分别使用：

```text
DryRunArgs
VerifyArgs
```

这样以后性能 benchmark 不需要修改 Go 源码。

---

## 19. 参数注入位置

必须保证最终 command 结构：

```text
rclone
[harness global execution flags]
sync
[harness correctness/safety flags]
[user tuning args]
source
destination
```

例如：

```bash
rclone \
  --config ... \
  --use-json-log \
  --stats 10s \
  --stats-log-level NOTICE \
  sync \
  --files-from ... \
  --delete-excluded \
  --fast-list \
  --track-renames \
  --max-delete=100 \
  --transfers=12 \
  --checkers=8 \
  SOURCE \
  REMOTE
```

配置参数不能出现在 source/destination 之后。

---

## 20. 配置文件不存在时

V1 推荐：

```text
文件不存在
→ 使用 Default()
→ 不报错
```

避免升级现有安装后 worker 因缺少配置文件直接失败。

文件存在但 JSON 错误：

```text
→ fail-fast
```

例如：

```text
load knowledge-sync config ~/.config/knowledge-sync/config.json:
invalid character ...
```

文件存在但包含 reserved flag：

```text
→ fail-fast
```

---

## 21. 是否自动创建配置文件

建议本次 **不要在每次启动时自动写文件**。

原因：

- worker 不应悄悄修改用户配置
- 默认值可以由 `Default()` 提供
- 用户需要调整时再创建配置文件

可以在 README / `doctor` 中显示：

```text
App config: ~/.config/knowledge-sync/config.json (not found; using defaults)
Full sync rclone args: --transfers=12
Fast upsert rclone args: --transfers=4
```

如果后续需要，可单独增加：

```bash
knowledge-sync config init
knowledge-sync config show
knowledge-sync config validate
```

不作为本次必需项。

---

# 22. 预计文件变更

## 新增

```text
internal/config/config.go
internal/config/config_test.go
```

## 修改

```text
internal/paths/paths.go

internal/exec/rclone.go
internal/exec/rclone_test.go

internal/state/migrate.go
internal/state/runs.go
internal/state/syncstate.go
internal/state/migrate_test.go

internal/cli/root.go
internal/cli/worker.go
internal/cli/profilestatus.go
internal/cli/async_test.go

internal/sync/service.go
internal/sync/service_test.go   # 若当前没有则新增
```

README/计划文档可选更新。

---

# 23. 测试计划

## 23.1 Active transfers parser

输入：

```json
{
  "stats": {
    "transferring": [
      {"name":"a.md"},
      {"name":"b.md"},
      {"name":"c.pdf"}
    ]
  }
}
```

预期：

```text
ActiveTransfers = 3
ActiveTransfersKnown = true
CurrentItem = a.md
```

空数组：

```text
ActiveTransfers = 0
ActiveTransfersKnown = true
```

字段不存在：

```text
ActiveTransfersKnown = false
```

---

## 23.2 Files/min

构造：

```text
upload_started_at = now - 2m
files_completed = 160
```

预期：

```text
Files/min ≈ 80.0
```

验证：

- scanning 阶段不显示
- uploading 但 0 files 时不显示或显示 `0.0`，二选一后固定测试
- upload_started_at 不会被第二次 PhaseUploading 覆盖

推荐 0 files 时显示：

```text
Files/min: 0.0
```

只要 upload_started_at 已知，这比隐藏字段更一致。

---

## 23.3 migration

从 v6 DB 升到 v7：

验证：

```text
sync_runs.active_transfers
sync_runs.upload_started_at
```

存在且旧数据可正常读取。

---

## 23.4 config default

无配置文件：

预期：

```text
FullSync:
--transfers=12

FastUpsert:
--transfers=4
```

---

## 23.5 config override

配置：

```json
{
  "rclone": {
    "full_sync_args": [
      "--transfers=8"
    ]
  }
}
```

最终 FullSync args：

```text
--transfers=8
```

不能再出现：

```text
--transfers=12
```

---

## 23.6 operation override

配置：

```json
{
  "rclone": {
    "global_args": [
      "--transfers=8",
      "--checkers=6"
    ],
    "full_sync_args": [
      "--transfers=12"
    ]
  }
}
```

最终：

```text
--transfers=12
--checkers=6
```

且 `--transfers` 只出现一次。

---

## 23.7 reserved flag

配置：

```json
{
  "rclone": {
    "full_sync_args": [
      "--max-delete=999999"
    ]
  }
}
```

预期启动失败：

```text
reserved flag "--max-delete"
```

同样验证：

```text
--files-from
--config
--delete-excluded
--use-json-log
--stats
```

不能覆盖。

---

## 23.8 实际 worker 验证

配置：

```json
{
  "rclone": {
    "full_sync_args": [
      "--transfers=12"
    ]
  }
}
```

启动 worker，观察：

```bash
knowledge-sync profile status obsidian-main --watch
```

期望出现：

```text
Files/min:          ...
Active transfers:   12
```

在尾部或 API 等待阶段允许：

```text
Active transfers: 8
Active transfers: 3
Active transfers: 0
```

这是运行状态，不要求始终等于配置的 12。

---

# 24. 实施顺序

建议按以下顺序开发：

1. **配置模块**
   - paths
   - config loader/default/validation
   - merge/precedence
   - tests

2. **Service 注入配置**
   - 移除 FastUpsert `--transfers 4`
   - 移除 FullSync 临时 `--transfers 12`
   - 使用 `ArgsFor()`
   - command construction tests

3. **Active transfers parser**
   - `ProgressStats`
   - parser tests

4. **DB migration v7**
   - `active_transfers`
   - `upload_started_at`

5. **worker persistence**
   - ProgressSnapshot
   - UpdateRunStats
   - uploading phase timestamp

6. **status renderer**
   - Files/min
   - Active transfers

7. **全量测试**
   - `go test ./...`

8. **真实运行验证**
   - 配置 `--transfers=12`
   - worker restart
   - `profile status --watch`
   - 对比 files/min / heartbeat / active transfers

---

# 25. 验收标准

本次修改完成必须满足：

### 配置化

- [ ] `internal/sync/service.go` 不再写死 `--transfers 4/12`
- [ ] 修改 `~/.config/knowledge-sync/config.json` 后无需重新编译
- [ ] 重启 worker 后新参数生效
- [ ] 配置缺失时使用稳定默认值
- [ ] JSON 错误时明确报错
- [ ] reserved safety flag 无法覆盖
- [ ] global 与 operation override 行为有测试覆盖

### 状态输出

- [ ] uploading 时显示 `Files/min`
- [ ] uploading 时显示 `Active transfers`
- [ ] Active transfers 来自 rclone structured stats 的实际 active transfer 数
- [ ] `Current` 保持现有行为，仍展示一个代表性 transfer
- [ ] active transfer 数量变化不会错误刷新 `Last progress`
- [ ] Files/min 不包含 scanning/planning 时间
- [ ] worker restart / old DB upgrade 不会破坏 status

### 回归

- [ ] `go test ./...` 全部通过
- [ ] ownership validation 行为不变
- [ ] delete budget 行为不变
- [ ] retry/generation/debt 行为不变
- [ ] sidecar 行为不变
- [ ] FastUpsert/FullSync 的文件选择语义不变

---

# 26. 推荐最终默认配置

第一版建议默认值只包含已有证据支持的参数：

```json
{
  "rclone": {
    "global_args": [],
    "full_sync_args": [
      "--transfers=12"
    ],
    "fast_upsert_args": [
      "--transfers=4"
    ],
    "dry_run_args": [],
    "verify_args": []
  }
}
```

暂时不要默认增加：

```text
--checkers=16
--drive-chunk-size=64M
```

这些参数可以通过配置文件实验，但在没有真实 workload A/B 结果前不应该成为默认值。

---

# 27. 后续可选增强

不属于本次必须范围：

- `knowledge-sync config init`
- `knowledge-sync config show`
- `knowledge-sync config validate`
- profile-specific rclone override
- `Files/min (recent)` 60 秒滚动窗口
- 同时显示前 N 个 current transfers
- status 增加：
  - configured transfers
  - active/configured，例如 `Active transfers: 8 / 12`
- 将 rclone retry/pacer 摘要暴露到 status
- doctor 检查配置中的 rclone 参数是否被当前 rclone 版本支持
